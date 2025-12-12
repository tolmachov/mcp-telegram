package tgclient

import (
	"fmt"

	"github.com/gotd/td/tg"
)

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
