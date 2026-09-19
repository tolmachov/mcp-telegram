package tools

import "github.com/tolmachov/mcp-telegram/internal/presentation"

type MessageRef = presentation.MessageRef
type messageDTO = presentation.Message

var (
	ParseMessageRef    = presentation.ParseMessageRef
	FormatRegularRef   = presentation.FormatRegularRef
	FormatScheduledRef = presentation.FormatScheduledRef
	toMessageDTO       = presentation.FromMessage
)
