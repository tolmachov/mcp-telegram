package config

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore satisfies Store but only implements LoadAll — the other methods
// are unused by LoadIntoEnv and return zero values so the test fails loudly
// if that ever changes.
type fakeStore struct {
	all   map[string]string
	err   error
	calls int
}

func (f *fakeStore) Get(string) (string, error)          { return "", nil }
func (f *fakeStore) Set(string, string) error            { return nil }
func (f *fakeStore) Delete(string) error                 { return nil }
func (f *fakeStore) List() ([]string, error)             { return nil, nil }
func (f *fakeStore) LoadAll() (map[string]string, error) { f.calls++; return f.all, f.err }

func TestLoadIntoEnvSetsUnsetVars(t *testing.T) {
	const key = "TEST_LOADINTOENV_UNSET"
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() { _ = os.Unsetenv(key) })

	store := &fakeStore{all: map[string]string{key: "from-store"}}
	require.NoError(t, LoadIntoEnv(store))
	assert.Equal(t, "from-store", os.Getenv(key))
}

func TestLoadIntoEnvPreservesExistingVars(t *testing.T) {
	// t.Setenv restores the prior value on cleanup, so we don't leak into
	// sibling tests that might run in parallel.
	const key = "TEST_LOADINTOENV_EXISTING"
	t.Setenv(key, "already-set")

	store := &fakeStore{all: map[string]string{key: "from-store"}}
	require.NoError(t, LoadIntoEnv(store))
	assert.Equal(t, "already-set", os.Getenv(key),
		"LoadIntoEnv must not overwrite a value that was already set — "+
			"priority is flag > env/.env > secure store")
}

func TestLoadIntoEnvWrapsStoreError(t *testing.T) {
	sentinel := errors.New("store boom")
	store := &fakeStore{err: sentinel}

	err := LoadIntoEnv(store)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "LoadIntoEnv must wrap, not swallow, the store error")
	assert.Contains(t, err.Error(), "loading config from secure store")
}
