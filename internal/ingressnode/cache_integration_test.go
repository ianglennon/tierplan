// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressnode

import (
	"net"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// cachedLine is what the supervisor logs when it starts Traefik from a verified cache.
const cachedLine = "serving the cached bundle"

// TestServesFromCache (D-13, PRX-03, success criterion 3): with TierPlan stopped, a container that is
// restarted and then a new container on the same volume, standing in for a host reboot, each serve the route
// and answer readiness 200 from the cache alone.
func TestServesFromCache(t *testing.T) {
	e := startNodeEnv(t)
	_, chain1, _ := serveBundle1(t, e)
	serial := serialOf(t, chain1)

	// The first start had no cache, so it did not serve from one: the line below is the restart's.
	if logs := e.logs(t); strings.Contains(logs, cachedLine) {
		t.Fatalf("the first start logged %q though it had no cache:\n%s", cachedLine, tail(logs, 20))
	}

	e.srv.Stop() // TierPlan is gone: nothing answers at the address the node dials
	e.sh(t, "docker restart "+ingressName)
	e.awaitServing(t, serial)
	e.awaitReady(t)
	if logs := e.logs(t); !strings.Contains(logs, cachedLine) {
		t.Fatalf("the restarted container did not log %q:\n%s", cachedLine, tail(logs, 20))
	}

	// A new container on the same volume, as after a host reboot.
	e.sh(t, "docker rm -f "+ingressName)
	e.runIngress(t, "ingress-data")
	e.awaitServing(t, serial)
	e.awaitReady(t)
	if logs := e.logs(t); !strings.Contains(logs, cachedLine) {
		t.Fatalf("the new container on the old volume did not log %q:\n%s", cachedLine, tail(logs, 20))
	}
}

// TestNoCacheNoTierPlan (D-13, PRX-05, success criterion 5): a node with a new volume and a TierPlan it
// cannot reach has nothing to serve, so it serves nothing and says so: readiness 503 with no body, the proxy
// ports closed, no Traefik, and a health status of starting, not a failure and not a healthy node.
func TestNoCacheNoTierPlan(t *testing.T) {
	e := startNodeEnv(t)
	e.srv.Stop() // the address answers nothing from here on
	e.runIngress(t, "empty-data")

	var status int
	var body string
	testsupport.Poll(t, pollFor, "/readyz to answer", func() (bool, string) { // testsupport: external, the image starts on its own schedule
		s, b, err := e.readyz()
		if err != nil {
			return false, err.Error()
		}
		status, body = s, b
		return true, ""
	})
	if status != 503 || body != "" {
		t.Fatalf("/readyz answered %d with a body of %d bytes, want 503 and none", status, len(body))
	}

	for _, port := range []string{"8080", "8443"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(e.node.IP, port), testsupport.Deadline)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("a connection to port %s succeeded, want it refused: no route is served", port)
		}
	}

	if health := strings.TrimSpace(e.sh(t, "docker inspect -f '{{.State.Health.Status}}' "+ingressName)); health != "starting" {
		t.Fatalf("the container's health status is %q, want starting", health)
	}
	if pid := e.traefikPID(t); pid != "" {
		t.Fatalf("a Traefik process (pid %s) runs on a node with nothing to serve", pid)
	}
}
