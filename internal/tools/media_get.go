package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// errMediaTooLarge is returned by limitedWriter when the underlying download
// exceeds the configured cap. Use errors.Is(err, errMediaTooLarge) to detect.
var errMediaTooLarge = errors.New("media exceeds configured size limit")

// limitedWriter wraps an io.Writer and aborts the download once total bytes
// exceed limit. The downloader streams chunks via io.Writer, so returning a
// non-nil error from Write propagates up and stops further requests.
type limitedWriter struct {
	w     io.Writer
	limit int64
	wrote int64
}

func newLimitedWriter(w io.Writer, limit int64) *limitedWriter {
	return &limitedWriter{w: w, limit: limit}
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.limit > 0 && l.wrote+int64(len(p)) > l.limit {
		// Write what fits, then fail. Returning short + error is the
		// io.Writer convention for "wrote some, then aborted".
		room := l.limit - l.wrote
		if room < 0 {
			room = 0
		}
		if room > 0 {
			n, _ := l.w.Write(p[:room])
			l.wrote += int64(n)
		}
		return int(room), errMediaTooLarge
	}
	n, err := l.w.Write(p)
	l.wrote += int64(n)
	return n, err
}

// progressWriter wraps an io.Writer and reports the running byte count via the
// supplied callback at most once per minInterval. Used to surface download
// progress to MCP clients when the total size is unknown up front.
type progressWriter struct {
	w           io.Writer
	written     int64
	lastReport  atomic.Int64 // unix nanos of last report
	minInterval time.Duration
	report      func(written int64)
}

func newProgressWriter(w io.Writer, minInterval time.Duration, report func(written int64)) *progressWriter {
	return &progressWriter{w: w, minInterval: minInterval, report: report}
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.w.Write(p)
	pw.written += int64(n)
	now := time.Now().UnixNano()
	last := pw.lastReport.Load()
	if last == 0 || time.Duration(now-last) >= pw.minInterval {
		if pw.lastReport.CompareAndSwap(last, now) && pw.report != nil {
			pw.report(pw.written)
		}
	}
	return n, err
}

// MediaGetHandler handles the GetMedia tool.
type MediaGetHandler struct {
	client   *tg.Client
	maxBytes int
}

// NewMediaGetHandler creates a new MediaGetHandler with a hard size cap on
// downloads. maxBytes <= 0 disables the cap (not recommended).
func NewMediaGetHandler(client *tg.Client, maxBytes int) *MediaGetHandler {
	return &MediaGetHandler{client: client, maxBytes: maxBytes}
}

// GetMediaInput is the input for the GetMedia tool.
type GetMediaInput struct {
	URI string `json:"uri" jsonschema:"The media resource URI (e.g.\\, telegram://media/...)"`
}

// Register adds the tool to the MCP server.
func (h *MediaGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetMedia",
		Description: "Download a photo from Telegram using a media resource URI (telegram://media/...) returned by GetMessages. Returns the image as base64-encoded data.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

// mediaURIPattern matches: telegram://media/{id}/{access_hash}/{dc_id}/{thumb}?ref={base64}
var mediaURIPattern = regexp.MustCompile(`^telegram://media/(\d+)/(-?\d+)/(\d+)/([a-zA-Z]+)\?ref=(.+)$`)

func (h *MediaGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetMediaInput) (*mcp.CallToolResult, any, error) {
	if in.URI == "" {
		return errResult("uri parameter is required"), nil, nil
	}

	matches := mediaURIPattern.FindStringSubmatch(in.URI)
	if matches == nil {
		return errResult(fmt.Sprintf("invalid media URI format: %s", in.URI)), nil, nil
	}

	mediaID, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return errResult(fmt.Sprintf("invalid media ID: %v", err)), nil, nil
	}

	accessHash, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return errResult(fmt.Sprintf("invalid access hash: %v", err)), nil, nil
	}

	// DC ID is included in the URI but not used directly — the client handles DC transfer.
	if _, err := strconv.Atoi(matches[3]); err != nil {
		return errResult(fmt.Sprintf("invalid DC ID: %v", err)), nil, nil
	}

	thumbSize := matches[4]

	fileRefEncoded, err := url.QueryUnescape(matches[5])
	if err != nil {
		return errResult(fmt.Sprintf("invalid file reference encoding: %v", err)), nil, nil
	}
	fileReference, err := base64.URLEncoding.DecodeString(fileRefEncoded)
	if err != nil {
		return errResult(fmt.Sprintf("invalid file reference: %v", err)), nil, nil
	}

	location := &tg.InputPhotoFileLocation{
		ID:            mediaID,
		AccessHash:    accessHash,
		FileReference: fileReference,
		ThumbSize:     thumbSize,
	}

	// Download the photo, surfacing byte-level progress for clients that requested it.
	// Total size is unknown until the download completes, so report total=0 (indeterminate).
	dl := downloader.NewDownloader()
	var buf bytes.Buffer
	token := req.Params.GetProgressToken()
	session := req.Session

	mcpLog(ctx, session, logLevelInfo, "GetMedia", map[string]any{
		"media_id":   mediaID,
		"thumb_size": thumbSize,
	})

	sendProgressWithToken(ctx, session, token, 0, 0, "Starting media download")
	// Cap → progress → buffer. The cap aborts the download early if the file
	// is larger than configured; without it a multi-GB attachment would OOM
	// the process before base64-encoding.
	limited := newLimitedWriter(&buf, int64(h.maxBytes))
	pw := newProgressWriter(limited, 500*time.Millisecond, func(written int64) {
		sendProgressWithToken(ctx, session, token, float64(written), 0, fmt.Sprintf("Downloaded %d bytes", written))
	})

	if _, err := dl.Download(h.client, location).Stream(ctx, pw); err != nil {
		if errors.Is(err, errMediaTooLarge) {
			mcpLog(ctx, session, logLevelWarning, "GetMedia", map[string]any{
				"media_id":   mediaID,
				"max_bytes":  h.maxBytes,
				"downloaded": buf.Len(),
				"reason":     "size cap exceeded",
			})
			return errResult(fmt.Sprintf(
				"Media exceeds the configured size limit of %d bytes (downloaded at least %d bytes before aborting). Use a smaller thumb_size, or raise --media-max-bytes / TELEGRAM_MEDIA_MAX_BYTES if you really need the full file.",
				h.maxBytes, buf.Len(),
			)), nil, nil
		}
		mcpLog(ctx, session, logLevelError, "GetMedia", map[string]any{
			"media_id": mediaID,
			"error":    err.Error(),
		})
		return errResult(fmt.Sprintf("failed to download photo: %v", err)), nil, nil
	}
	sendProgressWithToken(ctx, session, token, float64(buf.Len()), float64(buf.Len()), fmt.Sprintf("Downloaded %d bytes", buf.Len()))

	// Return as image content. The SDK marshals []byte to base64 in JSON, so
	// pass raw bytes here — DO NOT base64-encode upfront.
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: "Photo downloaded successfully"},
			&mcp.ImageContent{Data: buf.Bytes(), MIMEType: "image/jpeg"},
		},
	}, nil, nil
}
