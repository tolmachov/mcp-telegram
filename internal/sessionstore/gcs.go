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
// deleted. Revocation tombstones live under a separate revoked/ prefix (see
// Revoke) and are swept the same way.
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
	return sessionPrefix + sessionBase(userID, sid)
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

const (
	sessionPrefix = "sessions/"
	// revokedPrefix namespaces revocation tombstones. The gotd client only ever
	// writes under sessionPrefix, so a tombstone here survives any blob re-store.
	revokedPrefix = "revoked/"
)

func (g *GCS) List(ctx context.Context) ([]SessionRef, error) {
	return g.listPrefix(ctx, sessionPrefix)
}

func (g *GCS) ListRevoked(ctx context.Context) ([]SessionRef, error) {
	return g.listPrefix(ctx, revokedPrefix)
}

// listPrefix enumerates objects under prefix whose base name is a session name
// (sessionBase), skipping any foreign object.
func (g *GCS) listPrefix(ctx context.Context, prefix string) ([]SessionRef, error) {
	var refs []SessionRef
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("sessionstore: listing %s: %w", prefix, err)
		}
		base, found := strings.CutPrefix(attrs.Name, prefix)
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

func (g *GCS) revokedName(userID tgid.UserID, sid string) string {
	return revokedPrefix + sessionBase(userID, sid)
}

func (g *GCS) Revoke(ctx context.Context, userID tgid.UserID, sid string) error {
	// Write the tombstone first (source of truth), then delete the blob. A
	// zero-byte object is enough; its presence is the signal.
	name := g.revokedName(userID, sid)
	w := g.bucket.Object(name).NewWriter(ctx)
	if err := w.Close(); err != nil {
		return fmt.Errorf("sessionstore: writing tombstone %s: %w", name, err)
	}
	if err := g.Delete(ctx, userID, sid); err != nil {
		return err
	}
	return nil
}

func (g *GCS) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	name := g.revokedName(userID, sid)
	_, err := g.bucket.Object(name).Attrs(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrObjectNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: probing tombstone %s: %w", name, err)
	}
}

func (g *GCS) DeleteRevoked(ctx context.Context, userID tgid.UserID, sid string) error {
	name := g.revokedName(userID, sid)
	err := g.bucket.Object(name).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("sessionstore: deleting tombstone %s: %w", name, err)
	}
	return nil
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
