package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when a config key is not found in the store.
var ErrNotFound = errors.New("config key not found")

// allowedKeys is the set of config keys that can be stored securely.
var allowedKeys = map[string]bool{
	"TELEGRAM_API_ID":   true,
	"TELEGRAM_API_HASH":  true,
	"ANTHROPIC_API_KEY":  true, //nolint:gosec // key name, not a credential
	"GEMINI_API_KEY":     true, //nolint:gosec // key name, not a credential
}

// Store defines the interface for secure config storage.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
	List() ([]string, error)
	LoadAll() (map[string]string, error)
}

// ValidateKey checks if a key is in the allowed set.
func ValidateKey(key string) error {
	if !allowedKeys[strings.ToUpper(key)] {
		return fmt.Errorf("unknown config key %q; allowed keys: TELEGRAM_API_ID, TELEGRAM_API_HASH, ANTHROPIC_API_KEY, GEMINI_API_KEY", key)
	}
	return nil
}
