package flags

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllFlagConstructors(t *testing.T) {
	assert.Equal(t, APIID, APIIDFlag().Name)
	assert.Equal(t, APIHash, APIHashFlag().Name)
	assert.Equal(t, AllowedPaths, AllowedPathsFlag().Name)
	assert.Equal(t, Phone, PhoneFlag().Name)
	assert.Equal(t, SummarizeModel, SummarizeModelFlag().Name)
	assert.Equal(t, OllamaURL, OllamaURLFlag().Name)
	assert.Equal(t, GeminiAPIKey, GeminiAPIKeyFlag().Name)
	assert.Equal(t, AnthropicAPIKey, AnthropicAPIKeyFlag().Name)
	assert.Equal(t, SummarizeBatchTokens, SummarizeBatchTokensFlag().Name)
	assert.Equal(t, MediaMaxBytes, MediaMaxBytesFlag().Name)
	assert.Equal(t, TGRateLimitRPS, TGRateLimitRPSFlag().Name)
	assert.Equal(t, PinnedRefreshSecs, PinnedRefreshSecsFlag().Name)
	assert.Equal(t, FloodWaitMaxSecs, FloodWaitMaxSecsFlag().Name)
	assert.Equal(t, Transport, TransportFlag().Name)
	assert.Equal(t, HTTPAddr, HTTPAddrFlag().Name)
	assert.Equal(t, AuthIssuerURL, AuthIssuerURLFlag().Name)
	assert.Equal(t, AuthTokenKey, AuthTokenKeyFlag().Name)
	assert.Equal(t, AuthAllowedRedirects, AuthAllowedRedirectsFlag().Name)
	assert.Equal(t, AuthSessionBucket, AuthSessionBucketFlag().Name)
	assert.Equal(t, AuthSessionDir, AuthSessionDirFlag().Name)
	assert.Equal(t, LogFormat, LogFormatFlag().Name)
	assert.Equal(t, LogLevel, LogLevelFlag().Name)
	assert.Equal(t, Variant, VariantFlag().Name)
}

func TestFlagValidationActions(t *testing.T) {
	ctx := context.Background()
	provider := SummarizeProviderFlag()
	require.NotNil(t, provider.Action)
	assert.NoError(t, provider.Action(ctx, nil, "sampling"))
	assert.Error(t, provider.Action(ctx, nil, "unknown"))

	users := AuthAllowedUsersFlag()
	require.NotNil(t, users.Action)
	assert.NoError(t, users.Action(ctx, nil, []string{"123"}))
	assert.Error(t, users.Action(ctx, nil, []string{"*", "123"}))

	hops := AuthTrustedProxyHopsFlag()
	require.NotNil(t, hops.Action)
	assert.NoError(t, hops.Action(ctx, nil, 0))
	assert.NoError(t, hops.Action(ctx, nil, 16))
	assert.Error(t, hops.Action(ctx, nil, -1))
	assert.Error(t, hops.Action(ctx, nil, 17))
}
