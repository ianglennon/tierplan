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
	pem, err := os.ReadFile(filepath.Join(dir, KeyFile)) //nolint:gosec // a test reads the file it just wrote
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
	b, err := os.ReadFile(filepath.Join(dir, ConnectedFile)) //nolint:gosec // a test reads the file it just wrote
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil || at.Before(before) || at.After(time.Now().Add(2*time.Second)) {
		t.Fatalf("connected marker = %q (%v), want the current time as RFC 3339", b, err)
	}
}

// pin builds a well-formed pin from one letter.
func pin(c string) string { return "sha256/" + strings.Repeat(c, 43) + "=" }

func pinsOf(es []PinEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Pin)
	}
	return out
}

func TestMergePinsStartsFromWhatTheServerSays(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	got := MergePins(nil, []string{pin("B")}, now)
	if len(got) != 1 || got[0].Pin != pin("B") || !got[0].ExpiresAt.IsZero() {
		t.Fatalf("pins = %+v, want B with no expiry", got)
	}
}

func TestMergePinsKeepsAPinTheServerDroppedFor30DaysAndThenLetsItGo(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	current := []PinEntry{{Pin: pin("A")}}

	got := MergePins(current, []string{pin("B")}, now)
	if len(got) != 2 {
		t.Fatalf("pins = %+v, want A and B", got)
	}
	var a, b PinEntry
	for _, e := range got {
		switch e.Pin {
		case pin("A"):
			a = e
		case pin("B"):
			b = e
		}
	}
	if want := now.Add(PinOverlap); !a.ExpiresAt.Equal(want) {
		t.Errorf("A expires %v, want %v (30 days)", a.ExpiresAt, want)
	}
	if PinOverlap != 30*24*time.Hour {
		t.Errorf("PinOverlap = %v, want 30 days", PinOverlap)
	}
	if !b.ExpiresAt.IsZero() {
		t.Errorf("B expires %v, want never while the server lists it", b.ExpiresAt)
	}

	// A later welcome does not push the expiry out: the clock started once.
	got = MergePins(got, []string{pin("B")}, now.Add(10*24*time.Hour))
	for _, e := range got {
		if e.Pin == pin("A") && !e.ExpiresAt.Equal(now.Add(PinOverlap)) {
			t.Errorf("A expires %v after a second welcome, want the first deadline", e.ExpiresAt)
		}
	}

	// Just before the deadline A is still there; after it A is dropped.
	if got := MergePins(got, []string{pin("B")}, now.Add(PinOverlap-time.Second)); !slicesContain(pinsOf(got), pin("A")) {
		t.Errorf("A dropped before its expiry: %+v", got)
	}
	if got := MergePins(got, []string{pin("B")}, now.Add(PinOverlap)); slicesContain(pinsOf(got), pin("A")) {
		t.Errorf("A kept at its expiry: %+v", got)
	}
}

func TestMergePinsBringsBackAPinTheServerListsAgain(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	current := []PinEntry{{Pin: pin("A"), ExpiresAt: now.Add(time.Hour)}}
	got := MergePins(current, []string{pin("A")}, now)
	if len(got) != 1 || !got[0].ExpiresAt.IsZero() {
		t.Fatalf("pins = %+v, want A with no expiry", got)
	}
}

func TestMergePinsIgnoresWhatIsNotAPinAndCapsTheList(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	got := MergePins(nil, []string{"", "md5/xx", "sha256/short", pin("C"), pin("C")}, now)
	if len(got) != 1 || got[0].Pin != pin("C") {
		t.Fatalf("pins = %+v, want only the one well-formed pin, once", got)
	}
	var many []string
	for i := range 40 {
		many = append(many, "sha256/"+strings.Repeat(string(rune('A'+i%26)), 42)+string(rune('a'+i/26))+"=")
	}
	if got := MergePins(nil, many, now); len(got) > MaxPins {
		t.Errorf("%d pins kept, want at most %d", len(got), MaxPins)
	}
}

func TestActivePinsSkipsTheExpired(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	es := []PinEntry{{Pin: pin("A")}, {Pin: pin("B"), ExpiresAt: now.Add(time.Hour)}, {Pin: pin("C"), ExpiresAt: now}}
	if got := ActivePins(es, now); !slicesEqual(got, []string{pin("A"), pin("B")}) {
		t.Fatalf("ActivePins = %v, want A and B", got)
	}
}

func TestSaveAndLoadPinsRoundTripAndAreMode0600(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	want := []PinEntry{{Pin: pin("A"), ExpiresAt: now.Add(PinOverlap)}, {Pin: pin("B")}}
	if err := SavePins(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, PinsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("pins.json: %v, %v; want mode 0600", fi, err)
	}
	got, err := LoadPins(dir)
	if err != nil || len(got) != 2 || got[0].Pin != pin("A") || !got[0].ExpiresAt.Equal(want[0].ExpiresAt) || got[1].Pin != pin("B") || !got[1].ExpiresAt.IsZero() {
		t.Fatalf("LoadPins = %+v, %v; want %+v", got, err, want)
	}
}

func TestLoadPinsOfAnEmptyDirectoryIsNoPinsAndADamagedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadPins(dir); err != nil || len(got) != 0 {
		t.Fatalf("LoadPins(empty) = %v, %v; want none", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, PinsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPins(dir); err == nil || !strings.Contains(err.Error(), PinsFile) {
		t.Fatalf("LoadPins(damaged) = %v, want an error naming %s", err, PinsFile)
	}
}

func slicesContain(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
