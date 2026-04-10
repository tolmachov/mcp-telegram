package tools

import (
	"strings"
	"testing"
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
		if err != nil {
			t.Errorf("ParseMessageRef(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got.ID != tc.wantID {
			t.Errorf("ParseMessageRef(%q).ID = %d, want %d", tc.in, got.ID, tc.wantID)
		}
		if got.Scheduled != tc.wantSched {
			t.Errorf("ParseMessageRef(%q).Scheduled = %v, want %v", tc.in, got.Scheduled, tc.wantSched)
		}
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
		if err == nil {
			t.Errorf("ParseMessageRef(%q) expected error, got nil", tc.in)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErrPart) {
			t.Errorf("ParseMessageRef(%q) error = %q, want substring %q", tc.in, err.Error(), tc.wantErrPart)
		}
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
		if got := tc.ref.Format(); got != tc.want {
			t.Errorf("MessageRef{%d, %v}.Format() = %q, want %q", tc.ref.ID, tc.ref.Scheduled, got, tc.want)
		}
	}
}

func TestMessageRef_RoundTrip(t *testing.T) {
	// Every accepted input must round-trip unchanged through Parse → Format.
	inputs := []string{"1", "42", "999", "1000000", "s:1", "s:42", "s:1000000"}
	for _, in := range inputs {
		ref, err := ParseMessageRef(in)
		if err != nil {
			t.Errorf("ParseMessageRef(%q) unexpected error: %v", in, err)
			continue
		}
		if out := ref.Format(); out != in {
			t.Errorf("round-trip %q: Format after Parse = %q, want %q", in, out, in)
		}
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
