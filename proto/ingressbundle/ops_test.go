// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	"regexp"
	"slices"
	"testing"
	"time"
)

// TestStopTimes pins the numbers the supervisor, the image's health check and Phase 24's service spec
// share (D-06, D-17, D-18, D-21): each stage of a stop gets longer than the one it waits for, and a
// health probe finishes before the next one starts.
func TestStopTimes(t *testing.T) {
	if !(TraefikGraceTimeout < SupervisorStopWait && SupervisorStopWait < StopGracePeriod) {
		t.Errorf("want TraefikGraceTimeout (%v) < SupervisorStopWait (%v) < StopGracePeriod (%v)",
			TraefikGraceTimeout, SupervisorStopWait, StopGracePeriod)
	}
	if HealthcheckTimeout >= HealthcheckInterval {
		t.Errorf("HealthcheckTimeout %v must be shorter than HealthcheckInterval %v", HealthcheckTimeout, HealthcheckInterval)
	}
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"TraefikGraceTimeout", TraefikGraceTimeout, 10 * time.Second},
		{"SupervisorStopWait", SupervisorStopWait, 20 * time.Second},
		{"StopGracePeriod", StopGracePeriod, 30 * time.Second},
		{"HealthcheckInterval", HealthcheckInterval, 30 * time.Second},
		{"HealthcheckTimeout", HealthcheckTimeout, 5 * time.Second},
		{"HealthcheckStartPeriod", HealthcheckStartPeriod, 10 * time.Minute},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if HealthcheckRetries != 3 {
		t.Errorf("HealthcheckRetries = %d, want 3", HealthcheckRetries)
	}
	if ReadyPort != 8083 || PingPort != 8082 {
		t.Errorf("ReadyPort, PingPort = %d, %d, want 8083, 8082", ReadyPort, PingPort)
	}
	if ReadyPath != "/readyz" || PingPath != "/ping" {
		t.Errorf("ReadyPath, PingPath = %q, %q, want /readyz, /ping", ReadyPath, PingPath)
	}
	// The unprivileged ports the proxy uses must be distinct and above 1024 (D-06).
	ports := []int{ReadyPort, PingPort, 8080, 8443}
	seen := map[int]bool{}
	for _, p := range ports {
		if p <= 1024 {
			t.Errorf("port %d is privileged", p)
		}
		if seen[p] {
			t.Errorf("port %d is used twice in %v", p, ports)
		}
		seen[p] = true
	}
}

// maxReasonLen mirrors ingresswire.MaxReasonLen (512). This package must not import the wire package, so
// the bound is repeated here; the wire package's frozen test pins its own copy.
const maxReasonLen = 512

func TestRejectReasons(t *testing.T) {
	all := RejectReasons()
	if len(all) != 20 {
		t.Fatalf("RejectReasons has %d entries, want 20: %v", len(all), all)
	}
	if !slices.IsSorted(all) {
		t.Errorf("RejectReasons is not sorted: %v", all)
	}
	shape := regexp.MustCompile(`^[a-z][a-z_]*$`)
	seen := map[string]bool{}
	for _, r := range all {
		if seen[r] {
			t.Errorf("reason %q appears twice", r)
		}
		seen[r] = true
		if !shape.MatchString(r) {
			t.Errorf("reason %q is not lower snake case", r)
		}
		if len(r) > maxReasonLen {
			t.Errorf("reason %q is longer than %d bytes", r, maxReasonLen)
		}
	}
	for _, want := range []string{
		// The nine the verifier already used.
		ReasonChecksum, ReasonMissingFile, ReasonMissingEntry, ReasonID, ReasonPath,
		ReasonDuplicate, ReasonSchema, ReasonMode, ReasonStep,
		// The eleven the supervisor adds (D-37).
		ReasonInstanceMismatch, ReasonTooLarge, ReasonSchemaUnsupported, ReasonMalformed,
		ReasonYAMLInvalid, ReasonTemplateSyntax, ReasonCertMissing, ReasonCertPath,
		ReasonKeyMismatch, ReasonTLSOptions, ReasonApplyFailed,
	} {
		if !seen[want] {
			t.Errorf("RejectReasons is missing %q", want)
		}
	}

	all[0] = "mutated"
	if again := RejectReasons(); slices.Contains(again, "mutated") {
		t.Error("RejectReasons returned a shared slice: a caller's change reached the next caller")
	}
}

func TestSupportedSchemas(t *testing.T) {
	if got := SupportedSchemas(); !slices.Equal(got, []int{Schema1}) {
		t.Fatalf("SupportedSchemas = %v, want [%d]", got, Schema1)
	}
	s := SupportedSchemas()
	s[0] = 99
	if got := SupportedSchemas(); !slices.Equal(got, []int{Schema1}) {
		t.Fatalf("a caller's change reached the next call: %v", got)
	}
}
