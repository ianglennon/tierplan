// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package vault holds TierPlan's master key and derives everything from it
// (D-13, D-14).
//
// The master key is 32 random bytes, written as one line of standard base64.
// It comes from TIERPLAN_ENCRYPTION_KEY (or the _FILE variant, which config
// resolves first) or from <data dir>/tierplan.key, which is generated on first
// start. Every ciphertext and every audit HMAC records the ID of the key that
// produced it, so a later rotation needs no format change.
//
// Formats (confirmed at plan 01-07 Task 1, and one-way once data exists):
//
//	key ID      "k1-" + hex(first 8 bytes of SHA-256("tierplan/key-id/v1" || key))
//	subkey      HKDF-SHA256(key, no salt, info = label), 32 bytes
//	envelope    "v1." + keyID + "." + base64url(nonce(12) || GCM ciphertext+tag),
//	            AAD = "tierplan/v1/" + caller context
//
// The key is never logged and is only reachable through derived subkeys.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// KeyFile is the generated key's file name inside the data directory.
	KeyFile = "tierplan.key"

	// Subkey labels. They are part of the on-disk format: changing one
	// invalidates everything derived with it.
	LabelAuditHMAC = "tierplan/audit-hmac/v1"
	LabelVaultAEAD = "tierplan/vault-aead/v1"
	LabelCSRF      = "tierplan/csrf/v1"

	// Sealing contexts that more than one package must agree on. The CA key
	// context is followed by the environment ID, the registry credential one by
	// the credential ID. Like the labels they are part of the on-disk format: a
	// ciphertext only opens under the context it was sealed with.
	ContextAgentCA        = "agent-ca/v1/"
	ContextBackupSchedule = "backup-schedule/v1"
	ContextRegistryCred   = "registry-cred/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// A git credential's token or private key is sealed under the credential's ID (Phase 9), so a
	// value copied into another row does not open.
	ContextGitCred = "git-cred/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// A stack's secret variable is sealed under the snapshot ID and the variable's name, a secret
	// stack file under the snapshot ID and its path (Phase 7), so a value copied to another row,
	// another snapshot or another name does not open.
	ContextStackVar  = "stack-var/v1/"  //nolint:gosec // G101: a sealing context label, not a credential
	ContextStackFile = "stack-file/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// A git stack's webhook secret is sealed under the stack's ID (Phase 9), so a value copied into
	// another stack's row does not open.
	ContextGitWebhook = "git-webhook/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// A TLS private key of the server is sealed under its certificate row's ID (Phase 13.1), so a
	// key copied into another row does not open.
	ContextTLSKey = "tls-key/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// The ACME account key is sealed under the account's directory URL, and the fresh certificate key
	// of an open ACME order under the order's ID (Phase 14), so a key copied into another account or
	// order does not open. A key that has been issued a certificate moves to the tls_certs row "acme".
	ContextACMEAccountKey = "acme-account/v1/"   //nolint:gosec // G101: a sealing context label, not a credential
	ContextACMEOrderKey   = "acme-order-key/v1/" //nolint:gosec // G101: a sealing context label, not a credential
	// The Cloudflare token for DNS-01 is sealed under the DNS provider's name (Phase 15), so a token
	// copied into another provider's row does not open.
	ContextACMEDNSToken = "acme-dns-token/v1/" //nolint:gosec // G101: a sealing context label, not a credential

	keyLen         = 32
	keyIDDomain    = "tierplan/key-id/v1"
	keyIDPrefix    = "k1-"
	envelopeV1     = "v1"
	aadPrefix      = "tierplan/v1/"
	envKeyName     = "TIERPLAN_ENCRYPTION_KEY"
	sourceEnv      = "env"
	sourceFile     = "file"
	keyFileMode    = 0o600
	broadModeMask  = 0o077
	subkeyLenBytes = 32
)

// Errors returned by the vault. Callers match them with errors.Is.
var (
	// ErrNoKey means no key is configured and no key file exists. Load
	// returns it instead of generating one.
	ErrNoKey = errors.New("vault: no encryption key is configured and no key file exists")
	// ErrUnknownKey means a sealed value or HMAC names a key ID this vault does
	// not hold: the wrong key file, or a key that was rotated away.
	ErrUnknownKey = errors.New("vault: the data was produced with a key this vault does not hold")
	// ErrDecrypt means authentication failed: the ciphertext was modified, or
	// it was sealed for a different context. AEAD cannot tell the two apart.
	ErrDecrypt = errors.New("vault: decryption failed")
	// ErrMalformed means the sealed value is not a v1 envelope.
	ErrMalformed = errors.New("vault: malformed sealed value")
)

// Vault is the loaded master key. It is safe for concurrent use.
type Vault struct {
	active string
	source string

	mu      sync.Mutex        // guards keys and subkeys
	keys    map[string][]byte // key ID to master key
	subkeys map[string][]byte // keyID + "\x00" + label
}

// LoadOrCreate returns the vault for configKey (the resolved
// TIERPLAN_ENCRYPTION_KEY, empty when unset). A set configKey always wins and
// creates no file. Otherwise it reads <dataDir>/tierplan.key, generating it
// (0600, written atomically) on first start and warning to back it up.
func LoadOrCreate(configKey, dataDir string, log *slog.Logger) (*Vault, error) {
	return load(configKey, dataDir, log, true)
}

// Load is LoadOrCreate that never generates a key: it returns ErrNoKey
// instead. Command-line tools that only read (for example `audit verify`)
// use it so they can never mint a key that does not match the server's.
func Load(configKey, dataDir string) (*Vault, error) {
	return load(configKey, dataDir, slog.Default(), false)
}

func load(configKey, dataDir string, log *slog.Logger, create bool) (*Vault, error) {
	if log == nil {
		log = slog.Default()
	}
	v, err := loadActive(configKey, dataDir, log, create)
	if err != nil {
		return nil, err
	}
	addOldKey(v, dataDir, log)
	return v, nil
}

// addOldKey makes the key a rotation kept as tierplan.key.old readable, so
// values and audit attestations it produced still open and verify (D-33). The
// file is a convenience, never a requirement: one that is missing, unreadable
// or not a key is logged and skipped, and never stops a start.
func addOldKey(v *Vault, dataDir string, log *slog.Logger) {
	key, err := ReadKeyFile(dataDir, OldKeyFile)
	switch {
	case err == nil:
		_, _ = v.AddKey(key) // cannot fail: ReadKeyFile checked the length
	case errors.Is(err, fs.ErrNotExist):
	default:
		log.Warn("ignoring the previous encryption key file: "+err.Error(), "path", filepath.Join(dataDir, OldKeyFile))
	}
}

func loadActive(configKey, dataDir string, log *slog.Logger, create bool) (*Vault, error) {
	if configKey != "" {
		key, err := decodeKey(configKey)
		if err != nil {
			// The supplied value is never echoed into the error.
			return nil, fmt.Errorf("%s: %w", envKeyName, err)
		}
		return newVault(key, sourceEnv), nil
	}

	path := filepath.Join(dataDir, KeyFile)
	raw, err := os.ReadFile(path) //nolint:gosec // path is <data dir>/tierplan.key
	switch {
	case err == nil:
		key, derr := decodeKey(string(raw))
		if derr != nil {
			return nil, fmt.Errorf("key file %s: %w", path, derr)
		}
		warnIfBroad(path, log)
		return newVault(key, sourceFile), nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	case !create:
		return nil, ErrNoKey
	}

	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	created, err := writeKeyFile(path, key)
	if err != nil {
		return nil, err
	}
	if !created {
		// Another process won the race to create the file; use its key.
		return load("", dataDir, log, false)
	}
	log.Warn("generated encryption key at "+path+": back it up together with the database; "+
		"without it stored secrets cannot be decrypted and the audit log cannot be verified",
		"path", path)
	return newVault(key, sourceFile), nil
}

// decodeKey parses one line of standard base64 that must hold exactly 32
// bytes. The error never contains the input.
func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("must be standard base64 of exactly 32 random bytes (generate one with: openssl rand -base64 32)")
	}
	if len(key) != keyLen {
		return nil, fmt.Errorf("must decode to exactly 32 bytes, got %d (generate one with: openssl rand -base64 32)", len(key))
	}
	return key, nil
}

// linkFile is os.Link; a test replaces it to stand in for a file system
// without hard links.
var linkFile = os.Link

// writeKeyFile writes the key atomically: a 0600 temporary file in the same
// directory, hard-linked into place so an existing file is never replaced. It
// reports false when the file already exists. Where hard links are not
// available it creates the file with O_CREATE|O_EXCL instead, which also never
// replaces an existing file: an existing master key is never overwritten by
// either path, because losing it makes every stored secret unreadable (WR-05).
func writeKeyFile(path string, key []byte) (created bool, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false, fmt.Errorf("create data directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tierplan.key-*")
	if err != nil {
		return false, fmt.Errorf("write key file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(keyFileMode); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write key file: %w", err)
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write key file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("write key file: %w", err)
	}

	if err := linkFile(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return writeKeyFileExclusively(path, key)
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // dir is the operator data directory
		_ = d.Sync()
		_ = d.Close()
	}
	return true, nil
}

// writeKeyFileExclusively is the fallback for a file system without hard
// links. O_EXCL makes the create itself the race: exactly one process creates
// the file, and the others see fs.ErrExist and use the winner's key.
func writeKeyFileExclusively(path string, key []byte) (created bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode) //nolint:gosec // path is the operator data directory
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("create key file %s: %w", path, err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// Remove only the file this call just created, so no half-written key
		// is left for the next start to trip over.
		_ = os.Remove(path)
		return false, fmt.Errorf("write key file %s: %w", path, werr)
	}
	return true, nil
}

func warnIfBroad(path string, log *slog.Logger) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if st.Mode().Perm()&broadModeMask != 0 {
		log.Warn("the encryption key file is readable by other users: run chmod 600 on it",
			"path", path, "mode", fmt.Sprintf("%04o", st.Mode().Perm()))
	}
}

// New returns a vault holding exactly key, which must be 32 bytes. It is for
// code that has a key in hand (a restore, a rotation, tests); the server loads
// its key with LoadOrCreate.
func New(key []byte, source string) (*Vault, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("vault: the key must be exactly %d bytes, got %d", keyLen, len(key))
	}
	return newVault(append([]byte(nil), key...), source), nil
}

// GenerateKey returns 32 fresh random bytes from the system CSPRNG.
func GenerateKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate encryption key: %w", err)
	}
	return key, nil
}

// AddKey makes key readable by this vault without changing the active key, and
// returns its ID. It lets one vault open values and verify audit entries that an
// older key produced. Adding a key the vault already holds is a no-op.
func (v *Vault) AddKey(key []byte) (string, error) {
	if len(key) != keyLen {
		return "", fmt.Errorf("vault: the key must be exactly %d bytes, got %d", keyLen, len(key))
	}
	id := keyID(key)
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.keys[id]; !ok {
		v.keys[id] = append([]byte(nil), key...)
	}
	return id, nil
}

// Succeed returns a new vault whose active key is key and that still holds every
// key v holds, so it can open what v sealed and verify what v signed. v is not
// changed. A master-key rotation uses it to seal under the new key and check the
// old key's attestation in the same run.
func (v *Vault) Succeed(key []byte) (*Vault, error) {
	next, err := New(key, v.source)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, k := range v.keys {
		if _, ok := next.keys[id]; !ok {
			next.keys[id] = append([]byte(nil), k...)
		}
	}
	return next, nil
}

func newVault(key []byte, source string) *Vault {
	id := keyID(key)
	return &Vault{
		keys:    map[string][]byte{id: key},
		active:  id,
		source:  source,
		subkeys: make(map[string][]byte),
	}
}

func keyID(key []byte) string {
	h := sha256.New()
	h.Write([]byte(keyIDDomain))
	h.Write(key)
	return keyIDPrefix + hex.EncodeToString(h.Sum(nil)[:8])
}

// KeyID is the active key's ID, recorded on every ciphertext and audit entry.
func (v *Vault) KeyID() string { return v.active }

// KeySource reports where the key came from: "env" (TIERPLAN_ENCRYPTION_KEY or
// its _FILE variant) or "file" (<data dir>/tierplan.key).
func (v *Vault) KeySource() string { return v.source }

// Subkey derives the 32-byte subkey for label from the active key.
func (v *Vault) Subkey(label string) []byte {
	k, err := v.SubkeyFor(v.active, label)
	if err != nil {
		// Unreachable: the active key is always held.
		panic("vault: " + err.Error())
	}
	return k
}

// SubkeyFor derives the subkey for label from the key with the given ID, for
// verifying data written under an older key. The result is a copy.
func (v *Vault) SubkeyFor(id, label string) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	master, ok := v.keys[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownKey, id)
	}
	cacheKey := id + "\x00" + label
	sub, ok := v.subkeys[cacheKey]
	if !ok {
		var err error
		sub, err = hkdf.Key(sha256.New, master, nil, label, subkeyLenBytes)
		if err != nil {
			return nil, fmt.Errorf("derive subkey: %w", err)
		}
		v.subkeys[cacheKey] = sub
	}
	return append([]byte(nil), sub...), nil
}

func (v *Vault) aead(id string) (cipher.AEAD, error) {
	sub, err := v.SubkeyFor(id, LabelVaultAEAD)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		return nil, fmt.Errorf("vault: %w", err)
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with AES-256-GCM under the active key. context
// binds the ciphertext to its purpose: Open fails if it differs.
func (v *Vault) Seal(context string, plaintext []byte) (string, error) {
	gcm, err := v.aead(v.active)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("vault: generate nonce: %w", err)
	}
	body := gcm.Seal(nonce, nonce, plaintext, []byte(aadPrefix+context))
	return envelopeV1 + "." + v.active + "." + base64.RawURLEncoding.EncodeToString(body), nil
}

// Open decrypts a value produced by Seal with the same context. It returns
// ErrMalformed, ErrUnknownKey or ErrDecrypt so callers can tell a bad value
// from a missing key from a failed authentication.
func (v *Vault) Open(context, sealed string) ([]byte, error) {
	version, rest, ok := strings.Cut(sealed, ".")
	if !ok || version != envelopeV1 {
		return nil, ErrMalformed
	}
	id, encoded, ok := strings.Cut(rest, ".")
	if !ok || id == "" || encoded == "" {
		return nil, ErrMalformed
	}
	gcm, err := v.aead(id)
	if err != nil {
		return nil, err
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(body) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrMalformed
	}
	nonce, ct := body[:gcm.NonceSize()], body[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ct, []byte(aadPrefix+context))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
