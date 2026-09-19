package config

import (
	"errors"
	"fmt"
	"strconv"
)

// Values is the typed subset of urfave/cli used by Resolver. CLI already
// applies command-line values before process environment sources.
type Values interface {
	IsSet(string) bool
	String(string) string
	Int(string) int
}

// Resolver implements the single precedence contract for store-backed secrets:
// CLI -> process env -> secure store. urfave/cli marks a flag as set when
// either the command line or its env source supplied it (an explicitly empty
// env value included), so IsSet covers the first two tiers and a store miss
// resolves to the zero value. It never writes the process environment.
type Resolver struct {
	values Values
	store  Store
}

func NewResolver(values Values) (*Resolver, error) {
	if values == nil {
		return nil, fmt.Errorf("config resolver values are required")
	}
	store, err := NewStore()
	if err != nil {
		return nil, fmt.Errorf("initializing secure config store: %w", err)
	}
	return &Resolver{values: values, store: store}, nil
}

func (r *Resolver) String(flagName, storeKey string) (string, error) {
	if r.values.IsSet(flagName) {
		return r.values.String(flagName), nil
	}
	value, err := r.store.Get(storeKey)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading %s from secure config store: %w", storeKey, err)
	}
	return value, nil
}

func (r *Resolver) Int(flagName, storeKey string) (int, error) {
	if r.values.IsSet(flagName) {
		return r.values.Int(flagName), nil
	}
	raw, err := r.store.Get(storeKey)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s from secure config store: %w", storeKey, err)
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid integer in secure config key %s: %w", storeKey, err)
	}
	return value, nil
}
