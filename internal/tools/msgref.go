package tools

import (
	"fmt"
	"strconv"
	"strings"
)

// MessageRef is an opaque reference to a Telegram message, distinguishing
// regular history messages from scheduled (pending) messages. It's the
// internal representation of the opaque string handles exposed to MCP
// clients at the tool boundary.
//
// Two message spaces exist in Telegram:
//   - Regular: messages that live in a chat's history (messages.getHistory,
//     messages.deleteMessages, messages.editMessage without schedule_date).
//   - Scheduled: messages queued on Telegram's servers for future delivery
//     (messages.getScheduledHistory, messages.deleteScheduledMessages,
//     messages.editMessage with schedule_date).
//
// Regular and scheduled IDs are independent namespaces — ID 42 in one does
// not refer to the same message as ID 42 in the other. The handle form
// ("42" vs "s:42") carries this distinction across the MCP boundary so
// tools like DeleteMessage can route to the right API without a round-trip
// probe.
type MessageRef struct {
	ID        int
	Scheduled bool
}

// scheduledPrefix is the literal prefix marking a handle as referring to
// a scheduled message. Keep it short — handles are visible to the LLM in
// tool outputs and descriptions.
const scheduledPrefix = "s:"

// ParseMessageRef parses an opaque string handle into a MessageRef.
//
// Accepted forms:
//   - "42"   → MessageRef{ID: 42, Scheduled: false}  (regular message)
//   - "s:42" → MessageRef{ID: 42, Scheduled: true}   (scheduled message)
//
// Rejected (returns error):
//   - "" (empty)
//   - "0", "-1", or any non-positive ID
//   - "abc", "s:abc" (non-numeric)
//   - "s:" (empty ID after prefix)
//   - "S:42" (uppercase prefix; only lowercase "s:" is accepted)
//   - strings with leading/trailing whitespace
//   - leading zeros ("042")
//
// The strictness is deliberate: LLMs treat handles as opaque and copy them
// back verbatim from tool outputs. We control the output format, so we
// can also enforce a tight input format and catch accidental mangling early.
func ParseMessageRef(s string) (MessageRef, error) {
	if s == "" {
		return MessageRef{}, fmt.Errorf("message_id is required")
	}
	// Reject anything with whitespace — LLMs shouldn't be constructing handles
	// with incidental spaces, and tolerating them hides bugs.
	if s != strings.TrimSpace(s) {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: contains whitespace", s)
	}

	scheduled := false
	numPart := s
	if after, ok := strings.CutPrefix(s, scheduledPrefix); ok {
		scheduled = true
		numPart = after
		if numPart == "" {
			return MessageRef{}, fmt.Errorf("invalid message_id %q: missing ID after %q prefix", s, scheduledPrefix)
		}
	}

	// After cutting the prefix, the numeric part must not contain whitespace
	// either. "s: 42" would pass the outer whitespace check but smuggle a
	// space into numPart; catch it here before strconv produces a confusing
	// "invalid syntax" error.
	if numPart != strings.TrimSpace(numPart) {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: contains whitespace", s)
	}

	// Reject leading zeros: "042" is not a valid handle even though strconv
	// would happily parse it. This keeps Format(Parse(x)) == x as a round-trip
	// invariant for all accepted inputs.
	if len(numPart) > 1 && numPart[0] == '0' {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: leading zeros not allowed", s)
	}

	id, err := strconv.Atoi(numPart)
	if err != nil {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: %w", s, err)
	}
	if id <= 0 {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: must be a positive integer", s)
	}

	return MessageRef{ID: id, Scheduled: scheduled}, nil
}

// Format renders a MessageRef back to its opaque string handle form.
// Format is the inverse of ParseMessageRef: Format(Parse(x)) == x for
// every accepted input x.
//
// Panics when r.ID <= 0, consistent with FormatRegularRef/FormatScheduledRef.
// This prevents the zero-value MessageRef{} from silently emitting "0", which
// ParseMessageRef would later reject — making the contract symmetric.
func (r MessageRef) Format() string {
	if r.ID <= 0 {
		panic(fmt.Sprintf("MessageRef.Format: ID must be positive, got %d", r.ID))
	}
	if r.Scheduled {
		return scheduledPrefix + strconv.Itoa(r.ID)
	}
	return strconv.Itoa(r.ID)
}

// FormatRegularRef is a convenience constructor for the common case of
// formatting a regular (non-scheduled) message ID as a handle. Use this
// in tool outputs where the message ID is known to come from a regular
// history fetch or send, to avoid constructing a MessageRef by hand.
//
// Panics when id <= 0: callers must verify the ID is positive before
// calling (e.g. guard with "if msgID > 0"). The panic fires at the call
// site rather than serialising a "0" handle that ParseMessageRef would
// later reject, making the invariant violation immediately visible in tests.
func FormatRegularRef(id int) string {
	if id <= 0 {
		panic(fmt.Sprintf("FormatRegularRef: id must be positive, got %d", id))
	}
	return MessageRef{ID: id, Scheduled: false}.Format()
}

// FormatScheduledRef is a convenience constructor for scheduled message
// handles. Use in outputs from scheduled-specific code paths (e.g. when
// GetMessages calls provider.FetchScheduled, or when SendMessage with
// mode=schedule returns the queued message ID).
//
// Panics when id <= 0 for the same reason as FormatRegularRef.
func FormatScheduledRef(id int) string {
	if id <= 0 {
		panic(fmt.Sprintf("FormatScheduledRef: id must be positive, got %d", id))
	}
	return MessageRef{ID: id, Scheduled: true}.Format()
}
