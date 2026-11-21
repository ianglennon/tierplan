// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// dockerDuration formats d the way a Dockerfile option takes it: whole minutes as 10m, else whole seconds
// as 30s.
func dockerDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

// TestDockerfileHealthcheckMatchesConstants fails when the image's HEALTHCHECK drifts from the values the
// protocol package holds, which Phase 24 sets on the swarm service (D-18), or when the image stops being
// non-root with a SIGTERM stop signal (D-05, D-07).
func TestDockerfileHealthcheckMatchesConstants(t *testing.T) {
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	text := string(raw)

	var health string
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, strings.TrimSpace(line))
		if strings.HasPrefix(strings.TrimSpace(line), "HEALTHCHECK ") {
			health = strings.TrimSpace(line)
		}
	}
	if health == "" {
		t.Fatal("the Dockerfile has no HEALTHCHECK line")
	}

	for _, c := range []struct{ flag, want string }{
		{"interval", dockerDuration(ingressbundle.HealthcheckInterval)},
		{"timeout", dockerDuration(ingressbundle.HealthcheckTimeout)},
		{"start-period", dockerDuration(ingressbundle.HealthcheckStartPeriod)},
		{"retries", strconv.Itoa(ingressbundle.HealthcheckRetries)},
	} {
		m := regexp.MustCompile(`--` + c.flag + `=(\S+)`).FindStringSubmatch(health)
		if m == nil {
			t.Errorf("the HEALTHCHECK has no --%s", c.flag)
			continue
		}
		if m[1] != c.want {
			t.Errorf("HEALTHCHECK --%s=%s, the ingressbundle constant is %s", c.flag, m[1], c.want)
		}
	}

	wantCmd := `CMD ["/usr/local/bin/tierplan-ingress", "healthcheck"]`
	if !strings.HasSuffix(health, wantCmd) {
		t.Errorf("the HEALTHCHECK command is not %s: %s", wantCmd, health)
	}

	for _, want := range []string{"USER 1000:1000", "STOPSIGNAL SIGTERM"} {
		found := false
		for _, l := range lines {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the Dockerfile has no %q line", want)
		}
	}
}
