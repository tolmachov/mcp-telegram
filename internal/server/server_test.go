package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestNewRequiresConfig(t *testing.T) {
	_, err := New(Options{Version: "test"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config is required")
}

func TestNewAppliesDefaultStdio(t *testing.T) {
	srv, err := New(Options{
		Config:  &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version: "test",
	})
	require.NoError(t, err)
	assert.NotNil(t, srv.stdin)
	assert.NotNil(t, srv.stdout)
	assert.NotNil(t, srv.errOut)
}
