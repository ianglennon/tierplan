// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package vault holds Bayplan's master key and derives everything from it.
package vault

import (
	"errors"
	"log/slog"
)

// Errors returned by the vault.
var (
	ErrNoKey      = errors.New("vault: no encryption key")
	ErrUnknownKey = errors.New("vault: unknown key id")
	ErrDecrypt    = errors.New("vault: decryption failed")
)

var errNotImplemented = errors.New("vault: not implemented")

// Vault is the loaded master key.
type Vault struct{}

// LoadOrCreate is a placeholder.
func LoadOrCreate(configKey, dataDir string, log *slog.Logger) (*Vault, error) {
	return nil, errNotImplemented
}

// Load is a placeholder.
func Load(configKey, dataDir string) (*Vault, error) { return nil, errNotImplemented }

// KeyID is a placeholder.
func (v *Vault) KeyID() string { return "" }

// KeySource is a placeholder.
func (v *Vault) KeySource() string { return "" }

// Subkey is a placeholder.
func (v *Vault) Subkey(label string) []byte { return nil }

// SubkeyFor is a placeholder.
func (v *Vault) SubkeyFor(keyID, label string) ([]byte, error) { return nil, errNotImplemented }

// Seal is a placeholder.
func (v *Vault) Seal(context string, plaintext []byte) (string, error) {
	return "", errNotImplemented
}

// Open is a placeholder.
func (v *Vault) Open(context, sealed string) ([]byte, error) { return nil, errNotImplemented }
