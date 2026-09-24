package tgclient

import "math/rand/v2"

// RandomID returns a random non-zero int64 for an ID that must not collide
// with others: Telegram's random_id deduplication field, or the ID of a chat
// listing that a cursor from before a restart must not match.
func RandomID() int64 {
	// The ID only has to be unlikely to repeat, not unpredictable: the
	// runtime-seeded ChaCha8 generator behind math/rand/v2 is enough.
	return rand.Int64() | 1 //nolint:gosec // G404: collision resistance, not secrecy, is needed.
}
