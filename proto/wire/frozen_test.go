// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package wire

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares the JSON of v with testdata/<name>.json byte for byte. The
// files are the contract: an agent in the field parses exactly these bytes, so a
// failure here is a change of the frozen format, not a test to refresh. -update
// writes the files for a new message and must not be used to make a failure pass.
func golden(t *testing.T, name string, v any) []byte {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a golden file in the repository
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a golden file in the repository
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches its golden file (this format is frozen for all of 0.x):\n got: %s\nwant: %s", name, got, want)
	}
	return want
}

// withExtra adds an unknown field to a JSON object, as a later version would.
func withExtra(t *testing.T, line []byte) []byte {
	t.Helper()
	s := strings.TrimSpace(string(line))
	if !strings.HasSuffix(s, "}") {
		t.Fatalf("not an object: %s", s)
	}
	return []byte(strings.TrimSuffix(s, "}") + `,"futureField":{"a":[1,2]},"another":"x"}`)
}

func TestFrozenMessagesMatchTheirGoldenFiles(t *testing.T) {
	golden(t, "update", Update{T: TypeUpdate, ID: "req-1", Image: "ghcr.io/ianglennon/bayplan-agent:0.2.1"})
	golden(t, "update_digest", Update{T: TypeUpdate, Image: "ghcr.io/ianglennon/bayplan-agent@sha256:" + strings.Repeat("a", 64)})
	golden(t, "uninstall", Uninstall{T: TypeUninstall, ID: "req-2", RemoveData: true})
	golden(t, "update_result", UpdateResult{T: TypeUpdateResult, ID: "req-1", OK: true})
	golden(t, "update_result_error", UpdateResult{T: TypeUpdateResult, OK: false, Error: UpdateErrImageNotOwnRepository})
}

func TestFrozenMessageTypeNamesDoNotChange(t *testing.T) {
	for got, want := range map[string]string{
		TypeUpdate:       "update",
		TypeUninstall:    "uninstall",
		TypeUpdateResult: "update_result",
	} {
		if got != want {
			t.Errorf("a frozen message type changed: %q, want %q", got, want)
		}
	}
}

func TestFrozenMessagesIgnoreUnknownFieldsAndKeepTheKnownOnes(t *testing.T) {
	t.Run("update", func(t *testing.T) {
		var m Update
		if err := json.Unmarshal(withExtra(t, golden(t, "update", Update{T: TypeUpdate, ID: "req-1", Image: "ghcr.io/ianglennon/bayplan-agent:0.2.1"})), &m); err != nil {
			t.Fatalf("an unknown field must not fail the parse: %v", err)
		}
		if m.T != TypeUpdate || m.ID != "req-1" || m.Image != "ghcr.io/ianglennon/bayplan-agent:0.2.1" {
			t.Fatalf("known fields lost: %+v", m)
		}
	})
	t.Run("uninstall", func(t *testing.T) {
		var m Uninstall
		if err := json.Unmarshal(withExtra(t, golden(t, "uninstall", Uninstall{T: TypeUninstall, ID: "req-2", RemoveData: true})), &m); err != nil {
			t.Fatalf("an unknown field must not fail the parse: %v", err)
		}
		if m.T != TypeUninstall || m.ID != "req-2" || !m.RemoveData {
			t.Fatalf("known fields lost: %+v", m)
		}
	})
	t.Run("update_result", func(t *testing.T) {
		var m UpdateResult
		if err := json.Unmarshal(withExtra(t, golden(t, "update_result", UpdateResult{T: TypeUpdateResult, ID: "req-1", OK: true})), &m); err != nil {
			t.Fatalf("an unknown field must not fail the parse: %v", err)
		}
		if m.T != TypeUpdateResult || m.ID != "req-1" || !m.OK || m.Error != "" {
			t.Fatalf("known fields lost: %+v", m)
		}
	})
}

// A request without an id is legal, and the result then carries an empty id.
func TestAnUpdateWithoutAnIDParsesAndItsResultCarriesAnEmptyID(t *testing.T) {
	var m Update
	if err := json.Unmarshal([]byte(`{"t":"update","image":"x/y:1"}`), &m); err != nil || m.ID != "" {
		t.Fatalf("parse: %v, id %q", err, m.ID)
	}
	b, _ := json.Marshal(UpdateResult{T: TypeUpdateResult})
	if got, want := string(b), `{"t":"update_result","id":"","ok":false,"error":""}`; got != want {
		t.Fatalf("an empty result is %s, want %s", got, want)
	}
}

func TestCapIDCutsToTheLimitOnACharacterBoundary(t *testing.T) {
	if got := CapID("abc"); got != "abc" {
		t.Fatalf("a short id changed: %q", got)
	}
	long := strings.Repeat("a", MaxMessageIDLen+10)
	if got := CapID(long); len(got) != MaxMessageIDLen {
		t.Fatalf("length %d, want %d", len(got), MaxMessageIDLen)
	}
	multi := strings.Repeat("é", MaxMessageIDLen) // two bytes each
	got := CapID(multi)
	if len(got) > MaxMessageIDLen || !strings.HasPrefix(multi, got) || strings.ContainsRune(got, '�') {
		t.Fatalf("the cut split a character: %q (%d bytes)", got, len(got))
	}
}
