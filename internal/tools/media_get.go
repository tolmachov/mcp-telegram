package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"
)

// MediaGetHandler handles the GetMedia tool
type MediaGetHandler struct {
	client *tg.Client
}

// NewMediaGetHandler creates a new MediaGetHandler
func NewMediaGetHandler(client *tg.Client) *MediaGetHandler {
	return &MediaGetHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MediaGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetMedia",
		mcp.WithDescription("Get media (photo) from Telegram using a resource URI from message media."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("uri",
			mcp.Required(),
			mcp.Description("The media resource URI (e.g., telegram://media/...)"),
		),
	)
}

// mediaURIPattern matches: telegram://media/{id}/{access_hash}/{dc_id}/{thumb}?ref={base64}
var mediaURIPattern = regexp.MustCompile(`^telegram://media/(\d+)/(-?\d+)/(\d+)/([a-zA-Z]+)\?ref=(.+)$`)

// Handle processes the GetMedia tool request
func (h *MediaGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uri := mcp.ParseString(request, "uri", "")
	if uri == "" {
		return mcp.NewToolResultError("uri parameter is required"), nil
	}

	// Parse the URI
	matches := mediaURIPattern.FindStringSubmatch(uri)
	if matches == nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid media URI format: %s", uri)), nil
	}

	mediaID, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid media ID: %v", err)), nil
	}

	accessHash, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid access hash: %v", err)), nil
	}

	// DC ID is included in the URI but not used directly - the client handles DC transfer
	_, err = strconv.Atoi(matches[3])
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid DC ID: %v", err)), nil
	}

	thumbSize := matches[4]

	fileRefEncoded, err := url.QueryUnescape(matches[5])
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid file reference encoding: %v", err)), nil
	}

	fileReference, err := base64.URLEncoding.DecodeString(fileRefEncoded)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid file reference: %v", err)), nil
	}

	// Create the input photo file location
	location := &tg.InputPhotoFileLocation{
		ID:            mediaID,
		AccessHash:    accessHash,
		FileReference: fileReference,
		ThumbSize:     thumbSize,
	}

	// Download the photo
	dl := downloader.NewDownloader()
	var buf bytes.Buffer

	_, err = dl.Download(h.client, location).Stream(ctx, &buf)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to download photo: %v", err)), nil
	}

	// Return as image
	imageData := base64.StdEncoding.EncodeToString(buf.Bytes())
	return mcp.NewToolResultImage("Photo downloaded successfully", imageData, "image/jpeg"), nil
}
