package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/flags"
)

// ErrNotFound is returned when a config key is not found in the store.
var ErrNotFound = errors.New("config key not found")

// allowedKeys is the set of config keys that can be stored securely.
var allowedKeys = map[string]bool{
	flags.EnvTelegramAPIID:   true,
	flags.EnvTelegramAPIHash: true,
	flags.EnvAnthropicAPIKey: true,
	flags.EnvGeminiAPIKey:    true,
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
		return fmt.Errorf("unknown config key %q; allowed keys: %s, %s, %s, %s", key, flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash, flags.EnvAnthropicAPIKey, flags.EnvGeminiAPIKey)
	}
	return nil
}
