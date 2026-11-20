// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// goldenDir is the renderer's own golden output. The test reads it across the module boundary; it
// imports nothing from the renderer.
const goldenDir = "../../../internal/ingressrender/testdata/golden"

func TestCheckAcceptsABundle(t *testing.T) {
	b := testBundle(t, "a.test.local")
	if err := Check(b); err != nil {
		t.Fatalf("Check refused a valid bundle: %v", err)
	}
}

func TestCheckAcceptsTheEmptyBundle(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(goldenDir, "empty", "tierplan.yml"))
	if err != nil {
		t.Fatalf("read the empty golden: %v", err)
	}
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 1},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: data}}, nil)
	if err != nil {
		t.Fatalf("build the empty bundle: %v", err)
	}
	if err := Check(b); err != nil {
		t.Fatalf("Check refused the empty bundle, which is valid and fails closed at the handshake: %v", err)
	}
}

// TestCertRefsMatchRendererGoldens proves the YAML reference scan against what the renderer writes: for
// every golden, the certificate paths certRefs finds are exactly the certs/ files the golden's manifest
// lists.
func TestCertRefsMatchRendererGoldens(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(goldenDir, "*"))
	if err != nil {
		t.Fatalf("glob the goldens: %v", err)
	}
	var seen int
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		seen++
		t.Run(filepath.Base(dir), func(t *testing.T) {
			dynamic, err := os.ReadFile(filepath.Join(dir, "tierplan.yml"))
			if err != nil {
				t.Fatalf("read tierplan.yml: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
			if err != nil {
				t.Fatalf("read bundle.json: %v", err)
			}
			var golden struct {
				Manifest struct {
					Files []struct {
						Path string `json:"path"`
					} `json:"files"`
				} `json:"manifest"`
			}
			if err := json.Unmarshal(raw, &golden); err != nil {
				t.Fatalf("parse bundle.json: %v", err)
			}
			want := []string{}
			for _, f := range golden.Manifest.Files {
				if strings.HasPrefix(f.Path, ingressbundle.CertsDir+"/") {
					want = append(want, f.Path)
				}
			}
			slices.Sort(want)

			refs, err := certRefs(dynamic)
			if err != nil {
				t.Fatalf("certRefs: %v", err)
			}
			got := []string{}
			for _, r := range refs {
				rel, ok := relativeCertPath(r)
				if !ok {
					t.Fatalf("a reference the renderer wrote is not under the cert root: %q", r)
				}
				got = append(got, rel)
			}
			slices.Sort(got)
			got = slices.Compact(got)
			if !slices.Equal(got, want) {
				t.Fatalf("certRefs = %v, the manifest lists %v", got, want)
			}
		})
	}
	if seen < 8 {
		t.Fatalf("found %d golden directories under %s, want at least the eight Phase 21 goldens", seen, goldenDir)
	}
}

// cloned is b with its own manifest and file slices, so a test row can change one without touching the
// bundle it started from.
func cloned(b ingressbundle.Bundle) ingressbundle.Bundle {
	out := b
	out.Manifest.Files = slices.Clone(b.Manifest.Files)
	out.Files = slices.Clone(b.Files)
	return out
}

// firstPath is the first file path of b with the given suffix.
func firstPath(t *testing.T, b ingressbundle.Bundle, suffix string) string {
	t.Helper()
	for _, f := range b.Files {
		if strings.HasSuffix(f.Path, suffix) {
			return f.Path
		}
	}
	t.Fatalf("no %s file in the bundle", suffix)
	return ""
}

// dynamicOf is the dynamic file of b as a string.
func dynamicOf(t *testing.T, b ingressbundle.Bundle) string {
	t.Helper()
	for _, f := range b.Files {
		if f.Path == ingressbundle.DynamicFile {
			return string(f.Data)
		}
	}
	t.Fatal("no dynamic file in the bundle")
	return ""
}

// withDynamic rebuilds b with its dynamic file replaced, so the change passes Verify.
func withDynamic(t *testing.T, b ingressbundle.Bundle, mutate func(string) string) ingressbundle.Bundle {
	t.Helper()
	return rebuild(t, withFile(b, ingressbundle.DynamicFile, []byte(mutate(dynamicOf(t, b)))))
}

// abs is the absolute path the YAML uses for a bundle path.
func abs(rel string) string { return ingressbundle.NodeRoot + "/" + rel }

// TestCheckRejects has one row per check-level reason, and asserts the exact stable code in a
// *ingressbundle.VerifyError. A row with verifies set targets a check after Verify, and the test asserts
// its bundle passes Verify, so the check it names is the one that is reached.
type rejectRow struct {
	name     string
	want     string
	verifies bool
	build    func(t *testing.T) ingressbundle.Bundle
}

// rejectRows is one faulty bundle per way Verify or Check refuses one. The store's tests reuse it, so the
// faults a frame can carry are written once.
func rejectRows() []rejectRow {
	const host = "a.test.local"
	return []rejectRow{
		{"schema_unsupported", ingressbundle.ReasonSchemaUnsupported, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			out, err := ingressbundle.Build(2, b.Version, b.Files, nil)
			if err != nil {
				t.Fatalf("build a schema 2 bundle: %v", err)
			}
			return out
		}},
		{"schema", ingressbundle.ReasonSchema, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			b.Manifest.Schema = 2
			return b
		}},
		{"checksum", ingressbundle.ReasonChecksum, false, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			return withFile(b, ingressbundle.DynamicFile, []byte(dynamicOf(t, b)+"# changed\n"))
		}},
		{"id", ingressbundle.ReasonID, false, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			b.ID = strings.Repeat("0", 64)
			return b
		}},
		{"path/absolute", ingressbundle.ReasonPath, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			b.Manifest.Files[0].Path = "/etc/passwd"
			return b
		}},
		{"path/dot-dot", ingressbundle.ReasonPath, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			b.Manifest.Files[0].Path = "certs/../dynamic/tierplan.yml"
			return b
		}},
		{"duplicate", ingressbundle.ReasonDuplicate, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			b.Manifest.Files = append(b.Manifest.Files, b.Manifest.Files[0])
			return b
		}},
		{"missing_file/manifest-entry-without-file", ingressbundle.ReasonMissingFile, false, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			return withoutFile(b, firstPath(t, b, ".crt"))
		}},
		{"missing_file/no-dynamic-file", ingressbundle.ReasonMissingFile, true, func(t *testing.T) ingressbundle.Bundle {
			l := newLeafFiles(t, host)
			b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 1},
				[]ingressbundle.File{{Path: l.certPath, Data: l.cert}, {Path: l.keyPath, Data: l.key}}, nil)
			if err != nil {
				t.Fatalf("build a bundle with no dynamic file: %v", err)
			}
			return b
		}},
		{"missing_entry", ingressbundle.ReasonMissingEntry, false, func(t *testing.T) ingressbundle.Bundle {
			return withFile(testBundle(t, host), "certs/extra.crt", []byte("x"))
		}},
		{"mode", ingressbundle.ReasonMode, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			for i := range b.Manifest.Files {
				if b.Manifest.Files[i].Path == ingressbundle.DynamicFile {
					b.Manifest.Files[i].Mode = ingressbundle.ModeKey
				}
			}
			return b
		}},
		{"step", ingressbundle.ReasonStep, false, func(t *testing.T) ingressbundle.Bundle {
			b := cloned(testBundle(t, host))
			for i := range b.Manifest.Files {
				if b.Manifest.Files[i].Path == ingressbundle.DynamicFile {
					b.Manifest.Files[i].Step = ingressbundle.StepCerts
				}
			}
			return b
		}},
		{"template_syntax", ingressbundle.ReasonTemplateSyntax, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(s string) string { return s + "# {{ env \"HOME\" }}\n" })
		}},
		{"yaml_invalid", ingressbundle.ReasonYAMLInvalid, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(string) string { return "tls: [unclosed\n" })
		}},
		{"tls_options/sniStrict-false", ingressbundle.ReasonTLSOptions, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(s string) string {
				return strings.Replace(s, "sniStrict: true", "sniStrict: false", 1)
			})
		}},
		{"tls_options/sniStrict-absent", ingressbundle.ReasonTLSOptions, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(s string) string {
				return strings.Replace(s, "      sniStrict: true\n", "", 1)
			})
		}},
		{"tls_options/default-store", ingressbundle.ReasonTLSOptions, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(s string) string {
				return strings.Replace(s, "  certificates:\n",
					"  stores:\n    default:\n      defaultCertificate:\n        certFile: "+abs("certs/a.crt")+"\n        keyFile: "+abs("certs/a.key")+"\n  certificates:\n", 1)
			})
		}},
		{"cert_path/relative", ingressbundle.ReasonCertPath, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			crt := firstPath(t, b, ".crt")
			return withDynamic(t, b, func(s string) string { return strings.Replace(s, abs(crt), crt, 1) })
		}},
		{"cert_path/outside-cert-root", ingressbundle.ReasonCertPath, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			crt := firstPath(t, b, ".crt")
			return withDynamic(t, b, func(s string) string { return strings.Replace(s, abs(crt), "/etc/ssl/certs/x.crt", 1) })
		}},
		{"cert_path/nested-name", ingressbundle.ReasonCertPath, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			crt := firstPath(t, b, ".crt")
			return withDynamic(t, b, func(s string) string {
				return strings.Replace(s, abs(crt), ingressbundle.CertRoot+"/sub/x.crt", 1)
			})
		}},
		{"cert_missing/certFile", ingressbundle.ReasonCertMissing, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, host)
			crt := firstPath(t, b, ".crt")
			return withDynamic(t, b, func(s string) string {
				return strings.Replace(s, abs(crt), ingressbundle.CertRoot+"/absent.crt", 1)
			})
		}},
		{"cert_missing/rootCAs", ingressbundle.ReasonCertMissing, true, func(t *testing.T) ingressbundle.Bundle {
			return withDynamic(t, testBundle(t, host), func(s string) string {
				return s + "  serversTransports:\n    t-user-x:\n      rootCAs:\n        - " + ingressbundle.CertRoot + "/absent-ca.crt\n"
			})
		}},
		{"key_mismatch", ingressbundle.ReasonKeyMismatch, true, func(t *testing.T) ingressbundle.Bundle {
			b := testBundle(t, "a.test.local", "b.test.local")
			var keys []string
			for _, f := range b.Files {
				if strings.HasSuffix(f.Path, ".key") {
					keys = append(keys, f.Path)
				}
			}
			if len(keys) != 2 {
				t.Fatalf("want two keys, have %v", keys)
			}
			const marker = "\x00swap\x00"
			return withDynamic(t, b, func(s string) string {
				s = strings.Replace(s, abs(keys[0]), marker, 1)
				s = strings.Replace(s, abs(keys[1]), abs(keys[0]), 1)
				return strings.Replace(s, marker, abs(keys[1]), 1)
			})
		}},
	}
}

func TestCheckRejects(t *testing.T) {
	rows := rejectRows()
	produced := map[string]bool{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			b := row.build(t)
			if row.verifies {
				if err := b.Verify(); err != nil {
					t.Fatalf("the row targets a check after Verify but Verify refuses it: %v", err)
				}
			}
			err := Check(b)
			var ve *ingressbundle.VerifyError
			if !errors.As(err, &ve) {
				t.Fatalf("Check = %v, want a *ingressbundle.VerifyError with reason %q", err, row.want)
			}
			if ve.Reason != row.want {
				t.Fatalf("Check refused with reason %q (%v), want %q", ve.Reason, err, row.want)
			}
			if !slices.Contains(ingressbundle.RejectReasons(), ve.Reason) {
				t.Fatalf("reason %q is not in RejectReasons", ve.Reason)
			}
			produced[ve.Reason] = true
		})
	}

	// Every reason the node can report is produced by some row, except the four that this table cannot
	// reach: three come from decoding the frame (TestReasonOf) and one from the store (23-08).
	notHere := []string{ingressbundle.ReasonTooLarge, ingressbundle.ReasonInstanceMismatch, ingressbundle.ReasonMalformed, ingressbundle.ReasonApplyFailed}
	for _, code := range ingressbundle.RejectReasons() {
		if slices.Contains(notHere, code) {
			continue
		}
		if !produced[code] {
			t.Errorf("no TestCheckRejects row produces the reason %q", code)
		}
	}
}

func TestCheckIgnoresStripPrefixPaths(t *testing.T) {
	dynamic := "tls:\n  options:\n    default:\n      sniStrict: true\n      minVersion: VersionTLS12\n" +
		"http:\n  middlewares:\n    m-strip-x:\n      stripPrefix:\n        prefixes:\n          - /x\n"
	b, err := ingressbundle.Build(ingressbundle.Schema1, ingressbundle.Version{Config: 1},
		[]ingressbundle.File{{Path: ingressbundle.DynamicFile, Data: []byte(dynamic)}}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := Check(b); err != nil {
		t.Fatalf("a stripPrefix prefix starting with / was taken for a certificate path: %v", err)
	}
}
