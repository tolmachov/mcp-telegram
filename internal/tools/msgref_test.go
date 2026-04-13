package tools

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMessageRef_Valid(t *testing.T) {
	cases := []struct {
		in        string
		wantID    int
		wantSched bool
	}{
		{"1", 1, false},
		{"42", 42, false},
		{"999999999", 999999999, false},
		{"s:1", 1, true},
		{"s:42", 42, true},
		{"s:999999999", 999999999, true},
	}
	for _, tc := range cases {
		got, err := ParseMessageRef(tc.in)
		require.NoError(t, err)
		assert.Equal(t, tc.wantID, got.ID)
		assert.Equal(t, tc.wantSched, got.Scheduled)
	}
}

func TestParseMessageRef_Invalid(t *testing.T) {
	cases := []struct {
		in          string
		wantErrPart string // substring the error must contain
	}{
		{"", "required"},
		{"0", "positive"},
		{"-1", "positive"},
		{"abc", "invalid"},
		{"s:", "missing ID"},
		{"s:abc", "invalid"},
		{"s:0", "positive"},
		{"s:-1", "invalid"},
		{"S:42", "invalid"}, // uppercase prefix not accepted
		{"42 ", "whitespace"},
		{" 42", "whitespace"},
		{"s: 42", "whitespace"},
		{"042", "leading zeros"},
		{"s:042", "leading zeros"},
		{"42.0", "invalid"},
		{"1e5", "invalid"},
	}
	for _, tc := range cases {
		_, err := ParseMessageRef(tc.in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.wantErrPart)
	}
}

func TestMessageRef_Format(t *testing.T) {
	cases := []struct {
		ref  MessageRef
		want string
	}{
		{MessageRef{ID: 1, Scheduled: false}, "1"},
		{MessageRef{ID: 42, Scheduled: false}, "42"},
		{MessageRef{ID: 1, Scheduled: true}, "s:1"},
		{MessageRef{ID: 42, Scheduled: true}, "s:42"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tc.ref.Format())
	}
}

func TestMessageRef_RoundTrip(t *testing.T) {
	// Every accepted input must round-trip unchanged through Parse → Format.
	inputs := []string{"1", "42", "999", "1000000", "s:1", "s:42", "s:1000000"}
	for _, in := range inputs {
		ref, err := ParseMessageRef(in)
		require.NoError(t, err)
		assert.Equal(t, in, ref.Format())
	}
}

func TestFormatRegularRef(t *testing.T) {
	if got := FormatRegularRef(42); got != "42" {
		t.Errorf("FormatRegularRef(42) = %q, want %q", got, "42")
	}
	if got := FormatRegularRef(1); got != "1" {
		t.Errorf("FormatRegularRef(1) = %q, want %q", got, "1")
	}
}

func TestFormatScheduledRef(t *testing.T) {
	if got := FormatScheduledRef(42); got != "s:42" {
		t.Errorf("FormatScheduledRef(42) = %q, want %q", got, "s:42")
	}
	if got := FormatScheduledRef(1); got != "s:1" {
		t.Errorf("FormatScheduledRef(1) = %q, want %q", got, "s:1")
	}
}

func TestFormatRegularRefPanicsOnNonPositive(t *testing.T) {
	for _, id := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("id=%d", id), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("FormatRegularRef(%d) expected panic, got none", id)
				}
			}()
			FormatRegularRef(id)
		})
	}
}

func TestFormatScheduledRefPanicsOnNonPositive(t *testing.T) {
	for _, id := range []int{0, -1, -100} {
		t.Run(fmt.Sprintf("id=%d", id), func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("FormatScheduledRef(%d) expected panic, got none", id)
				}
			}()
			FormatScheduledRef(id)
		})
	}
}

func TestMessageRef_Invariants(t *testing.T) {
	// Regular and scheduled handles for the same numeric ID must format to
	// distinct strings — otherwise we can't distinguish spaces at the boundary.
	reg := MessageRef{ID: 42, Scheduled: false}.Format()
	sched := MessageRef{ID: 42, Scheduled: true}.Format()
	if reg == sched {
		t.Errorf("regular and scheduled handles for the same ID must differ: both = %q", reg)
	}
	// Parse must recover the original scheduled-ness.
	regParsed, _ := ParseMessageRef(reg)
	schedParsed, _ := ParseMessageRef(sched)
	if regParsed.Scheduled {
		t.Errorf("regular handle %q parsed as scheduled", reg)
	}
	if !schedParsed.Scheduled {
		t.Errorf("scheduled handle %q parsed as regular", sched)
	}
}
