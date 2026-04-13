package tgclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/gotd/td/tg"
)

func TestUserName(t *testing.T) {
	tests := []struct {
		name string
		user *tg.User
		want string
	}{
		{
			name: "username preferred",
			user: &tg.User{ID: 1, Username: "durov", FirstName: "Pavel"},
			want: "@durov",
		},
		{
			name: "first and last name fallback",
			user: &tg.User{ID: 2, FirstName: "Alice", LastName: "Smith"},
			want: "Alice Smith",
		},
		{
			name: "first name only fallback",
			user: &tg.User{ID: 3, FirstName: "Bob"},
			want: "Bob",
		},
		{
			name: "id fallback",
			user: &tg.User{ID: 4},
			want: "User#4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, UserName(tt.user))
		})
	}
}
