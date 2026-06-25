//go:build darwin

// Package secret owns the single macOS Keychain item that backs both config
// values and the Telegram session. Keeping everything in one keychain item
// guarantees at most one password prompt per process — each distinct
// generic-password item has its own ACL, and a locally-built (ad-hoc signed)
// binary re-prompts for each one.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"

	"github.com/keybase/go-keychain"
)

const (
	keychainService = "mcp-telegram"
	keychainAccount = "vault"
	keychainLabel   = "MCP Telegram Settings"
)

// ErrNotFound is returned when a requested value is absent from the vault.
// Callers translate this to their domain-specific sentinel (config.ErrNotFound
// or session.ErrNotFound).
var ErrNotFound = errors.New("secret not found")

// blob is the on-disk schema of the unified keychain item.
type blob struct {
	Config  map[string]string `json:"config,omitempty"`
	Session []byte            `json:"session,omitempty"`
}

// Vault serializes all Keychain I/O under mu; the cache is populated once via
// ensureLoaded (single Keychain read per process = single password prompt),
// and subsequent Set/Delete write the merged blob back via SecItemUpdate.
//
// loadFn and writeFn are function values so tests can substitute in-memory
// fakes without needing a real Keychain; Shared() installs the real
// loadBlob/writeBlob implementations below.
type Vault struct {
	mu      sync.Mutex
	loaded  bool
	config  map[string]string
	session []byte

	loadFn  func() (map[string]string, []byte, error)
	writeFn func(cfg map[string]string, sess []byte) error
}

var (
	shared     *Vault
	sharedOnce sync.Once
)

// Shared returns the process-wide vault. Both the config store and the
// Telegram session storage call this so they share one Keychain read and
// one in-memory cache.
func Shared() *Vault {
	sharedOnce.Do(func() {
		shared = &Vault{
			loadFn:  loadBlob,
			writeFn: writeBlob,
		}
	})
	return shared
}

func (v *Vault) ensureLoaded() error {
	if v.loaded {
		return nil
	}
	cfg, sess, err := v.loadFn()
	if err != nil {
		return err
	}
	v.config = cfg
	v.session = sess
	v.loaded = true
	return nil
}

func loadBlob() (map[string]string, []byte, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(keychainAccount)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return map[string]string{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("querying keychain: %w", err)
	}

	if len(results) == 0 {
		return map[string]string{}, nil, nil
	}
	// A zero-length payload means the item exists but its secret is empty —
	// writeBlob never produces that (empty ⇒ DeleteItem), so this is
	// out-of-band corruption. Surface it rather than silently masking it.
	if len(results[0].Data) == 0 {
		return nil, nil, fmt.Errorf("keychain blob %q is empty; delete the keychain item and re-run `mcp-telegram config set`", keychainAccount)
	}

	var b blob
	if err := json.Unmarshal(results[0].Data, &b); err != nil {
		return nil, nil, fmt.Errorf("decoding keychain blob: %w", err)
	}
	if b.Config == nil {
		b.Config = map[string]string{}
	}
	return b.Config, b.Session, nil
}

func writeBlob(cfg map[string]string, sess []byte) error {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(keychainAccount)

	if len(cfg) == 0 && len(sess) == 0 {
		if err := keychain.DeleteItem(query); err != nil && !errors.Is(err, keychain.ErrorItemNotFound) {
			return fmt.Errorf("deleting keychain blob: %w", err)
		}
		return nil
	}

	data, err := json.Marshal(blob{Config: cfg, Session: sess})
	if err != nil {
		return fmt.Errorf("encoding keychain blob: %w", err)
	}

	// Prefer SecItemUpdate so a partial failure can't leave the keychain
	// without any blob. If the item is missing we fall through to AddItem.
	update := keychain.NewItem()
	update.SetData(data)
	switch err := keychain.UpdateItem(query, update); {
	case err == nil:
		return nil
	case !errors.Is(err, keychain.ErrorItemNotFound):
		return fmt.Errorf("updating keychain blob: %w", err)
	}

	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(keychainService)
	item.SetAccount(keychainAccount)
	item.SetLabel(keychainLabel)
	item.SetData(data)
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)

	if err := keychain.AddItem(item); err != nil {
		return fmt.Errorf("storing keychain blob: %w", err)
	}
	return nil
}

func (v *Vault) ConfigGet(key string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return "", err
	}
	val, ok := v.config[key]
	if !ok {
		return "", ErrNotFound
	}
	return val, nil
}

func (v *Vault) ConfigSet(key, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return err
	}
	next := cloneMap(v.config)
	next[key] = value
	if err := v.writeFn(next, v.session); err != nil {
		return err
	}
	v.config = next
	return nil
}

func (v *Vault) ConfigDelete(key string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return err
	}
	if _, ok := v.config[key]; !ok {
		return nil
	}
	next := cloneMap(v.config)
	delete(next, key)
	if err := v.writeFn(next, v.session); err != nil {
		return err
	}
	v.config = next
	return nil
}

func (v *Vault) ConfigList() ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(v.config))
	for k := range v.config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func (v *Vault) ConfigLoadAll() (map[string]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return nil, err
	}
	return cloneMap(v.config), nil
}

func (v *Vault) SessionLoad() ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return nil, err
	}
	if len(v.session) == 0 {
		return nil, ErrNotFound
	}
	out := make([]byte, len(v.session))
	copy(out, v.session)
	return out, nil
}

func (v *Vault) SessionStore(data []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return err
	}
	next := make([]byte, len(data))
	copy(next, data)
	if err := v.writeFn(v.config, next); err != nil {
		return err
	}
	v.session = next
	return nil
}

func (v *Vault) SessionDelete() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.ensureLoaded(); err != nil {
		return err
	}
	if len(v.session) == 0 {
		return nil
	}
	if err := v.writeFn(v.config, nil); err != nil {
		return err
	}
	v.session = nil
	return nil
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}
