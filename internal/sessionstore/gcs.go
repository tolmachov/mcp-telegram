package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
// deleted. Revocation tombstones live under a separate revoked-v3/ prefix (see
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

func (g *GCS) Session(userID tgid.UserID, sid string) session.Storage {
	return gcsSession{object: g.bucket.Object(objectName(userID, sid))}
}

func (g *GCS) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
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

// writerChunkSize is the upload buffer of every writer: the library minimum
// (256 KiB), which holds any object this package writes in one request.
const writerChunkSize = 256 << 10

// newWriter opens an upload with a small buffer instead of the default 16 MiB
// one. The buffer is what lets the library resend a request after a transient
// error; under the default policy it does so only for writes with a
// precondition — the conditional grant writes — and leaves unconditioned
// session and tombstone writes to fail to the caller. A grant retry that
// follows a lost success gets 412, which StoreGrant reports as
// ErrGrantConflict and the compare-and-swap loop settles by re-reading.
func newWriter(ctx context.Context, object *storage.ObjectHandle) *storage.Writer {
	w := object.NewWriter(ctx)
	w.ChunkSize = writerChunkSize
	return w
}

func grantObjectName(family string) string { return grantPrefix + family + ".json" }

func isPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed
}

// LoadGrant reads family's grant record; the version is the object's GCS
// generation, which is never zero for an existing object.
func (g *GCS) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	r, err := g.bucket.Object(grantObjectName(family)).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return GrantRecord{}, 0, nil
	}
	if err != nil {
		return GrantRecord{}, 0, fmt.Errorf("sessionstore: opening grant: %w", err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		return GrantRecord{}, 0, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	var grant GrantRecord
	if err := json.Unmarshal(data, &grant); err != nil {
		return GrantRecord{}, 0, fmt.Errorf("sessionstore: parsing grant: %w", err)
	}
	return grant, r.Attrs.Generation, nil
}

// StoreGrant writes family's grant record conditioned on the object
// generation (or on its absence for version 0).
func (g *GCS) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	data, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("sessionstore: encoding grant: %w", err)
	}
	cond := storage.Conditions{GenerationMatch: version}
	if version == 0 {
		cond = storage.Conditions{DoesNotExist: true}
	}
	w := newWriter(ctx, g.bucket.Object(grantObjectName(family)).If(cond))
	_, err = w.Write(data)
	if closeErr := w.Close(); err == nil {
		err = closeErr
	}
	if isPreconditionFailed(err) {
		return ErrGrantConflict
	}
	if err != nil {
		return fmt.Errorf("sessionstore: writing grant: %w", err)
	}
	return nil
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
		grant, generation, err := g.LoadGrant(ctx, family)
		if err != nil {
			return err
		}
		if generation != 0 && grant.Expired(now) {
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
