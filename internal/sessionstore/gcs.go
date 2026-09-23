package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gotd/td/session"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// GCS stores one object per authorization in a Cloud Storage bucket:
// sessions-v3/<userID>.<sid>.bin. This is the production backend on Cloud Run, where
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

const (
	sessionPrefix = "sessions-v3/"
	// revokedPrefix namespaces revocation tombstones. The gotd client only ever
	// writes under sessionPrefix, so a tombstone here survives any blob re-store.
	revokedPrefix = "revoked-v3/"
	grantPrefix   = "oauth-v3/grants/"
)

// objectName maps a current-format session to its bucket object.
func objectName(userID tgid.UserID, sid string) string {
	return sessionPrefix + sessionBase(userID, sid)
}

func (g *GCS) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	if !ValidSID(sid) {
		return brokenSession{err: ErrInvalidSID}
	}
	return gcsSession{object: g.bucket.Object(objectName(userID, sid))}
}

func (g *GCS) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, ErrInvalidSID
	}
	name := objectName(userID, sid)
	attrs, err := g.bucket.Object(name).Attrs(ctx)
	switch {
	case err == nil:
		// A 0-byte session object is treated as absent to match LoadSession,
		// which maps an empty read to session.ErrNotFound. Without this a
		// truncated blob would pass the refresh Exists gate yet fail the client
		// build. (Tombstones are deliberately 0-byte and use their own Attrs
		// probe in Revoked, so this only affects session blobs.)
		return attrs.Size > 0, nil
	case errors.Is(err, storage.ErrObjectNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: probing %s: %w", name, err)
	}
}

func (g *GCS) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	name := objectName(userID, sid)
	err := g.bucket.Object(name).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("sessionstore: deleting %s: %w", name, err)
	}
	return nil
}

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
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	// Write the tombstone first (source of truth), then delete the blob. A
	// zero-byte object is enough; its presence is the signal.
	name := g.revokedName(userID, sid)
	w := newWriter(ctx, g.bucket.Object(name))
	if err := w.Close(); err != nil {
		return fmt.Errorf("sessionstore: writing tombstone %s: %w", name, err)
	}
	if err := g.Delete(ctx, userID, sid); err != nil {
		return err
	}
	return nil
}

func (g *GCS) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, ErrInvalidSID
	}
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
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	name := g.revokedName(userID, sid)
	err := g.bucket.Object(name).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("sessionstore: deleting tombstone %s: %w", name, err)
	}
	return nil
}

// newWriter opens a single-request upload. Every object here is at most a few
// KiB, so the default 16 MiB chunk buffer per writer is pure waste. Without
// the buffer the client cannot retry a failed upload itself; callers already
// surface write errors, and the conditional grant writes must not be blindly
// retried anyway (a retry after a lost success reads as a precondition
// failure).
func newWriter(ctx context.Context, object *storage.ObjectHandle) *storage.Writer {
	w := object.NewWriter(ctx)
	w.ChunkSize = 0
	return w
}

func grantObjectName(family string) string { return grantPrefix + family + ".json" }

func isPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed
}

func (g *GCS) RedeemCode(ctx context.Context, family, sid string, expiresAt time.Time) (bool, error) {
	if !ValidSID(family) || !ValidSID(sid) {
		return false, ErrInvalidSID
	}
	data, _ := json.Marshal(grantRecord{SID: sid, ExpiresAt: expiresAt})
	object := g.bucket.Object(grantObjectName(family)).If(storage.Conditions{DoesNotExist: true})
	w := newWriter(ctx, object)
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, err
	}
	if err := w.Close(); err != nil {
		if isPreconditionFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("sessionstore: creating grant: %w", err)
	}
	return true, nil
}

func (g *GCS) loadGrant(ctx context.Context, family string) (grantRecord, int64, error) {
	object := g.bucket.Object(grantObjectName(family))
	r, err := object.NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return grantRecord{}, 0, os.ErrNotExist
	}
	if err != nil {
		return grantRecord{}, 0, fmt.Errorf("sessionstore: opening grant: %w", err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		return grantRecord{}, 0, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	var grant grantRecord
	if err := json.Unmarshal(data, &grant); err != nil {
		return grantRecord{}, 0, fmt.Errorf("sessionstore: parsing grant: %w", err)
	}
	return grant, r.Attrs.Generation, nil
}

func (g *GCS) storeGrantCAS(ctx context.Context, family string, generation int64, grant grantRecord) error {
	data, _ := json.Marshal(grant)
	object := g.bucket.Object(grantObjectName(family)).If(storage.Conditions{GenerationMatch: generation})
	w := newWriter(ctx, object)
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("sessionstore: committing grant CAS: %w", err)
	}
	return nil
}

func (g *GCS) RotateGrant(ctx context.Context, family string, expected int64) (GrantRotation, error) {
	if !ValidSID(family) {
		return GrantMissing, ErrInvalidSID
	}
	for range 4 {
		grant, generation, err := g.loadGrant(ctx, family)
		if errors.Is(err, os.ErrNotExist) || (!grant.ExpiresAt.IsZero() && !time.Now().Before(grant.ExpiresAt)) {
			return GrantMissing, nil
		}
		if err != nil {
			return GrantMissing, err
		}
		result := GrantRotated
		if grant.Revoked || grant.Generation != expected {
			grant.Revoked = true
			result = GrantReplay
		} else {
			grant.Generation++
		}
		if err := g.storeGrantCAS(ctx, family, generation, grant); err != nil {
			if isPreconditionFailed(err) {
				continue
			}
			return GrantMissing, err
		}
		return result, nil
	}
	return GrantMissing, fmt.Errorf("sessionstore: grant CAS contention")
}

func (g *GCS) RevokeGrant(ctx context.Context, family string) error {
	if !ValidSID(family) {
		return ErrInvalidSID
	}
	for range 4 {
		grant, generation, err := g.loadGrant(ctx, family)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sessionstore: listing grants: %w", err)
		}
		grant.Revoked = true
		if err := g.storeGrantCAS(ctx, family, generation, grant); err != nil {
			if isPreconditionFailed(err) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("sessionstore: grant CAS contention")
}

func (g *GCS) SweepAuthState(ctx context.Context, now time.Time) error {
	it := g.bucket.Objects(ctx, &storage.Query{Prefix: grantPrefix})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sessionstore: listing grants: %w", err)
		}
		family := strings.TrimSuffix(strings.TrimPrefix(attrs.Name, grantPrefix), ".json")
		if !ValidSID(family) {
			continue
		}
		grant, generation, err := g.loadGrant(ctx, family)
		if err != nil {
			return err
		}
		if !now.Before(grant.ExpiresAt) {
			err = g.bucket.Object(attrs.Name).If(storage.Conditions{GenerationMatch: generation}).Delete(ctx)
			if err != nil && !isPreconditionFailed(err) && !errors.Is(err, storage.ErrObjectNotExist) {
				return fmt.Errorf("sessionstore: deleting expired grant: %w", err)
			}
		}
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
	// Give the writer its own cancelable context so a failed Write can ABORT the
	// upload rather than commit it. storage.Writer.Close finalizes the object;
	// calling it after a partial write could publish a truncated/0-byte blob.
	// Canceling the context makes Close return without finalizing.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := newWriter(wctx, s.object)
	if _, err := w.Write(data); err != nil {
		cancel()
		_ = w.Close() // aborts the upload; error is expected and irrelevant here
		return fmt.Errorf("sessionstore: writing %s: %w", s.object.ObjectName(), err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("sessionstore: committing %s: %w", s.object.ObjectName(), err)
	}
	return nil
}
