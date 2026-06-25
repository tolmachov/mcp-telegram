//go:build !darwin

// Package secret is darwin-only. On other platforms the config store and
// Telegram session use independent file-backed storage, so nothing imports
// this package and it needs no symbols — this file exists only so
// `go build ./...` doesn't complain about a directory with no Go files.
package secret
