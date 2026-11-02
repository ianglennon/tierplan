// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/internal/vault"
)

// PLAT-07 (D-55, D-56): no join token and no CA private key can be read back out
// of TierPlan's data directory, in any file, in any common encoding. The search is
// over raw bytes with no checkpoint, so a page that still lives only in the WAL,
// or in a free page, counts. tierplan.key is the master key and is the one file
// that is allowed to hold a secret: it is what protects the rest.

// encodings gives the raw bytes of a secret and the text forms it could be stored
// in, each under a name for the failure message.
func encodings(what string, raw []byte) map[string][]byte {
	return map[string][]byte{
		what + " (raw)":       raw,
		what + " (base64)":    []byte(base64.StdEncoding.EncodeToString(raw)),
		what + " (base64url)": []byte(base64.RawURLEncoding.EncodeToString(raw)),
		what + " (hex)":       []byte(hex.EncodeToString(raw)),
	}
}

// needlesFor lists everything that must not be in the data directory: each join
// token, its secret part and the bytes of that secret, the CA key as PKCS #8, as
// Go holds it and as its bare seed, each in the encodings above, and the PEM
// marker of a private key.
func needlesFor(t *testing.T, srv *tunneltest.Server, envID string, tokens ...string) map[string][]byte {
	t.Helper()
	needles := map[string][]byte{"a PEM private key": []byte("PRIVATE KEY")}
	for i, tok := range tokens {
		_, secret, ok := agents.ParseToken(tok)
		if !ok {
			t.Fatalf("token %d does not parse", i)
		}
		needles["join token "+string(rune('1'+i))] = []byte(tok)
		needles["secret part of token "+string(rune('1'+i))] = []byte(secret)
		raw, err := base64.RawURLEncoding.DecodeString(secret)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range encodings("secret bytes of token "+string(rune('1'+i)), raw) {
			needles[k] = v
		}
	}
	// The CA key is obtained by opening the sealed value, so the search is for the
	// real key and not a stand-in.
	var sealed string
	if err := srv.Store.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = ?`, envID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	der, err := srv.Vault.Open("agent-ca/v1/"+envID, sealed)
	if err != nil {
		t.Fatalf("open the sealed CA key: %v", err)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatalf("parse the CA key: %v", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		t.Fatalf("the CA key is a %T, want ed25519", parsed)
	}
	for k, v := range encodings("CA key (PKCS #8 DER)", der) {
		needles[k] = v
	}
	for k, v := range encodings("CA key (private key bytes)", priv) {
		needles[k] = v
	}
	for k, v := range encodings("CA key (seed)", priv.Seed()) {
		needles[k] = v
	}
	return needles
}

// scanDataDir reads every file under dir except the master key file and reports
// each needle found, as "<file>: <what>". It also returns the files it read, so a
// test can prove the search looked where it was meant to.
func scanDataDir(t *testing.T, dir string, needles map[string][]byte) (found, scanned []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == vault.KeyFile {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // the test's own data directory
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		scanned = append(scanned, rel)
		for what, needle := range needles {
			if len(needle) == 0 {
				t.Fatalf("an empty needle for %s would match every file", what)
			}
			if bytes.Contains(b, needle) {
				found = append(found, rel+": "+what)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(found)
	sort.Strings(scanned)
	return found, scanned
}

func TestNoPlaintextSecrets(t *testing.T) {
	t.Run("after an enrolment and a rotation nothing in the data directory is a secret", func(t *testing.T) {
		srv := tunneltest.Start(t, tunneltest.Options{})
		env := srv.NewEnvironment(t, "Production")
		// A real handshake: the token is presented over the wire and the agent's
		// certificate is signed by the sealed CA key.
		srv.Enrol(t, env)
		rot, err := srv.Svc.RotateToken(context.Background(), audit.SystemCLI, env.ID, "203.0.113.9", "test")
		if err != nil {
			t.Fatal(err)
		}
		if c := srv.Dial(t, tunneltest.Agent{Token: rot.JoinToken}); c.Welcome == nil {
			t.Fatalf("enrol with the rotated token was refused: %+v (err %v)", c.Refuse, c.Err)
		}

		needles := needlesFor(t, srv, env.ID, env.Token, rot.JoinToken)
		found, scanned := scanDataDir(t, srv.DataDir, needles)
		for _, f := range found {
			t.Errorf("a secret is in the data directory in plaintext: %s", f)
		}

		// Prove the search looked at the database and its write-ahead log, and never
		// at the master key.
		have := strings.Join(scanned, " ")
		for _, want := range []string{"tierplan.db", "tierplan.db-wal"} {
			if !strings.Contains(" "+have+" ", " "+want+" ") {
				t.Errorf("the search did not read %s; it read %v", want, scanned)
			}
		}
		if strings.Contains(have, vault.KeyFile) {
			t.Errorf("the search read the master key file: %v", scanned)
		}
		if _, err := os.Stat(filepath.Join(srv.DataDir, vault.KeyFile)); err != nil {
			t.Errorf("the master key is not in the data directory the search covered, so the exemption proves nothing: %v", err)
		}
	})

	t.Run("every sealed CA key carries the key ID and opens only for its own environment", func(t *testing.T) {
		srv := tunneltest.Start(t, tunneltest.Options{})
		a := srv.NewEnvironment(t, "A")
		b := srv.NewEnvironment(t, "B")

		rows, err := srv.Store.Read.Query(`SELECT env_id, key_sealed FROM env_cas`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		sealed := map[string]string{}
		for rows.Next() {
			var id, s string
			if err := rows.Scan(&id, &s); err != nil {
				t.Fatal(err)
			}
			sealed[id] = s
			if prefix := "v1." + srv.Vault.KeyID() + "."; !strings.HasPrefix(s, prefix) {
				t.Errorf("the sealed key of %s starts %q, want %q", id, s[:min(len(s), len(prefix)+4)], prefix)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(sealed) != 2 {
			t.Fatalf("%d sealed keys, want one per environment", len(sealed))
		}
		if _, err := srv.Vault.Open("agent-ca/v1/"+a.ID, sealed[a.ID]); err != nil {
			t.Fatalf("the key of A does not open for A: %v", err)
		}
		if _, err := srv.Vault.Open("agent-ca/v1/"+b.ID, sealed[a.ID]); !errors.Is(err, vault.ErrDecrypt) {
			t.Fatalf("the key of A opened under B's context: %v, want the vault's decrypt error", err)
		}
	})

	// A scanner that never finds anything would pass the cases above. This one puts
	// each secret into the database in plaintext, in its own store, and the scan has
	// to name every one.
	t.Run("negative control: the search finds a secret written in plaintext", func(t *testing.T) {
		srv := tunneltest.Start(t, tunneltest.Options{})
		env := srv.NewEnvironment(t, "Leaky")
		srv.Enrol(t, env)
		needles := needlesFor(t, srv, env.ID, env.Token)
		if found, _ := scanDataDir(t, srv.DataDir, needles); len(found) != 0 {
			t.Fatalf("the control store already holds a secret before the leak: %v", found)
		}

		var sealed string
		if err := srv.Store.Read.QueryRow(`SELECT key_sealed FROM env_cas WHERE env_id = ?`, env.ID).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		der, err := srv.Vault.Open("agent-ca/v1/"+env.ID, sealed)
		if err != nil {
			t.Fatal(err)
		}
		srv.Exec(t, `CREATE TABLE scratch (what TEXT, v BLOB)`)
		srv.Exec(t, `INSERT INTO scratch VALUES ('token', ?)`, env.Token)
		srv.Exec(t, `INSERT INTO scratch VALUES ('ca key hex', ?)`, hex.EncodeToString(der))
		srv.Exec(t, `INSERT INTO scratch VALUES ('ca key der', ?)`, der)
		srv.Exec(t, `INSERT INTO scratch VALUES ('pem', ?)`, "-----BEGIN PRIVATE KEY-----")

		found, _ := scanDataDir(t, srv.DataDir, needles)
		joined := strings.Join(found, "\n")
		for _, want := range []string{
			"join token 1",
			"secret part of token 1",
			"CA key (PKCS #8 DER) (raw)",
			"CA key (PKCS #8 DER) (hex)",
			"CA key (seed) (raw)",
			"a PEM private key",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("the search missed %q although it was written in plaintext; found:\n%s", want, joined)
			}
		}
	})
}
