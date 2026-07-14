// Package logging builds the process slog handler. In JSON mode it emits the
// GCP structured-logging shape (severity/timestamp/message, GCP severity
// values, and Error-Reporting fields on errors) so mcp-telegram logs read
// consistently alongside other services in the same Cloud project; in text
// mode it is the plain human-readable handler used for stdio/local runs.
//
// The MCP SDK accepts only a *slog.Logger, so this is a slog handler rather
// than a port of the org's zerolog helper — the field conventions are matched
// deliberately (see the GCP field constants below), not the implementation.
package logging

import (
	"context"
	"io"
	"log/slog"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// Log format names accepted by NewHandler and the --log-format flag.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// GCP structured-logging field names. Cloud Logging maps these special keys
// into the LogEntry (severity, timestamp) and Error Reporting groups errors
// carrying the reported-error @type. Mirrors the org's pkg/log/fields.go.
const (
	fieldTime           = "timestamp"
	fieldSeverity       = "severity"
	fieldMessage        = "message"
	fieldServiceContext = "serviceContext"
	fieldSourceLocation = "logging.googleapis.com/sourceLocation"
	fieldType           = "@type"

	// reportedErrorType flags a log entry as a ReportedErrorEvent so it shows
	// up in GCP Error Reporting. GCP additionally requires a sourceLocation.
	reportedErrorType = "type.googleapis.com/google.devtools.clouderrorreporting.v1beta1.ReportedErrorEvent"
)

// ParseLevel maps a level name to a slog.Level, defaulting to Info for the
// empty string and reporting an error for anything unrecognised.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		var zero slog.Level
		return zero, &parseError{name}
	}
}

type parseError struct{ name string }

func (e *parseError) Error() string { return "unknown log level: " + strconv.Quote(e.name) }

// NewHandler builds the slog handler for the given format. service/version are
// attached to every JSON record as serviceContext so Error Reporting can group
// by service; they are ignored in text mode. An unknown format falls back to
// text.
func NewHandler(w io.Writer, format string, level slog.Leveler, service, version string) slog.Handler {
	if format == FormatJSON {
		base := slog.NewJSONHandler(w, &slog.HandlerOptions{
			Level:       level,
			ReplaceAttr: gcpReplaceAttr,
		})
		return &gcpHandler{Handler: base, service: service, version: version}
	}
	return slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
}

// gcpReplaceAttr renames the built-in slog keys to the GCP special keys and
// maps slog levels to GCP severity strings.
func gcpReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey:
		a.Key = fieldTime
	case slog.MessageKey:
		a.Key = fieldMessage
	case slog.LevelKey:
		a.Key = fieldSeverity
		if lvl, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(gcpSeverity(lvl))
		}
	}
	return a
}

// gcpSeverity maps a slog level to the closest GCP severity value.
func gcpSeverity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// gcpHandler wraps the JSON handler to attach serviceContext to every record
// and, for error-level records, the Error-Reporting @type plus sourceLocation
// so failures surface in GCP Error Reporting.
type gcpHandler struct {
	slog.Handler
	service string
	version string
}

func (h *gcpHandler) Handle(ctx context.Context, r slog.Record) error {
	r = r.Clone()
	r.AddAttrs(slog.Group(fieldServiceContext,
		slog.String("service", h.service),
		slog.String("version", h.version),
	))
	if r.Level >= slog.LevelError {
		r.AddAttrs(slog.String(fieldType, reportedErrorType))
		if loc := sourceLocation(r.PC); loc.file != "" {
			r.AddAttrs(slog.Group(fieldSourceLocation,
				slog.String("file", loc.file),
				slog.Int("line", loc.line),
				slog.String("function", loc.function),
			))
		}
	}
	//nolint:wrapcheck // pass the underlying handler's error through unchanged.
	return h.Handler.Handle(ctx, r)
}

func (h *gcpHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &gcpHandler{Handler: h.Handler.WithAttrs(attrs), service: h.service, version: h.version}
}

func (h *gcpHandler) WithGroup(name string) slog.Handler {
	return &gcpHandler{Handler: h.Handler.WithGroup(name), service: h.service, version: h.version}
}

type srcLoc struct {
	file     string
	line     int
	function string
}

// sourceLocation resolves the call site recorded on the slog.Record so Error
// Reporting can point at the emitting code. The file is trimmed to its base
// name and the function to its final path segment, matching the org convention.
func sourceLocation(pc uintptr) srcLoc {
	if pc == 0 {
		return srcLoc{}
	}
	fs := runtime.CallersFrames([]uintptr{pc})
	f, _ := fs.Next()
	if f.File == "" {
		return srcLoc{}
	}
	fn := f.Function
	if i := strings.LastIndex(fn, "."); i >= 0 {
		fn = fn[i+1:]
	}
	return srcLoc{file: path.Base(f.File), line: f.Line, function: fn}
}
