package tgdata

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// GetCurrentUser retrieves information about the currently authenticated user
func GetCurrentUser(ctx context.Context, client *tg.Client) (*UserInfo, error) {
	user, err := client.UsersGetFullUser(ctx, &tg.InputUserSelf{})
	if err != nil {
		return nil, fmt.Errorf("getting current user: %w", err)
	}

	var info UserInfo
	for _, u := range user.Users {
		if userObj, ok := u.(*tg.User); ok && userObj.Self {
			info = UserInfo{
				ID:        userObj.ID,
				FirstName: userObj.FirstName,
				LastName:  userObj.LastName,
				Username:  userObj.Username,
				Phone:     userObj.Phone,
				Premium:   userObj.Premium,
			}
			break
		}
	}

	info.Bio = user.FullUser.About

	return &info, nil
}
