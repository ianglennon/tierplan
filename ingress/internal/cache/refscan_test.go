// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// nodeKey is a file Traefik can read from the node's volume that a bundle must never make it load as a
// certificate.
var nodeKey = abs("state/node.key")

// caseRows are dynamic files that Traefik reads one way and a case-sensitive struct decode reads another
// (WR-A-05). Each holds the canonical, valid form and, beside it, a spelling Traefik folds into the same
// field. Before the fix every row passed Check.
func caseRows() []struct {
	name   string
	want   string
	mutate func(b ingressbundle.Bundle, s string) string
} {
	return []struct {
		name   string
		want   string
		mutate func(b ingressbundle.Bundle, s string) string
	}{
		{"CertFile beside certFile", ingressbundle.ReasonCertPath, func(b ingressbundle.Bundle, s string) string {
			key := abs(firstPathOf(b, ".key"))
			return strings.Replace(s, "keyFile: "+key+"\n", "keyFile: "+key+"\n      CertFile: "+nodeKey+"\n", 1)
		}},
		{"KEYFILE beside keyFile", ingressbundle.ReasonCertPath, func(b ingressbundle.Bundle, s string) string {
			key := abs(firstPathOf(b, ".key"))
			return strings.Replace(s, "keyFile: "+key+"\n", "keyFile: "+key+"\n      KEYFILE: "+nodeKey+"\n", 1)
		}},
		{"ROOTCAS in a servers transport", ingressbundle.ReasonCertPath, func(_ ingressbundle.Bundle, s string) string {
			return s + "  serversTransports:\n    t-x:\n      ROOTCAS:\n        - /etc/ssl/certs/ca.pem\n"
		}},
		{"rootcas naming a file the bundle lacks", ingressbundle.ReasonCertMissing, func(_ ingressbundle.Bundle, s string) string {
			return s + "  serversTransports:\n    t-x:\n      rootcas:\n        - " + ingressbundle.CertRoot + "/absent-ca.crt\n"
		}},
		{"caFiles of a client CA outside certs", ingressbundle.ReasonCertPath, func(_ ingressbundle.Bundle, s string) string {
			return strings.Replace(s, "      minVersion: VersionTLS12\n",
				"      minVersion: VersionTLS12\n      clientAuth:\n        caFiles:\n          - /etc/passwd\n", 1)
		}},
		{"a servers transport client certificate outside certs", ingressbundle.ReasonCertPath, func(_ ingressbundle.Bundle, s string) string {
			return s + "  serversTransports:\n    t-x:\n      certificates:\n        - certFile: " + nodeKey + "\n          keyFile: " + nodeKey + "\n"
		}},
		{"a forwardAuth tls block", ingressbundle.ReasonCertPath, func(_ ingressbundle.Bundle, s string) string {
			return s + "  middlewares:\n    m-auth:\n      forwardAuth:\n        address: http://auth:9091\n        tls:\n          ca: " + nodeKey + "\n"
		}},
		{"any field that points into the node root", ingressbundle.ReasonCertPath, func(_ ingressbundle.Bundle, s string) string {
			return s + "  middlewares:\n    m-x:\n      headers:\n        customRequestHeaders:\n          X-Where: " + nodeKey + "\n"
		}},
		{"a file field that came in by a merge key", ingressbundle.ReasonCertPath, func(b ingressbundle.Bundle, s string) string {
			key := abs(firstPathOf(b, ".key"))
			return strings.Replace(s, "keyFile: "+key+"\n", "keyFile: "+key+"\n      <<: {CertFile: "+nodeKey+"}\n", 1)
		}},
		{"SniStrict beside sniStrict", ingressbundle.ReasonYAMLInvalid, func(_ ingressbundle.Bundle, s string) string {
			return strings.Replace(s, "      sniStrict: true\n", "      sniStrict: true\n      SniStrict: false\n", 1)
		}},
		{"Stores beside the checked tls keys", ingressbundle.ReasonYAMLInvalid, func(b ingressbundle.Bundle, s string) string {
			crt, key := abs(firstPathOf(b, ".crt")), abs(firstPathOf(b, ".key"))
			return strings.Replace(s, "  certificates:\n",
				"  Stores:\n    default:\n      defaultCertificate:\n        certFile: "+crt+"\n        keyFile: "+key+"\n  certificates:\n", 1)
		}},
		{"Options beside options", ingressbundle.ReasonYAMLInvalid, func(_ ingressbundle.Bundle, s string) string {
			return strings.Replace(s, "  certificates:\n", "  Options:\n    default:\n      sniStrict: false\n  certificates:\n", 1)
		}},
		{"TLS beside tls", ingressbundle.ReasonYAMLInvalid, func(_ ingressbundle.Bundle, s string) string {
			return s + "TLS:\n  options:\n    default:\n      sniStrict: false\n"
		}},
		{"SERVERSTRANSPORTS beside the http keys", ingressbundle.ReasonYAMLInvalid, func(_ ingressbundle.Bundle, s string) string {
			return s + "  SERVERSTRANSPORTS:\n    t-x:\n      rootCAs:\n        - /etc/ssl/certs/ca.pem\n"
		}},
	}
}

func firstPathOf(b ingressbundle.Bundle, suffix string) string {
	for _, f := range b.Files {
		if strings.HasSuffix(f.Path, suffix) {
			return f.Path
		}
	}
	return ""
}

// TestCheckReadsTheYAMLTheWayTraefikDoes is WR-A-05: a spelling of a checked field that Traefik folds into
// it, or a file field Check did not read at all, no longer gets past Check.
func TestCheckReadsTheYAMLTheWayTraefikDoes(t *testing.T) {
	for _, row := range caseRows() {
		t.Run(row.name, func(t *testing.T) {
			b := testBundle(t, "a.test.local")
			b = withDynamic(t, b, func(s string) string { return row.mutate(b, s) })
			if err := b.Verify(); err != nil {
				t.Fatalf("the row must pass Verify so the check under test is reached: %v", err)
			}
			err := Check(b)
			var ve *ingressbundle.VerifyError
			if !errors.As(err, &ve) {
				t.Fatalf("Check = %v, want a *ingressbundle.VerifyError with reason %q", err, row.want)
			}
			if ve.Reason != row.want {
				t.Fatalf("Check refused with reason %q (%v), want %q", ve.Reason, err, row.want)
			}
			if strings.Contains(ve.Path, "node.key") || strings.Contains(err.Error(), "/etc/") {
				t.Errorf("the failure carries the raw reference: %q", err)
			}
		})
	}
}

// TestCheckStillAcceptsWhatTheScanReads is the other side of WR-A-05: a client CA that is a certificate of
// the bundle, a stripPrefix and a transport with a CA of the bundle's own are all valid, so the stricter
// scan refuses nothing a renderer writes.
func TestCheckStillAcceptsWhatTheScanReads(t *testing.T) {
	b := testBundle(t, "a.test.local")
	crt := abs(firstPathOf(b, ".crt"))
	b = withDynamic(t, b, func(s string) string {
		s = strings.Replace(s, "      minVersion: VersionTLS12\n",
			"      minVersion: VersionTLS12\n      clientAuth:\n        caFiles:\n          - "+crt+"\n", 1)
		return s + "  serversTransports:\n    t-x:\n      rootCAs:\n        - " + crt + "\n" +
			"  middlewares:\n    m-strip-x:\n      stripPrefix:\n        prefixes:\n          - /x\n"
	})
	if err := Check(b); err != nil {
		t.Fatalf("Check refused a bundle whose every file reference is one of its own certificates: %v", err)
	}
}

// TestRefScanAcceptsRendererGoldens proves the scans against what the renderer writes: in every golden the
// keys Check reads are spelled canonically, and the files the scan finds are exactly the certs/ files of
// the golden's manifest.
func TestRefScanAcceptsRendererGoldens(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(goldenDir, "*", "tierplan.yml"))
	if err != nil || len(dirs) < 8 {
		t.Fatalf("found %d goldens under %s (%v), want at least the eight Phase 21 goldens", len(dirs), goldenDir, err)
	}
	for _, path := range dirs {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			raw, err := os.ReadFile(path) //nolint:gosec // G304: a fixture file in a Phase 21 golden directory found by Glob
			if err != nil {
				t.Fatalf("read the golden: %v", err)
			}
			var tree yaml.Node
			if err := yaml.Unmarshal(raw, &tree); err != nil {
				t.Fatalf("parse the golden: %v", err)
			}
			if !keysCanonical(&tree) {
				t.Error("the scan took a key the renderer wrote for a misspelling")
			}
			scanned, ok := fileRefs(&tree)
			if !ok {
				t.Fatal("the scan gave up on a golden")
			}
			typed, err := certRefs(raw)
			if err != nil {
				t.Fatalf("certRefs: %v", err)
			}
			slices.Sort(typed)
			if want := slices.Compact(typed); !slices.Equal(scanned, want) {
				t.Errorf("the scan found %v, the typed read finds %v", scanned, want)
			}
		})
	}
}

// TestRefScanGivesUpOnADeepTree: nesting past the scan's depth is not read and not trusted.
func TestRefScanGivesUpOnADeepTree(t *testing.T) {
	deep := strings.Repeat("[", refscanDepth+20) + strings.Repeat("]", refscanDepth+20)
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte("x: "+deep+"\n"), &tree); err != nil {
		t.Skipf("the YAML parser itself refuses this nesting: %v", err)
	}
	if _, ok := fileRefs(&tree); ok {
		t.Fatal("fileRefs read a tree nested past its depth limit")
	}
}
