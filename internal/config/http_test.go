package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestResolveHTTPAddr(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		explicit   bool
		env        map[string]string
		want       string
		wantErr    string
	}{
		{name: "local safe default", configured: defaultHTTPAddr, want: defaultHTTPAddr},
		{name: "Cloud Run PORT", configured: defaultHTTPAddr, env: map[string]string{"K_SERVICE": "telegram", "PORT": "9090"}, want: ":9090"},
		{name: "explicit address wins in Cloud Run", configured: ":8443", explicit: true, env: map[string]string{"K_SERVICE": "telegram", "PORT": "9090"}, want: ":8443"},
		{name: "unrelated PORT does not widen bind", configured: defaultHTTPAddr, env: map[string]string{"PORT": "9090"}, want: defaultHTTPAddr},
		{name: "missing Cloud Run PORT", configured: defaultHTTPAddr, env: map[string]string{"K_SERVICE": "telegram"}, wantErr: "PORT is missing"},
		{name: "invalid Cloud Run PORT", configured: defaultHTTPAddr, env: map[string]string{"K_SERVICE": "telegram", "PORT": "nope"}, wantErr: "invalid Cloud Run PORT"},
		{name: "out of range Cloud Run PORT", configured: defaultHTTPAddr, env: map[string]string{"K_SERVICE": "telegram", "PORT": "65536"}, wantErr: "invalid Cloud Run PORT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveHTTPAddr(tt.configured, tt.explicit, envLookup(tt.env))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
