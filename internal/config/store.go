package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/flags"
)

// ErrNotFound is returned by Store.Get when the requested key is absent.
var ErrNotFound = errors.New("config key not found")

// configKeys maps short CLI aliases to the canonical env-var names under
// which values are stored and later read at startup. The alias is the
// user-facing argument to `mcp-telegram config set/delete`; the canonical
// name is what ends up in the environment for the rest of the code.
var configKeys = map[string]string{
	"api-id":    flags.EnvTelegramAPIID,
	"api-hash":  flags.EnvTelegramAPIHash,
	"anthropic": flags.EnvAnthropicAPIKey,
	"gemini":    flags.EnvGeminiAPIKey,
}

// Store is the persistent config backend. Keys are always canonical env-var
// names; alias translation happens in the CLI layer via ResolveKey.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
	List() ([]string, error)
	LoadAll() (map[string]string, error)
}

// ResolveKey translates a user-facing alias (e.g. "api-id") to its canonical
// env-var name (e.g. "TELEGRAM_API_ID").
func ResolveKey(alias string) (string, error) {
	canonical, ok := configKeys[strings.ToLower(alias)]
	if !ok {
		return "", fmt.Errorf("unknown config key %q; allowed keys: %s", alias, strings.Join(aliasList(), ", "))
	}
	return canonical, nil
}

// AliasFor returns the user-facing alias for a canonical env-var name, or
// the canonical name unchanged if no alias is registered.
func AliasFor(canonical string) string {
	for alias, c := range configKeys {
		if c == canonical {
			return alias
		}
	}
	return canonical
}

func aliasList() []string {
	aliases := make([]string, 0, len(configKeys))
	for alias := range configKeys {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}
