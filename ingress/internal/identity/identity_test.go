// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/ingress/internal/fsx"
	"github.com/ianglennon/tierplan/ingress/internal/fsx/fsxtest"
)

const (
	keyRel    = "state/node.key"
	recordRel = "state/identity.json"
)

// preparedRoot is a temporary root with the state/ and tmp/ directories the supervisor's layout makes.
func preparedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"state", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatalf("make %s: %v", d, err)
		}
	}
	return root
}

func readFile(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func TestKeyCreatedOnceAndReloaded(t *testing.T) {
	root := preparedRoot(t)
	key, err := New(fsx.OS(), root).LoadOrCreateKey()
	if err != nil {
		t.Fatalf("create the key: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("key is %d bytes, want %d", len(key), ed25519.PrivateKeySize)
	}
	path := filepath.Join(root, keyRel)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the key file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode is %o, want 600", info.Mode().Perm())
	}
	block, rest := pem.Decode(readFile(t, root, keyRel))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("the key file is not one PRIVATE KEY PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse PKCS#8: %v", err)
	}
	if got, ok := parsed.(ed25519.PrivateKey); !ok || !bytes.Equal(got, key) {
		t.Fatalf("the file does not hold the returned Ed25519 key")
	}

	before := readFile(t, root, keyRel)
	again, err := New(fsx.OS(), root).LoadOrCreateKey()
	if err != nil {
		t.Fatalf("reload the key: %v", err)
	}
	if !bytes.Equal(again, key) {
		t.Fatalf("a new Store over the same root returned a different key")
	}
	s := New(fsx.OS(), root)
	for i := 0; i < 2; i++ {
		if k, err := s.LoadOrCreateKey(); err != nil || !bytes.Equal(k, key) {
			t.Fatalf("call %d on one Store: %v", i, err)
		}
	}
	if !bytes.Equal(readFile(t, root, keyRel), before) {
		t.Fatalf("the key file changed on reload")
	}
}

func TestRecordRoundTrip(t *testing.T) {
	root := preparedRoot(t)
	s := New(fsx.OS(), root)
	if got, err := s.Enrolled(); err != nil || got {
		t.Fatalf("Enrolled on an empty volume = %v, %v; want false, nil", got, err)
	}
	if _, ok, err := s.Load(); err != nil || ok {
		t.Fatalf("Load on an empty volume = ok %v, err %v; want false, nil", ok, err)
	}
	want := Record{NodeID: "n-1", EnvID: "e-1", InstanceID: "i-1"}
	if err := s.Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, recordRel))
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode is %o, want 600", info.Mode().Perm())
	}
	fresh := New(fsx.OS(), root)
	got, ok, err := fresh.Load()
	if err != nil || !ok || got != want {
		t.Fatalf("Load on a new Store = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	if enrolled, err := fresh.Enrolled(); err != nil || !enrolled {
		t.Fatalf("Enrolled = %v, %v; want true, nil", enrolled, err)
	}
}

func TestSaveRefusesIncompleteRecord(t *testing.T) {
	root := preparedRoot(t)
	s := New(fsx.OS(), root)
	for name, r := range map[string]Record{
		"empty node ID":     {EnvID: "e-1", InstanceID: "i-1"},
		"empty instance ID": {NodeID: "n-1", EnvID: "e-1"},
		"empty record":      {},
	} {
		if err := s.Save(r); err == nil {
			t.Errorf("%s: Save accepted it", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, recordRel)); !os.IsNotExist(err) {
		t.Fatalf("a refused record left a file behind: %v", err)
	}
	if enrolled, err := s.Enrolled(); err != nil || enrolled {
		t.Fatalf("Enrolled after refusals = %v, %v; want false, nil", enrolled, err)
	}
}

// writeFile puts raw bytes at a path under root, as a damaged volume would hold them.
func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// damagedKeys are files at state/node.key that exist but are not a node key.
func damagedKeys(t *testing.T) map[string][]byte {
	t.Helper()
	root := preparedRoot(t)
	if _, err := New(fsx.OS(), root).LoadOrCreateKey(); err != nil {
		t.Fatalf("make a real key: %v", err)
	}
	good := readFile(t, root, keyRel)
	return map[string][]byte{
		"garbage":   []byte("not a key at all"),
		"empty":     {},
		"truncated": good[:len(good)/2],
		"wrong PEM": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("abc")}),
		"bad DER":   pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("abc")}),
	}
}

func TestDamagedKeyIsNotReplaced(t *testing.T) {
	for name, damaged := range damagedKeys(t) {
		t.Run(name, func(t *testing.T) {
			root := preparedRoot(t)
			writeFile(t, root, keyRel, damaged)
			key, err := New(fsx.OS(), root).LoadOrCreateKey()
			if err == nil || key != nil {
				t.Fatalf("a damaged key file was accepted: key %v, err %v", key != nil, err)
			}
			if !strings.Contains(err.Error(), keyRel) {
				t.Errorf("the error does not name %s: %v", keyRel, err)
			}
			if !bytes.Equal(readFile(t, root, keyRel), damaged) {
				t.Errorf("the damaged key file was changed")
			}
			if entries, err := os.ReadDir(filepath.Join(root, "tmp")); err != nil || len(entries) != 0 {
				t.Errorf("a temporary file was made in tmp/: %v, %v", entries, err)
			}
		})
	}
}

func TestDamagedRecordIsNotReplaced(t *testing.T) {
	for name, damaged := range map[string][]byte{
		"garbage":       []byte("not json"),
		"empty":         {},
		"empty object":  []byte(`{}`),
		"no node ID":    []byte(`{"envId":"e","instanceId":"i"}`),
		"no instance":   []byte(`{"nodeId":"n","envId":"e"}`),
		"wrong typed":   []byte(`{"nodeId":5}`),
		"truncated rec": []byte(`{"nodeId":"n-1","envI`),
	} {
		t.Run(name, func(t *testing.T) {
			root := preparedRoot(t)
			writeFile(t, root, recordRel, damaged)
			s := New(fsx.OS(), root)
			_, ok, err := s.Load()
			if err == nil || ok || !strings.Contains(err.Error(), recordRel) {
				t.Fatalf("Load = ok %v, err %v; want an error naming %s", ok, err, recordRel)
			}
			enrolled, err := s.Enrolled()
			if err == nil || enrolled || !strings.Contains(err.Error(), recordRel) {
				t.Fatalf("Enrolled = %v, %v; want an error naming %s", enrolled, err, recordRel)
			}
			if !bytes.Equal(readFile(t, root, recordRel), damaged) {
				t.Errorf("the damaged record file was changed")
			}
		})
	}
}

// atomicWrite is what writing dst through tmp/ looks like: no write into state/ itself.
func atomicWrite(dst string) []string {
	return []string{
		"create tmp/.part-*", "write tmp/.part-*", "chmod tmp/.part-*", "sync tmp/.part-*", "close tmp/.part-*",
		"rename tmp/.part-* -> " + dst, "syncdir state",
	}
}

func TestWritesAreAtomic(t *testing.T) {
	root := preparedRoot(t)
	rec := fsxtest.New(fsx.OS(), root)
	s := New(rec, root)
	if _, err := s.LoadOrCreateKey(); err != nil {
		t.Fatalf("create the key: %v", err)
	}
	if got, want := rec.Ops(), atomicWrite(keyRel); !slices.Equal(got, want) {
		t.Fatalf("key creation did:\n%v\nwant:\n%v", got, want)
	}
	mark := len(rec.Ops())
	if err := s.Save(Record{NodeID: "n-1", EnvID: "e-1", InstanceID: "i-1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, want := rec.Ops()[mark:], atomicWrite(recordRel); !slices.Equal(got, want) {
		t.Fatalf("saving the record did:\n%v\nwant:\n%v", got, want)
	}
	mark = len(rec.Ops())
	if _, err := s.LoadOrCreateKey(); err != nil {
		t.Fatalf("reload the key: %v", err)
	}
	if _, _, err := s.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := rec.Ops()[mark:]; len(got) != 0 {
		t.Fatalf("reading wrote: %v", got)
	}
}

// TestKilledWriteLeavesNothingPartial kills the key write at every operation: the volume then
// has either no key file or the whole key, never a partial one that the next start would call damaged.
func TestKilledWriteLeavesNothingPartial(t *testing.T) {
	for n := 0; n < 7; n++ {
		root := preparedRoot(t)
		rec := fsxtest.New(fsx.OS(), root)
		rec.FailFrom(n)
		if _, err := New(rec, root).LoadOrCreateKey(); err == nil {
			t.Fatalf("kill at %d: the write reported success", n)
		}
		if _, err := New(fsx.OS(), root).LoadOrCreateKey(); err != nil {
			t.Fatalf("kill at %d: the next start cannot use the volume: %v", n, err)
		}
	}
}

func TestErrorsHoldNoKey(t *testing.T) {
	// A real key, then the same bytes damaged in ways that keep its base64 text in the file.
	root := preparedRoot(t)
	key, err := New(fsx.OS(), root).LoadOrCreateKey()
	if err != nil {
		t.Fatalf("make a key: %v", err)
	}
	pemText := string(readFile(t, root, keyRel))
	body := strings.ReplaceAll(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(pemText), "-----BEGIN PRIVATE KEY-----"), "-----END PRIVATE KEY-----")), "\n", "")
	secrets := []string{body, base64.StdEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(key.Seed()), hex.EncodeToString(key.Seed()), "BEGIN PRIVATE KEY"}

	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error holds key material: %.60q", what, err.Error())
			}
		}
	}

	for name, damaged := range map[string][]byte{
		"truncated":      []byte(pemText[:len(pemText)/2]),
		"body in a cert": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: key}),
		"body as json":   []byte(`{"nodeId":"` + body + `"`),
	} {
		dir := preparedRoot(t)
		writeFile(t, dir, keyRel, damaged)
		_, err := New(fsx.OS(), dir).LoadOrCreateKey()
		check("key "+name, err)
		writeFile(t, dir, recordRel, damaged)
		_, _, err = New(fsx.OS(), dir).Load()
		check("record "+name, err)
	}

	// A write that fails part way: the error is about the file, not its content.
	for n := 0; n < 7; n++ {
		dir := preparedRoot(t)
		rec := fsxtest.New(fsx.OS(), dir)
		rec.FailFrom(n)
		_, err := New(rec, dir).LoadOrCreateKey()
		if err == nil || strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("failed write %d: %v", n, err)
		}
	}

	// A record never formats the key: it has none.
	if s := fmt.Sprintf("%v %+v %#v", Record{NodeID: "n"}, Record{NodeID: "n"}, Record{NodeID: "n"}); strings.Contains(s, "PRIVATE") {
		t.Errorf("a Record formats key text: %s", s)
	}
}
