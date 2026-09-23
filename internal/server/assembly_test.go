package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func disconnectedTelegramClient() *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{})
}

func TestBuildAssemblyForVariantModesWithoutTelegramConnection(t *testing.T) {
	for _, variant := range []string{"", variantFull, variantCompact, VariantResearch} {
		t.Run(variant, func(t *testing.T) {
			srv, err := New(Options{
				Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
				Version:   "test",
				Variant:   variant,
				Stdin:     strings.NewReader(""),
				Stdout:    io.Discard,
				ErrOut:    io.Discard,
				Transport: TransportStdio,
			})
			require.NoError(t, err)
			assembly, err := srv.buildAssembly(disconnectedTelegramClient())
			require.NoError(t, err)
			require.NotNil(t, assembly.msgProvider)
			assert.NotEmpty(t, assembly.pinnedServers)
			if variant == "" {
				require.NotNil(t, assembly.variants)
				require.NoError(t, assembly.variants.Close())
			} else {
				require.NotNil(t, assembly.single)
			}
		})
	}
}

func TestRunHappyPinnedVariantExitsOnStdinEOF(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version:   "test",
		Variant:   VariantResearch,
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.runHappy(t.Context(), disconnectedTelegramClient()))
}

func TestRunHappyAllVariantsExitsOnStdinEOF(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version:   "test",
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.runHappy(t.Context(), disconnectedTelegramClient()))
}

func TestStreamableHTTPOptionsCarrySessionTimeout(t *testing.T) {
	opts := streamableHTTPOptions()
	require.NotNil(t, opts)
	assert.Equal(t, mcpSessionTimeout, opts.SessionTimeout)
}

func TestServerAuxiliaryLifecycleBranches(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash", FloodWaitMaxWait: 2 * time.Second},
		Version:   "test",
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	logFloodWait := srv.floodWaitLogger()
	logFloodWait(t.Context(), time.Second)
	logFloodWait(t.Context(), 3*time.Second)

	_, err = (&Server{opts: Options{Config: &tgclient.Config{}}}).startLogin(t.Context())
	require.Error(t, err)

	srv.opts.Variant = "unknown"
	_, err = srv.buildAssembly(disconnectedTelegramClient())
	require.ErrorContains(t, err, "variant")

	first := errors.New("first")
	second := errors.New("second")
	closed := 0
	err = (multiCloser{
		closerFunc(func() error { closed++; return first }),
		closerFunc(func() error { closed++; return second }),
	}).Close()
	assert.Equal(t, 2, closed)
	assert.ErrorIs(t, err, first)
	assert.ErrorIs(t, err, second)
}

func TestLoginRequiredSmallHelpers(t *testing.T) {
	assert.Empty(t, accountSuffix(""))
	assert.Equal(t, " as alice", accountSuffix("alice"))
	assert.ErrorContains(t, cause(t.Context()), "returned before")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, cause(ctx), context.Canceled)
	blocked := &Server{opts: Options{Transport: TransportHTTP}}
	assert.ErrorContains(t, blocked.startBlocked(t.Context(), "blocked reason"), "blocked reason")
}

func TestSafeValueCoversScalarAndTruncation(t *testing.T) {
	assert.Equal(t, "short", safeValue(json.RawMessage(`"short"`)))
	assert.Equal(t, "42", safeValue(json.RawMessage(`42`)))
	long := strings.Repeat("x", maxParamValueLen+1)
	assert.Contains(t, safeValue(json.RawMessage(`"`+long+`"`)), "truncated")
}
