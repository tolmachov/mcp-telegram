package tools

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
)

// newUpdatesWith builds a *tg.Updates containing a single update, which is the
// shape most Telegram API calls return when sending or editing a message.
func newUpdatesWith(update tg.UpdateClass) *tg.Updates {
	return &tg.Updates{Updates: []tg.UpdateClass{update}}
}

func TestExtractSentMessageID(t *testing.T) {
	tests := []struct {
		name    string
		updates tg.UpdatesClass
		wantID  int
		wantDate int
	}{
		{
			name:    "UpdateShortSentMessage",
			updates: &tg.UpdateShortSentMessage{ID: 7, Date: 1000},
			wantID:  7, wantDate: 1000,
		},
		{
			name: "Updates with UpdateNewMessage",
			updates: newUpdatesWith(&tg.UpdateNewMessage{
				Message: &tg.Message{ID: 42, Date: 2000},
			}),
			wantID: 42, wantDate: 2000,
		},
		{
			name: "Updates with UpdateNewChannelMessage",
			updates: newUpdatesWith(&tg.UpdateNewChannelMessage{
				Message: &tg.Message{ID: 99, Date: 3000},
			}),
			wantID: 99, wantDate: 3000,
		},
		{
			name:     "UpdateShortMessage for PM send",
			updates:  &tg.UpdateShortMessage{ID: 77, Date: 9000},
			wantID:   77,
			wantDate: 9000,
		},
		{
			name:    "unrecognised update type returns zero",
			updates: &tg.UpdateShort{Update: &tg.UpdateUserStatus{}},
			wantID:  0, wantDate: 0,
		},
		{
			name:    "Updates with unrelated update returns zero",
			updates: newUpdatesWith(&tg.UpdateUserStatus{}),
			wantID:  0, wantDate: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotDate := extractSentMessageID(tt.updates)
			assert.Equal(t, tt.wantID, gotID)
			assert.Equal(t, tt.wantDate, gotDate)
		})
	}
}

func TestExtractScheduledMessageID(t *testing.T) {
	tests := []struct {
		name     string
		updates  tg.UpdatesClass
		wantID   int
		wantDate int
	}{
		{
			name: "Updates with UpdateNewScheduledMessage",
			updates: newUpdatesWith(&tg.UpdateNewScheduledMessage{
				Message: &tg.Message{ID: 55, Date: 5000},
			}),
			wantID: 55, wantDate: 5000,
		},
		{
			// Telegram delivers sub-~10s schedules immediately as a regular
			// UpdateNewMessage — this helper must return (0, 0) so the caller
			// falls back to extractSentMessageID and gets the regular handle.
			name: "immediate delivery via UpdateNewMessage returns zero",
			updates: newUpdatesWith(&tg.UpdateNewMessage{
				Message: &tg.Message{ID: 42, Date: 2000},
			}),
			wantID: 0, wantDate: 0,
		},
		{
			name:    "unrecognised type returns zero",
			updates: &tg.UpdateShortSentMessage{ID: 7, Date: 1000},
			wantID:  0, wantDate: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotDate := extractScheduledMessageID(tt.updates)
			assert.Equal(t, tt.wantID, gotID)
			assert.Equal(t, tt.wantDate, gotDate)
		})
	}
}

func TestExtractEditedMessageID(t *testing.T) {
	tests := []struct {
		name     string
		updates  tg.UpdatesClass
		wantID   int
		wantDate int
	}{
		{
			name: "Updates with UpdateEditMessage",
			updates: newUpdatesWith(&tg.UpdateEditMessage{
				Message: &tg.Message{ID: 10, Date: 4000},
			}),
			wantID: 10, wantDate: 4000,
		},
		{
			name: "Updates with UpdateEditChannelMessage",
			updates: newUpdatesWith(&tg.UpdateEditChannelMessage{
				Message: &tg.Message{ID: 20, Date: 6000},
			}),
			wantID: 20, wantDate: 6000,
		},
		{
			name:    "unrecognised type returns zero",
			updates: &tg.UpdateShortSentMessage{ID: 7, Date: 1000},
			wantID:  0, wantDate: 0,
		},
		{
			name:    "Updates with unrelated update returns zero",
			updates: newUpdatesWith(&tg.UpdateUserStatus{}),
			wantID:  0, wantDate: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotDate := extractEditedMessageID(tt.updates)
			assert.Equal(t, tt.wantID, gotID)
			assert.Equal(t, tt.wantDate, gotDate)
		})
	}
}
