// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresswire

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

func TestPeekBundleID(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "bundle_envelope.json"))
	if err != nil {
		t.Fatalf("read the frozen envelope golden: %v", err)
	}

	other, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 9},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: []byte("tls:\n  options: {}\n")}}, nil)
	if err != nil {
		t.Fatalf("build another bundle: %v", err)
	}
	otherFrame, err := json.Marshal(BundleEnvelope{T: TypeBundle, InstanceID: fixedInstanceID, Bundle: other})
	if err != nil {
		t.Fatalf("marshal another envelope: %v", err)
	}

	// frame builds an envelope-shaped frame with the given type and bundle id, both as raw JSON text.
	frame := func(typ, id string) []byte {
		return []byte(`{"t":` + typ + `,"instanceId":"x","bundle":{"id":` + id + `}}`)
	}
	valid := strings.Repeat("a1", 32)

	padded := append(bytes.TrimSpace(golden), bytes.Repeat([]byte(" "), MaxBundleMessageBytes+1-len(bytes.TrimSpace(golden)))...)
	if len(padded) != MaxBundleMessageBytes+1 {
		t.Fatalf("the padded frame is %d bytes, want %d", len(padded), MaxBundleMessageBytes+1)
	}

	for _, c := range []struct {
		name  string
		frame []byte
		want  string
	}{
		{"the frozen golden", golden, fixedEnvelope(t).Bundle.ID},
		{"another bundle", otherFrame, other.ID},
		{"a path as the id", frame(`"bundle"`, `"../../etc"`), ""},
		{"65 hex characters", frame(`"bundle"`, `"`+valid+`a"`), ""},
		{"63 hex characters", frame(`"bundle"`, `"`+valid[:63]+`"`), ""},
		{"upper-case hex", frame(`"bundle"`, `"`+strings.ToUpper(valid)+`"`), ""},
		{"non-hex letters", frame(`"bundle"`, `"`+strings.Repeat("g1", 32)+`"`), ""},
		{"the id is not a string", frame(`"bundle"`, `12`), ""},
		{"a missing id", []byte(`{"t":"bundle","instanceId":"x","bundle":{}}`), ""},
		{"a missing bundle", []byte(`{"t":"bundle","instanceId":"x"}`), ""},
		{"a type that is not bundle", frame(`"hello"`, `"`+valid+`"`), ""},
		{"a missing type", []byte(`{"bundle":{"id":"` + valid + `"}}`), ""},
		{"invalid JSON", []byte(`{"t":"bundle","bundle":`), ""},
		{"an empty frame", nil, ""},
		{"a valid envelope one byte over the limit", padded, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := PeekBundleID(c.frame); got != c.want {
				t.Fatalf("PeekBundleID = %q, want %q", got, c.want)
			}
		})
	}
}
