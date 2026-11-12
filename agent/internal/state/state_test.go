// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package state

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
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

// D-19: a server that keeps listing the self-signed pin in every welcome (also while it serves a
// public certificate) never starts the pin's overlap, however long the agent runs.
func TestMergePinsNeverDatesAPinTheServerKeepsListingPastTheOverlap(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	got := MergePins(nil, []string{pin("S")}, now)
	for _, later := range []time.Duration{PinOverlap - time.Hour, PinOverlap + time.Hour, 3 * PinOverlap} {
		got = MergePins(got, []string{pin("S")}, now.Add(later))
		if len(got) != 1 || got[0].Pin != pin("S") || !got[0].ExpiresAt.IsZero() {
			t.Fatalf("after %v the pins are %+v, want S with no expiry", later, got)
		}
	}
	if active := ActivePins(got, now.Add(10*PinOverlap)); len(active) != 1 || active[0] != pin("S") {
		t.Errorf("active pins long after the overlap = %v, want S", active)
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
	if got := ActivePins(es, now); !slices.Equal(got, []string{pin("A"), pin("B")}) {
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

func TestDiscardRemovesTheIdentityAndOnlyTheIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	if err := MarkConnected(dir); err != nil {
		t.Fatal(err)
	}
	// Leftovers of an interrupted save go with the identity; other files stay.
	keep := map[string]string{PinsFile: "[]\n", "operator-notes.txt": "mine", "identity.json.bak": "backup"}
	for name, body := range keep {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile+tmpSuffix), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Discard(dir); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, ok, err := Load(dir); ok || err != nil {
		t.Fatalf("Load after Discard = %v, %v; want no identity and no error", ok, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if len(left) != len(keep) {
		t.Errorf("files left = %v, want exactly %v", left, keep)
	}
	for name, body := range keep {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a fixed name in a test directory
		if err != nil || string(got) != body {
			t.Errorf("%s = %q, %v; Discard must leave files that are not the identity", name, got, err)
		}
	}
}

func TestDiscardOfAnEmptyDirectoryIsNotAnErrorAndAllowsANewSave(t *testing.T) {
	dir := t.TempDir()
	if err := Discard(dir); err != nil {
		t.Fatalf("Discard of an empty directory: %v", err)
	}
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	if err := Discard(dir); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, sample(t)); err != nil {
		t.Fatalf("Save after Discard: %v", err)
	}
	if _, ok, err := Load(dir); !ok || err != nil {
		t.Fatalf("Load = %v, %v", ok, err)
	}
}

// A crash between the marker's removal and the rest leaves a key and a certificate
// without identity.json. That must read as "not enrolled", never as the damaged
// identity that Load refuses.
func TestAnInterruptedDiscardLeavesADirectoryThatReadsAsNotEnrolled(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, sample(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, IdentityFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Load(dir); ok || err != nil {
		t.Fatalf("Load = %v, %v; want not enrolled", ok, err)
	}
	if err := Save(dir, sample(t)); err != nil {
		t.Fatalf("Save over the leftovers: %v", err)
	}
}

// --- the addresses the server pushed (D-19) ---

const (
	urlA = "wss://tierplan.example.com/agent/connect"
	urlB = "wss://tierplan.internal:7480/agent/connect"
	urlC = "wss://c.example.org/tierplan/agent/connect"
)

func urlsOf(es []URLEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.URL)
	}
	return out
}

func TestMergeURLsStartsFromWhatTheServerSaysAndKeepsItsOrder(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	got := MergeURLs(nil, []string{urlB, urlC}, now)
	if !slices.Equal(urlsOf(got), []string{urlB, urlC}) {
		t.Fatalf("URLs = %v, want B then C in the server's order", urlsOf(got))
	}
	for _, e := range got {
		if !e.ExpiresAt.IsZero() {
			t.Errorf("%s expires %v, want never while the server lists it", e.URL, e.ExpiresAt)
		}
	}
}

func TestMergeURLsPutsTheNewURLFirstAndKeepsThePreviousOneAsAFallbackFor30Days(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	current := []URLEntry{{URL: urlA}}

	got := MergeURLs(current, []string{urlB}, now)
	if !slices.Equal(urlsOf(got), []string{urlB, urlA}) {
		t.Fatalf("URLs = %v, want the new B first and the previous A after it", urlsOf(got))
	}
	if !got[0].ExpiresAt.IsZero() {
		t.Errorf("the new URL expires %v, want never", got[0].ExpiresAt)
	}
	if want := now.Add(URLOverlap); !got[1].ExpiresAt.Equal(want) {
		t.Errorf("the previous URL expires %v, want %v", got[1].ExpiresAt, want)
	}
	if URLOverlap != 30*24*time.Hour {
		t.Errorf("URLOverlap = %v, want 30 days", URLOverlap)
	}

	// Hearing the same list again does not start the 30 days over.
	again := MergeURLs(got, []string{urlB}, now.Add(10*24*time.Hour))
	if !EqualURLs(again, got) {
		t.Errorf("a repeated list changed the entries: %+v, was %+v", again, got)
	}
}

func TestMergeURLsDropsThePreviousURLOnceItsThirtyDaysAreUp(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	got := MergeURLs([]URLEntry{{URL: urlA}}, []string{urlB}, now)

	if got := MergeURLs(got, []string{urlB}, now.Add(URLOverlap-time.Second)); !slicesContain(urlsOf(got), urlA) {
		t.Errorf("A dropped before its expiry: %+v", got)
	}
	if got := MergeURLs(got, []string{urlB}, now.Add(URLOverlap)); slicesContain(urlsOf(got), urlA) {
		t.Errorf("A kept at its expiry: %+v", got)
	}
	if got := ActiveURLs(got, now.Add(URLOverlap)); !slices.Equal(got, []string{urlB}) {
		t.Errorf("ActiveURLs after the expiry = %v, want only B", got)
	}
	if got := ActiveURLs(got, now); !slices.Equal(got, []string{urlB, urlA}) {
		t.Errorf("ActiveURLs inside the overlap = %v, want B then A", got)
	}
}

func TestActiveURLsSkipsTheExpiredAndKeepsTheOrder(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	es := []URLEntry{{URL: urlB}, {URL: urlA, ExpiresAt: now.Add(time.Hour)}, {URL: urlC, ExpiresAt: now}}
	if got := ActiveURLs(es, now); !slices.Equal(got, []string{urlB, urlA}) {
		t.Fatalf("ActiveURLs = %v, want B then A", got)
	}
}

func TestMergeURLsBringsBackAURLTheServerListsAgain(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	current := []URLEntry{{URL: urlB}, {URL: urlA, ExpiresAt: now.Add(time.Hour)}}
	got := MergeURLs(current, []string{urlA}, now)
	if len(got) != 2 || got[0].URL != urlA || !got[0].ExpiresAt.IsZero() || got[1].URL != urlB {
		t.Fatalf("URLs = %+v, want A first with no expiry, then B as the fallback", got)
	}
}

func TestMergeURLsIgnoresWhatIsNotAnAddressTheAgentWouldDialAndCapsTheList(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	bad := []string{
		"", "not a url", "https://tierplan.example.com/agent/connect", "ftp://x.example/agent/connect", "wss://",
		"wss://user:pw@tierplan.example.com/agent/connect", "wss://tierplan.example.com/agent/connect#frag",
		"wss://tierplan.example.com/agent/connect?token=x", "wss://tierplan.example.com\n/agent/connect",
		"wss://tierplan.example.com/" + strings.Repeat("a", MaxURLLen),
	}
	got := MergeURLs(nil, append(bad, urlA, urlA), now)
	if !slices.Equal(urlsOf(got), []string{urlA}) {
		t.Fatalf("URLs = %v, want only the one well-formed address, once", urlsOf(got))
	}
	var many []string
	for i := range 40 {
		many = append(many, "wss://h"+string(rune('a'+i%26))+string(rune('a'+i/26))+".example.com/agent/connect")
	}
	if got := MergeURLs(nil, many, now); len(got) > MaxURLs {
		t.Errorf("%d URLs kept, want at most %d", len(got), MaxURLs)
	}
}

func TestAnAddressTheServerPlainWSAllowedIsAValidURLToo(t *testing.T) {
	// ValidURL judges the shape; whether a plain ws:// address may be pushed over a
	// verified session is the connection's decision, not the file's.
	if !ValidURL("ws://127.0.0.1:7480/agent/connect") || !ValidURL(urlA) {
		t.Fatal("ws:// and wss:// addresses are valid URLs")
	}
}

func TestSaveAndLoadURLsRoundTripAndAreMode0600(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	want := []URLEntry{{URL: urlB}, {URL: urlA, ExpiresAt: now.Add(URLOverlap)}}
	if err := SaveURLs(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, URLsFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("urls.json: %v, %v; want mode 0600", fi, err)
	}
	got, err := LoadURLs(dir)
	if err != nil || len(got) != 2 || got[0].URL != urlB || !got[0].ExpiresAt.IsZero() || got[1].URL != urlA || !got[1].ExpiresAt.Equal(want[1].ExpiresAt) {
		t.Fatalf("LoadURLs = %+v, %v; want %+v", got, err, want)
	}
	if _, err := os.Stat(filepath.Join(dir, URLsFile+".tmp")); err == nil {
		t.Error("a temporary urls file was left behind")
	}
}

func TestLoadURLsOfAnEmptyDirectoryIsNoURLsAndADamagedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadURLs(dir); err != nil || len(got) != 0 {
		t.Fatalf("LoadURLs(empty) = %v, %v; want none", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, URLsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadURLs(dir); err == nil || !strings.Contains(err.Error(), URLsFile) {
		t.Fatalf("LoadURLs(damaged) = %v, want an error naming %s", err, URLsFile)
	}
}

func TestLoadURLsDropsEntriesThatAreNotAddresses(t *testing.T) {
	dir := t.TempDir()
	raw := `[{"url":"https://nope.example/agent/connect"},{"url":"` + urlB + `"}]`
	if err := os.WriteFile(filepath.Join(dir, URLsFile), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadURLs(dir)
	if err != nil || !slices.Equal(urlsOf(got), []string{urlB}) {
		t.Fatalf("LoadURLs = %+v, %v; want only B", got, err)
	}
}

// --- the TIERPLAN_AGENT_URL the agent last started with (G-02-4b) ---

func TestEnvURLRoundTripsIsPrivateAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveEnvURL(dir, urlA); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, EnvURLFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env-url.json: %v, %v; want mode 0600", fi, err)
	}
	if got, ok, err := LoadEnvURL(dir); err != nil || !ok || got != urlA {
		t.Fatalf("LoadEnvURL = %q, %v, %v; want %q", got, ok, err, urlA)
	}
	if _, err := os.Stat(filepath.Join(dir, EnvURLFile+".tmp")); err == nil {
		t.Error("a temporary env-url file was left behind")
	}
}

func TestLoadEnvURLOfAnEmptyDirectoryIsNoRecordAndADamagedFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if got, ok, err := LoadEnvURL(dir); err != nil || ok || got != "" {
		t.Fatalf("LoadEnvURL(empty) = %q, %v, %v; want no record", got, ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, EnvURLFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadEnvURL(dir); err == nil || !strings.Contains(err.Error(), EnvURLFile) {
		t.Fatalf("LoadEnvURL(damaged) = %v, want an error naming %s", err, EnvURLFile)
	}
}

func TestLoadEnvURLIgnoresARecordThatIsNotAnAddress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, EnvURLFile), []byte(`{"url":"https://nope.example/agent/connect"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := LoadEnvURL(dir); err != nil || ok || got != "" {
		t.Fatalf("LoadEnvURL = %q, %v, %v; want no record for an address the agent would not dial", got, ok, err)
	}
}

func TestStartURLsDecidesWhichAddressAnAgentStartsWith(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	soon := now.Add(24 * time.Hour)
	gone := now.Add(-time.Minute)
	stored := []URLEntry{{URL: urlB}, {URL: urlA, ExpiresAt: soon}, {URL: urlC, ExpiresAt: gone}}
	const fresh = "wss://new.example.net/agent/connect"
	for name, tc := range map[string]struct {
		stored      []URLEntry
		env, last   string
		known       bool
		want        []URLEntry
		wantChanged bool
	}{
		"nothing stored": {nil, fresh, urlA, true, []URLEntry{{URL: fresh}}, false},
		"only expired entries stored": {
			[]URLEntry{{URL: urlC, ExpiresAt: gone}}, fresh, urlA, true, []URLEntry{{URL: fresh}}, false},
		"no record of the last start":   {stored, fresh, "", false, stored, false},
		"the same address as last time": {stored, urlA, urlA, true, stored, false},
		"a different address": {
			stored, fresh, urlA, true,
			[]URLEntry{{URL: fresh}, {URL: urlB}, {URL: urlA, ExpiresAt: soon}}, true},
		"a different address that is already stored with an expiry": {
			stored, urlA, urlB, true,
			[]URLEntry{{URL: urlA}, {URL: urlB}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed := StartURLs(tc.stored, tc.env, tc.last, tc.known, now)
			if changed != tc.wantChanged || !EqualURLs(got, tc.want) {
				t.Fatalf("StartURLs = %+v, %v; want %+v, %v", got, changed, tc.want, tc.wantChanged)
			}
		})
	}
}

func TestStartURLsCapsTheListAtMaxURLs(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var stored []URLEntry
	for i := range MaxURLs {
		stored = append(stored, URLEntry{URL: "wss://h" + string(rune('a'+i)) + ".example.com/agent/connect"})
	}
	got, changed := StartURLs(stored, urlA, "wss://old.example.com/agent/connect", true, now)
	if !changed || len(got) != MaxURLs || got[0].URL != urlA {
		t.Fatalf("StartURLs kept %d addresses (changed %v) starting with %s, want %d starting with the new one", len(got), changed, got[0].URL, MaxURLs)
	}
}

func TestDialableURLAllowsOnlyWSS(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"wss://tierplan.example/agent/connect", true},
		{"wss://127.0.0.1:7480/agent/connect", true},
		{"ws://tierplan.example/agent/connect", false},
		{"ws://127.0.0.1:7480/agent/connect", false},
		{"ws://127.1.2.3/agent/connect", false},
		{"ws://[::1]:7480/agent/connect", false},
		{"ws://[::ffff:127.0.0.1]/agent/connect", false},
		{"ws://localhost:7480/agent/connect", false},
		{"ws://LOCALHOST/agent/connect", false},
		{"ws://192.168.1.10/agent/connect", false},
		// ValidURL refuses these.
		{"wss://user:pw@tierplan.example/agent/connect", false},
		{"ws://user@127.0.0.1/agent/connect", false},
		{"wss://tierplan.example/agent/connect?x=1", false},
		{"wss://tierplan.example/agent/connect#f", false},
		{"https://tierplan.example/agent/connect", false},
		{"", false},
	} {
		if got := DialableURL(tc.url); got != tc.want {
			t.Errorf("DialableURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
