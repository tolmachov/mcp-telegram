package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound is returned when a config key is not found in the store.
var ErrNotFound = errors.New("config key not found")

// Config key constants for environment variable names.
const (
	EnvTelegramAPIID   = "TELEGRAM_API_ID"
	EnvTelegramAPIHash = "TELEGRAM_API_HASH"
	EnvAnthropicAPIKey = "ANTHROPIC_API_KEY" //nolint:gosec // env var name, not a credential
	EnvGeminiAPIKey    = "GEMINI_API_KEY"    //nolint:gosec // env var name, not a credential
)

// allowedKeys is the set of config keys that can be stored securely.
var allowedKeys = map[string]bool{
	EnvTelegramAPIID:   true,
	EnvTelegramAPIHash: true,
	EnvAnthropicAPIKey: true,
	EnvGeminiAPIKey:    true,
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
		return fmt.Errorf("unknown config key %q; allowed keys: %s, %s, %s, %s", key, EnvTelegramAPIID, EnvTelegramAPIHash, EnvAnthropicAPIKey, EnvGeminiAPIKey)
	}
	return nil
}
