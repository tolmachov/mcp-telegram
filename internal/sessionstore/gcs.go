package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// GCS stores one object per user in a Cloud Storage bucket:
// sessions/<userID>.bin. This is the production backend on Cloud Run, where
// instances have no persistent disk. Objects hold ciphertext (wrap with
// Encrypted); bucket IAM is the outer protection layer.
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

func objectName(userID tgid.UserID) string {
	return "sessions/" + userID.String() + ".bin"
}

func (g *GCS) Session(userID tgid.UserID) session.Storage {
	return gcsSession{object: g.bucket.Object(objectName(userID))}
}

func (g *GCS) Exists(ctx context.Context, userID tgid.UserID) (bool, error) {
	_, err := g.bucket.Object(objectName(userID)).Attrs(ctx)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, storage.ErrObjectNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: probing %s: %w", objectName(userID), err)
	}
}

func (g *GCS) Delete(ctx context.Context, userID tgid.UserID) error {
	err := g.bucket.Object(objectName(userID)).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("sessionstore: deleting %s: %w", objectName(userID), err)
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
