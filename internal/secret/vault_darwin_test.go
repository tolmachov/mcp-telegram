//go:build darwin

package secret

import (
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"

	"github.com/keybase/go-keychain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeItemBackend struct {
	mu    sync.Mutex
	items map[string][]byte
}

func (f *fakeItemBackend) Load(account string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.items[account]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (f *fakeItemBackend) Store(account, _ string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[account] = append([]byte(nil), data...)
	return nil
}
func (f *fakeItemBackend) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, account)
	return nil
}
func (f *fakeItemBackend) List(prefix string) (map[string][]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]byte{}
	for key, value := range f.items {
		if strings.HasPrefix(key, prefix) {
			out[key] = append([]byte(nil), value...)
		}
	}
	return out, nil
}

func newTestVault() (*Vault, *fakeItemBackend) {
	b := &fakeItemBackend{items: map[string][]byte{}}
	return &Vault{backend: b}, b
}

func TestVaultUsesIndependentLiveItems(t *testing.T) {
	v, backend := newTestVault()
	require.NoError(t, v.ConfigSet("A", "one"))
	require.NoError(t, v.ConfigSet("B", "two"))
	backend.items[configAccountPrefix+"A"] = []byte("external-update")
	got, err := v.ConfigGet("A")
	require.NoError(t, err)
	assert.Equal(t, "external-update", got)
	all, err := v.ConfigLoadAll()
	require.NoError(t, err)
	assert.True(t, maps.Equal(map[string]string{"A": "external-update", "B": "two"}, all))
}

func TestVaultSessionCopiesAndZeroMeansAbsent(t *testing.T) {
	v, _ := newTestVault()
	input := []byte("session")
	require.NoError(t, v.SessionStore(input))
	input[0] = 'X'
	got, err := v.SessionLoad()
	require.NoError(t, err)
	assert.Equal(t, []byte("session"), got)
	got[0] = 'Y'
	again, err := v.SessionLoad()
	require.NoError(t, err)
	assert.Equal(t, []byte("session"), again)
	require.NoError(t, v.SessionStore(nil))
	_, err = v.SessionLoad()
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestVaultDeleteIsScoped(t *testing.T) {
	v, _ := newTestVault()
	require.NoError(t, v.ConfigSet("A", "one"))
	require.NoError(t, v.ConfigSet("B", "two"))
	require.NoError(t, v.ConfigDelete("A"))
	_, err := v.ConfigGet("A")
	assert.ErrorIs(t, err, ErrNotFound)
	value, err := v.ConfigGet("B")
	require.NoError(t, err)
	assert.Equal(t, "two", value)
}

func TestKeychainItemData(t *testing.T) {
	denied := errors.New("keychain access denied")
	for _, tc := range []struct {
		name    string
		results []keychain.QueryResult
		err     error
		wantErr error
	}{
		{name: "access failure with no results", err: denied, wantErr: denied},
		{name: "access failure with results", results: []keychain.QueryResult{{Data: []byte("secret")}}, err: denied, wantErr: denied},
		{name: "explicit not found", err: keychain.ErrorItemNotFound, wantErr: ErrNotFound},
		{name: "empty successful query", wantErr: ErrNotFound},
		{name: "success", results: []keychain.QueryResult{{Data: []byte("secret")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := keychainItemData("config/test", tc.results, tc.err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, data)
				if errors.Is(tc.wantErr, denied) {
					assert.NotErrorIs(t, err, ErrNotFound)
				}
				return
			}
			require.NoError(t, err)
			require.Equal(t, []byte("secret"), data)
			data[0] = 'X'
			assert.Equal(t, []byte("secret"), tc.results[0].Data)
		})
	}
}
