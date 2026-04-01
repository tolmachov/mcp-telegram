package config

import (
	"fmt"
	"os"
)

// LoadIntoEnv reads all stored config values and sets them as environment
// variables. Values are only set if the corresponding env var is not already
// set, preserving the priority: CLI flags > env vars / .env > secure store.
func LoadIntoEnv(store Store) error {
	values, err := store.LoadAll()
	if err != nil {
		return fmt.Errorf("loading config from secure store: %w", err)
	}

	for key, value := range values {
		if os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("setting env var %s: %w", key, err)
			}
		}
	}

	return nil
}
