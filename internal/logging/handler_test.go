package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func newJSONLogger(t *testing.T, buf *bytes.Buffer, level slog.Leveler) *slog.Logger {
	t.Helper()
	return slog.New(NewHandler(buf, FormatJSON, level, "mcp-telegram", "test-1.2.3"))
}

// decodeLast parses the last JSON log line into a map.
func decodeLast(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("log line is not JSON (%v): %q", err, lines[len(lines)-1])
	}
	return m
}

func TestJSONHandlerGCPFields(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(t, &buf, slog.LevelInfo)
	log.Info("hello", "tool", "GetChats")

	m := decodeLast(t, &buf)
	if m["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", m["severity"])
	}
	if m["message"] != "hello" {
		t.Errorf("message = %v, want hello", m["message"])
	}
	if _, ok := m["timestamp"]; !ok {
		t.Error("missing timestamp field")
	}
	if _, ok := m["level"]; ok {
		t.Error("raw slog 'level' key must be renamed to 'severity'")
	}
	if _, ok := m["msg"]; ok {
		t.Error("raw slog 'msg' key must be renamed to 'message'")
	}
	sc, ok := m["serviceContext"].(map[string]any)
	if !ok {
		t.Fatalf("serviceContext missing or wrong type: %v", m["serviceContext"])
	}
	if sc["service"] != "mcp-telegram" || sc["version"] != "test-1.2.3" {
		t.Errorf("serviceContext = %v, want service=mcp-telegram version=test-1.2.3", sc)
	}
	if m["tool"] != "GetChats" {
		t.Errorf("custom attr tool = %v, want GetChats", m["tool"])
	}
}

func TestJSONHandlerSeverityMapping(t *testing.T) {
	cases := map[slog.Level]string{
		slog.LevelDebug: "DEBUG",
		slog.LevelInfo:  "INFO",
		slog.LevelWarn:  "WARNING",
		slog.LevelError: "ERROR",
	}
	for lvl, want := range cases {
		var buf bytes.Buffer
		log := newJSONLogger(t, &buf, slog.LevelDebug)
		log.Log(t.Context(), lvl, "m")
		if got := decodeLast(t, &buf)["severity"]; got != want {
			t.Errorf("level %v → severity %v, want %v", lvl, got, want)
		}
	}
}

func TestJSONHandlerErrorReporting(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(t, &buf, slog.LevelInfo)
	log.Error("boom", "err", "kaboom")

	m := decodeLast(t, &buf)
	if m["@type"] != reportedErrorType {
		t.Errorf("@type = %v, want %v", m["@type"], reportedErrorType)
	}
	loc, ok := m["logging.googleapis.com/sourceLocation"].(map[string]any)
	if !ok {
		t.Fatalf("sourceLocation missing: %v", m)
	}
	if loc["file"] == "" || loc["function"] == "" {
		t.Errorf("sourceLocation incomplete: %v", loc)
	}
	if _, ok := loc["line"]; !ok {
		t.Error("sourceLocation missing line")
	}
}

func TestJSONHandlerNoErrorFieldsBelowError(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONLogger(t, &buf, slog.LevelInfo)
	log.Warn("careful")

	m := decodeLast(t, &buf)
	if _, ok := m["@type"]; ok {
		t.Error("@type must not appear on non-error records")
	}
	if _, ok := m["logging.googleapis.com/sourceLocation"]; ok {
		t.Error("sourceLocation must not appear on non-error records")
	}
}

func TestTextHandlerUnchanged(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, FormatText, slog.LevelInfo, "mcp-telegram", "v"))
	log.Info("hello", "k", "v")
	out := buf.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "msg=hello") {
		t.Errorf("text handler output not in slog text format: %q", out)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = (%v, %v), want (%v, nil)", in, got, err, want)
		}
	}
	if _, err := ParseLevel("nonsense"); err == nil {
		t.Error("ParseLevel accepted an unknown level")
	}
}
