// Package tgid defines the identifier type for Telegram user accounts. It is
// a leaf package so every layer (session storage, auth server, user pool,
// Telegram client plumbing) can share one type without import cycles.
package tgid

import (
	"fmt"
	"strconv"
)

// UserID is a numeric Telegram user (account) identifier, as returned in
// tg.User.ID. It is a distinct type so that user ids cannot be silently mixed
// with chat ids, message ids, or other int64-shaped values.
type UserID int64

// Int64 returns the raw numeric value for APIs that require int64.
func (id UserID) Int64() int64 { return int64(id) }

// String renders the id in decimal (implements fmt.Stringer).
func (id UserID) String() string { return strconv.FormatInt(int64(id), 10) }

// Parse converts a decimal string into a UserID. Telegram user ids are
// positive; zero or negative values are rejected.
func Parse(s string) (UserID, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("tgid: parsing %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("tgid: user id must be positive, got %d", n)
	}
	return UserID(n), nil
}
