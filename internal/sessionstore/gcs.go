package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"github.com/gotd/td/session"
	"google.golang.org/api/iterator"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// GCS stores one object per authorization in a Cloud Storage bucket:
// sessions/<userID>.<sid>.bin (or sessions/<userID>.bin for legacy sessions
// with an empty sid). This is the production backend on Cloud Run, where
// instances have no persistent disk. Objects hold ciphertext (wrap with
// Encrypted); bucket IAM is the outer protection layer.
//
// Because each authorization keeps its own object, a client that drops its
// token without logging out leaves an object behind. The authsrv orphan
// sweeper reclaims those via List: an object older than the refresh-token TTL
// can never be used again (its refresh would be rejected as expired) and is
// deleted.
type GCS struct {
	bucket *storage.BucketHandle
}

// NewGCS builds the store and probes the bucket so a misconfigured
// bucket/IAM fails at startup rather than on the first login.
func NewGCS(ctx context.Context, bucketName string) (*GCS, error) {
	if bucketName == "" {
		return nil, fmt.Errorf("sessionstore: bucket name is required")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: creating GCS client: %w", err)
	}
	bucket := client.Bucket(bucketName)
	if _, err := bucket.Attrs(ctx); err != nil {
		return nil, fmt.Errorf("sessionstore: probing bucket %s: %w", bucketName, err)
	}
	return &GCS{bucket: bucket}, nil
}

// objectName maps a session to its bucket object. An empty sid keeps the
// legacy per-user layout so pre-upgrade sessions remain readable.
func objectName(userID tgid.UserID, sid string) string {
	if sid == "" {
		return sessionPrefix + userID.String() + ".bin"
	}
	return sessionPrefix + userID.String() + "." + sid + ".bin"
}

func (g *GCS) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	return gcsSession{object: g.bucket.Object(objectName(userID, sid))}
}

func (g *GCS) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	name := objectName(userID, sid)
	_, err := g.bucket.Object(name).Attrs(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrObjectNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: probing %s: %w", name, err)
	}
}

func (g *GCS) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	name := objectName(userID, sid)
	err := g.bucket.Object(name).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("sessionstore: deleting %s: %w", name, err)
	}
	return nil
}

const sessionPrefix = "sessions/"

func (g *GCS) List(ctx context.Context) ([]SessionRef, error) {
	var refs []SessionRef
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: sessionPrefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sessionstore: listing sessions: %w", err)
		}
		base, found := strings.CutPrefix(attrs.Name, sessionPrefix)
		if !found {
			continue
		}
		userID, sid, ok := parseSessionBase(base)
		if !ok {
			// A foreign object under the prefix — not ours to touch.
			continue
		}
		refs = append(refs, SessionRef{UserID: userID, SID: sid, UpdatedAt: attrs.Updated})
	}
}

type gcsSession struct {
	object *storage.ObjectHandle
}

func (s gcsSession) LoadSession(ctx context.Context) ([]byte, error) {
	r, err := s.object.NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sessionstore: opening %s: %w", s.object.ObjectName(), err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("sessionstore: reading %s: %w", s.object.ObjectName(), err)
	}
	if len(data) == 0 {
		return nil, session.ErrNotFound
	}
	return data, nil
}

func (s gcsSession) StoreSession(ctx context.Context, data []byte) error {
	w := s.object.NewWriter(ctx)
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("sessionstore: writing %s: %w", s.object.ObjectName(), err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("sessionstore: committing %s: %w", s.object.ObjectName(), err)
	}
	return nil
}
