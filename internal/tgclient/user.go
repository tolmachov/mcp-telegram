package tgclient

import (
	"fmt"

	"github.com/gotd/td/tg"
)

// UserDisplayName returns a human-readable display name for a user.
// Returns "FirstName LastName" or username if name is empty.
func UserDisplayName(user *tg.User) string {
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if name == "" && user.Username != "" {
		return user.Username
	}
	return name
}

// UserName returns a formatted user identifier for display.
// Priority: @username > "FirstName LastName" > "User#ID"
func UserName(user *tg.User) string {
	if user.Username != "" {
		return "@" + user.Username
	}
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if name == "" {
		return fmt.Sprintf("User#%d", user.ID)
	}
	return name
}
