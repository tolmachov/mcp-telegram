//go:build darwin

// Package secret stores each secret in an independent versioned macOS
// Keychain item. The former aggregate blob is intentionally ignored.
package secret

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/keybase/go-keychain"
)

const (
	keychainService     = "mcp-telegram.v2"
	configAccountPrefix = "config/"
	sessionAccount      = "telegram-session"
)

var ErrNotFound = errors.New("secret not found")

type itemBackend interface {
	Load(account string) ([]byte, error)
	Store(account, label string, data []byte) error
	Delete(account string) error
	List(prefix string) (map[string][]byte, error)
}

// Vault serializes same-process operations but performs every read against the
// Keychain. There is deliberately no process cache: another process updating a
// key is immediately visible and independent items cannot suffer lost updates.
type Vault struct {
	mu      sync.Mutex
	backend itemBackend
}

var (
	shared     *Vault
	sharedOnce sync.Once
)

func Shared() *Vault {
	sharedOnce.Do(func() { shared = &Vault{backend: keychainBackend{}} })
	return shared
}

type keychainBackend struct{}

func keychainQuery(account string) keychain.Item {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	if account != "" {
		query.SetAccount(account)
	}
	return query
}

func (keychainBackend) Load(account string) ([]byte, error) {
	query := keychainQuery(account)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)
	results, err := keychain.QueryItem(query)
	return keychainItemData(account, results, err)
}

func keychainItemData(account string, results []keychain.QueryResult, err error) ([]byte, error) {
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("querying keychain item %q: %w", account, err)
	}
	if len(results) == 0 {
		return nil, ErrNotFound
	}
	return append([]byte(nil), results[0].Data...), nil
}

func (keychainBackend) Store(account, label string, data []byte) error {
	query := keychainQuery(account)
	update := keychain.NewItem()
	update.SetData(data)
	update.SetLabel(label)
	update.SetSynchronizable(keychain.SynchronizableNo)
	update.SetAccessible(keychain.AccessibleWhenUnlocked)
	switch err := keychain.UpdateItem(query, update); {
	case err == nil:
		return nil
	case !errors.Is(err, keychain.ErrorItemNotFound):
		return fmt.Errorf("updating keychain item %q: %w", account, err)
	}

	item := keychainQuery(account)
	item.SetLabel(label)
	item.SetData(data)
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)
	if err := keychain.AddItem(item); err != nil {
		return fmt.Errorf("storing keychain item %q: %w", account, err)
	}
	return nil
}

func (keychainBackend) Delete(account string) error {
	err := keychain.DeleteItem(keychainQuery(account))
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting keychain item %q: %w", account, err)
	}
	return nil
}

func (keychainBackend) List(prefix string) (map[string][]byte, error) {
	query := keychainQuery("")
	query.SetMatchLimit(keychain.MatchLimitAll)
	query.SetReturnAttributes(true)
	query.SetReturnData(true)
	results, err := keychain.QueryItem(query)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return map[string][]byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing keychain items: %w", err)
	}
	out := make(map[string][]byte)
	for _, result := range results {
		if strings.HasPrefix(result.Account, prefix) {
			out[result.Account] = append([]byte(nil), result.Data...)
		}
	}
	return out, nil
}

func (v *Vault) ConfigGet(key string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, err := v.backend.Load(configAccountPrefix + key)
	return string(data), err
}

func (v *Vault) ConfigSet(key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.backend.Store(configAccountPrefix+key, "MCP Telegram config: "+key, []byte(value))
}

func (v *Vault) ConfigDelete(key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.backend.Delete(configAccountPrefix + key)
}

func (v *Vault) ConfigList() ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	items, err := v.backend.List(configAccountPrefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(items))
	for account := range items {
		keys = append(keys, strings.TrimPrefix(account, configAccountPrefix))
	}
	sort.Strings(keys)
	return keys, nil
}

func (v *Vault) ConfigLoadAll() (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	items, err := v.backend.List(configAccountPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(items))
	for account, data := range items {
		out[strings.TrimPrefix(account, configAccountPrefix)] = string(data)
	}
	return out, nil
}

func (v *Vault) SessionLoad() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	data, err := v.backend.Load(sessionAccount)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrNotFound
	}
	return append([]byte(nil), data...), nil
}

func (v *Vault) SessionStore(data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(data) == 0 {
		return v.backend.Delete(sessionAccount)
	}
	return v.backend.Store(sessionAccount, "MCP Telegram session", append([]byte(nil), data...))
}

func (v *Vault) SessionDelete() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.backend.Delete(sessionAccount)
}
