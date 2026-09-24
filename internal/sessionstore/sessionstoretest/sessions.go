package sessionstoretest

import "context"

// FailingSession is a session.Storage whose every load and store fails with
// Err, the way an unreachable or refusing backend does.
type FailingSession struct{ Err error }

// LoadSession implements session.Storage.
func (s FailingSession) LoadSession(context.Context) ([]byte, error) { return nil, s.Err }

// StoreSession implements session.Storage.
func (s FailingSession) StoreSession(context.Context, []byte) error { return s.Err }

// BlockingSession is a session.Storage whose LoadSession parks until the
// caller's context ends, so a client started on it never becomes ready.
type BlockingSession struct{}

// LoadSession implements session.Storage.
func (BlockingSession) LoadSession(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// StoreSession implements session.Storage.
func (BlockingSession) StoreSession(context.Context, []byte) error { return nil }
