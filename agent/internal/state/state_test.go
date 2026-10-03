// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package state

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample(t *testing.T) State {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return State{AgentID: "agent-1", EnvID: "env-1", Key: priv, Cert: []byte("\x30\x82cert-der")}
}

func TestSaveThenLoadRoundTripsTheKeyTheCertificateAndTheIdentity(t *testing.T) {
	dir := t.TempDir()
	want := sample(t)
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := Load(dir)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v; want an identity", ok, err)
	}
	if got.AgentID != want.AgentID || got.EnvID != want.EnvID || !bytes.Equal(got.Cert, want.Cert) || !got.Key.Equal(want.Key) {
		t.Fatalf("Load = %+v, want %+v", got, want)
	}
}

func TestEveryStateFileIsPrivateToTheAgentUser(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{KeyFile, CertFile, IdentityFile} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", name, fi.Mode().Perm())
		}
	}
	pem, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil || !strings.Contains(string(pem), "BEGIN PRIVATE KEY") {
		t.Errorf("key.pem is not a PKCS#8 PEM: %q, %v", pem, err)
	}
}

func TestLoadOnAnEmptyDirectoryReportsNoIdentityWithoutAnError(t *testing.T) {
	for name, dir := range map[string]string{
		"empty":   t.TempDir(),
		"missing": filepath.Join(t.TempDir(), "not-there"),
	} {
		got, ok, err := Load(dir)
		if err != nil || ok || got.AgentID != "" || got.Key != nil {
			t.Errorf("%s: Load = %+v, %v, %v; want no identity and no error", name, got, ok, err)
		}
	}
}

func TestLoadRefusesAnIdentityWhoseKeyIsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, KeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Load(dir); err == nil || ok {
		t.Fatalf("Load with a missing key = %v, %v; want an error", ok, err)
	}
}

func TestASaveThatCannotFinishLeavesThePreviousStateIntact(t *testing.T) {
	dir := t.TempDir()
	first := sample(t)
	if err := Save(dir, first); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory where the certificate file belongs makes the final
	// step of the second Save fail.
	if err := os.Remove(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, CertFile, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	second := sample(t)
	second.AgentID = "agent-2"
	if err := Save(dir, second); err == nil {
		t.Fatal("Save succeeded although the certificate path is blocked")
	}
	if err := os.RemoveAll(filepath.Join(dir, CertFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, CertFile), first.Cert, 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir)
	if err != nil || !ok || got.AgentID != "agent-1" || !got.Key.Equal(first.Key) {
		t.Fatalf("after the failed Save, Load = %+v, %v, %v; want the first identity untouched", got, ok, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "*.tmp*"))
	if len(left) != 0 {
		t.Errorf("a failed Save left temporary files behind: %v", left)
	}
}

func TestSaveLeavesNoTemporaryFilesAndRefusesAnIncompleteState(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Errorf("temporary file %s left behind", e.Name())
		}
	}
	for name, s := range map[string]State{
		"no key":      {AgentID: "a", EnvID: "e", Cert: []byte("c")},
		"no cert":     {AgentID: "a", EnvID: "e", Key: sample(t).Key},
		"no agent id": {EnvID: "e", Key: sample(t).Key, Cert: []byte("c")},
		"no env id":   {AgentID: "a", Key: sample(t).Key, Cert: []byte("c")},
	} {
		if err := Save(t.TempDir(), s); err == nil {
			t.Errorf("Save accepted a state with %s", name)
		}
	}
	if err := Save(filepath.Join(dir, "not-there"), sample(t)); err == nil {
		t.Error("Save created a missing state directory; the image provides it with the right owner")
	}
}

func TestMarkConnectedWritesTheTimeOfTheHandshake(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Add(-2 * time.Second)
	if err := MarkConnected(dir); err != nil {
		t.Fatalf("MarkConnected: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ConnectedFile))
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || at.Before(before) || at.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("connected marker = %q (%v), want the current time as RFC 3339", b, err)
	}
}
