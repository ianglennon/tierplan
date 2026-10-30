// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package vault_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/vault"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestLoadOrCreateGeneratesKeyFile(t *testing.T) {
	dir := t.TempDir()
	log, buf := captureLog()

	v, err := vault.LoadOrCreate("", dir, log)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if v.KeySource() != "file" {
		t.Fatalf("KeySource = %q, want file", v.KeySource())
	}
	path := filepath.Join(dir, "coaming.key")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("key file mode = %o, want 600", got)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test reads the key file it just created
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	key, err := base64.StdEncoding.DecodeString(line)
	if err != nil || len(key) != 32 {
		t.Fatalf("key file is not one line of standard base64 for 32 bytes: %q (%v)", line, err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "back it up") || !strings.Contains(logs, path) {
		t.Fatalf("expected a back-up warning naming %s, got: %s", path, logs)
	}
	if strings.Contains(logs, line) {
		t.Fatal("the generated key was written to the log")
	}

	again, err := vault.LoadOrCreate("", dir, log)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if again.KeyID() != v.KeyID() {
		t.Fatalf("KeyID changed across loads: %s vs %s", again.KeyID(), v.KeyID())
	}
}

func TestLoadOrCreateWarnsOnBroadKeyFileMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coaming.key")
	if err := os.WriteFile(path, []byte(b64(testKey(7))+"\n"), 0o644); err != nil { //nolint:gosec // the test needs a broad mode
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // the test needs a broad mode
		t.Fatal(err)
	}
	log, buf := captureLog()
	if _, err := vault.LoadOrCreate("", dir, log); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !strings.Contains(buf.String(), path) || !strings.Contains(buf.String(), "WARN") {
		t.Fatalf("expected a warning naming %s, got: %s", path, buf.String())
	}
}

func TestConfigKeyWinsAndCreatesNoFile(t *testing.T) {
	dir := t.TempDir()
	log, _ := captureLog()
	v, err := vault.LoadOrCreate(b64(testKey(1)), dir, log)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if v.KeySource() != "env" {
		t.Fatalf("KeySource = %q, want env", v.KeySource())
	}
	if _, err := os.Stat(filepath.Join(dir, "coaming.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key file was created for an env key: %v", err)
	}
}

func TestSameKeyFromEnvAndFileHasSameID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "coaming.key"), []byte(b64(testKey(9))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	log, _ := captureLog()
	fromFile, err := vault.Load("", dir)
	if err != nil {
		t.Fatal(err)
	}
	fromEnv, err := vault.LoadOrCreate(b64(testKey(9)), t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.KeyID() != fromEnv.KeyID() {
		t.Fatalf("env and file sources gave different key IDs: %s vs %s", fromEnv.KeyID(), fromFile.KeyID())
	}
	if !strings.HasPrefix(fromFile.KeyID(), "k1-") || len(fromFile.KeyID()) != len("k1-")+16 {
		t.Fatalf("KeyID %q is not k1- plus 16 hex characters", fromFile.KeyID())
	}
}

func TestConfigKeyMustBe32Bytes(t *testing.T) {
	log, _ := captureLog()
	for name, bad := range map[string]string{
		"not base64": "!!!not base64!!!",
		"too short":  b64(testKey(1)[:16]),
		"too long":   b64(append(testKey(1), 0)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := vault.LoadOrCreate(bad, t.TempDir(), log)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), "COAMING_ENCRYPTION_KEY") {
				t.Fatalf("error does not name COAMING_ENCRYPTION_KEY: %v", err)
			}
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("error leaks the supplied key: %v", err)
			}
		})
	}
}

func TestLoadNeverGenerates(t *testing.T) {
	dir := t.TempDir()
	_, err := vault.Load("", dir)
	if !errors.Is(err, vault.ErrNoKey) {
		t.Fatalf("Load with no key = %v, want ErrNoKey", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "coaming.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load created a key file: %v", err)
	}
	if _, err := vault.Load("", filepath.Join(dir, "missing")); !errors.Is(err, vault.ErrNoKey) {
		t.Fatalf("Load in a missing directory = %v, want ErrNoKey", err)
	}
}

func newVault(t *testing.T) *vault.Vault {
	t.Helper()
	log, _ := captureLog()
	v, err := vault.LoadOrCreate(b64(testKey(3)), t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSealOpenRoundTrip(t *testing.T) {
	v := newVault(t)
	sealed, err := v.Seal("registry-credential", []byte("hunter2 and more"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v1."+v.KeyID()+".") {
		t.Fatalf("envelope %q does not start with v1.<keyID>.", sealed)
	}
	if strings.Contains(sealed, "hunter2") {
		t.Fatal("plaintext visible in the envelope")
	}
	got, err := v.Open("registry-credential", sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != "hunter2 and more" {
		t.Fatalf("Open = %q", got)
	}
	again, _ := v.Seal("registry-credential", []byte("hunter2 and more"))
	if again == sealed {
		t.Fatal("two Seals of the same plaintext are identical: nonce reused")
	}
}

func TestOpenFailuresAreDistinct(t *testing.T) {
	v := newVault(t)
	sealed, err := v.Seal("ctx-a", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := v.Open("ctx-b", sealed); !errors.Is(err, vault.ErrDecrypt) {
		t.Fatalf("wrong context = %v, want ErrDecrypt", err)
	}

	parts := strings.SplitN(sealed, ".", 3)
	raw, _ := base64.RawURLEncoding.DecodeString(parts[2])
	raw[len(raw)-1] ^= 0x01
	flipped := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(raw)
	if _, err := v.Open("ctx-a", flipped); !errors.Is(err, vault.ErrDecrypt) {
		t.Fatalf("flipped byte = %v, want ErrDecrypt", err)
	}

	unknown := parts[0] + ".k1-0000000000000000." + parts[2]
	_, err = v.Open("ctx-a", unknown)
	if !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("unknown key id = %v, want ErrUnknownKey", err)
	}
	if errors.Is(err, vault.ErrDecrypt) {
		t.Fatal("unknown key id must not be reported as a decrypt failure")
	}

	for _, malformed := range []string{"", "v1.", "v2." + parts[1] + "." + parts[2], "v1." + parts[1], "v1." + parts[1] + ".!!"} {
		if _, err := v.Open("ctx-a", malformed); err == nil {
			t.Fatalf("Open(%q) succeeded", malformed)
		}
	}
}

func TestSubkeys(t *testing.T) {
	v := newVault(t)
	a := v.Subkey("coaming/audit-hmac/v1")
	c := v.Subkey("coaming/csrf/v1")
	if len(a) != 32 || len(c) != 32 {
		t.Fatalf("subkey lengths = %d, %d, want 32", len(a), len(c))
	}
	if bytes.Equal(a, c) {
		t.Fatal("different labels gave the same subkey")
	}
	if !bytes.Equal(a, v.Subkey("coaming/audit-hmac/v1")) {
		t.Fatal("Subkey is not deterministic")
	}
	a[0] ^= 0xff
	if bytes.Equal(a, v.Subkey("coaming/audit-hmac/v1")) {
		t.Fatal("mutating a returned subkey changed the cached value")
	}

	other, err := vault.LoadOrCreate(b64(testKey(4)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(other.Subkey("coaming/csrf/v1"), v.Subkey("coaming/csrf/v1")) {
		t.Fatal("different master keys gave the same subkey")
	}

	same, err := v.SubkeyFor(v.KeyID(), "coaming/csrf/v1")
	if err != nil || !bytes.Equal(same, v.Subkey("coaming/csrf/v1")) {
		t.Fatalf("SubkeyFor(active) = %x, %v", same, err)
	}
	if _, err := v.SubkeyFor("k1-ffffffffffffffff", "coaming/csrf/v1"); !errors.Is(err, vault.ErrUnknownKey) {
		t.Fatalf("SubkeyFor(unknown) = %v, want ErrUnknownKey", err)
	}
}

func TestKeyIDRevealsNothingOfTheKey(t *testing.T) {
	key := testKey(0)
	for i := range key {
		key[i] = byte(i*7 + 1)
	}
	enc := b64(key)
	v, err := vault.LoadOrCreate(enc, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	id := v.KeyID()
	for i := 0; i+8 <= len(enc); i++ {
		if strings.Contains(id, enc[i:i+8]) {
			t.Fatalf("KeyID %q contains the key substring %q", id, enc[i:i+8])
		}
	}
	if strings.Contains(id, enc) {
		t.Fatal("KeyID contains the key")
	}
}
