package config

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeValues struct {
	set     map[string]bool
	strings map[string]string
	ints    map[string]int
}

func (f fakeValues) IsSet(name string) bool    { return f.set[name] }
func (f fakeValues) String(name string) string { return f.strings[name] }
func (f fakeValues) Int(name string) int       { return f.ints[name] }

type resolverStore map[string]string

func (s resolverStore) Get(key string) (string, error) {
	value, ok := s[key]
	if !ok {
		return "", ErrNotFound
	}
	return value, nil
}
func (resolverStore) Set(string, string) error { return nil }
func (resolverStore) Delete(string) error      { return nil }
func (resolverStore) List() ([]string, error)  { return nil, nil }

func TestResolverPrecedenceAndTypes(t *testing.T) {
	values := fakeValues{
		set:     map[string]bool{"cli": true, "empty-env": true, "cli-int": true},
		strings: map[string]string{"cli": "from-cli"},
		ints:    map[string]int{"cli-int": 7},
	}
	r := &Resolver{values: values, store: resolverStore{
		"CLI_KEY": "from-store", "EMPTY_KEY": "from-store", "STORE_KEY": "stored", "INT_KEY": strconv.Itoa(42),
	}}

	got, err := r.String("cli", "CLI_KEY")
	require.NoError(t, err)
	assert.Equal(t, "from-cli", got)
	got, err = r.String("empty-env", "EMPTY_KEY")
	require.NoError(t, err)
	assert.Empty(t, got, "an explicitly empty CLI/env value must not fall through to the store")
	got, err = r.String("unset", "STORE_KEY")
	require.NoError(t, err)
	assert.Equal(t, "stored", got)
	got, err = r.String("unset", "MISSING")
	require.NoError(t, err)
	assert.Empty(t, got)

	integer, err := r.Int("unset", "INT_KEY")
	require.NoError(t, err)
	assert.Equal(t, 42, integer)
	integer, err = r.Int("cli-int", "INT_KEY")
	require.NoError(t, err)
	assert.Equal(t, 7, integer)
	integer, err = r.Int("unset", "MISSING")
	require.NoError(t, err)
	assert.Zero(t, integer)
}
