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

// noCacheLine is the supervisor's decision on a volume with no verified bundle, and reconnectLine is what the
// channel loop logs for each failed attempt to reach TierPlan.
const (
	noCacheLine   = "no verified cached bundle: Traefik not started"
	reconnectLine = "the session ended; reconnecting"
)

// TestNoCacheNoTierPlan (D-13, PRX-05, success criterion 5): a node with a new volume and a TierPlan it
// cannot reach has nothing to serve, so it serves nothing and says so: readiness 503 with no body, the proxy
// ports closed, no Traefik, and a health status of starting, not a failure and not a healthy node.
//
// The readiness listener opens first, so an answer from it proves nothing about the decision that follows. The
// measurements wait for the supervisor's own decision line and for the channel loop's first failed attempt,
// which are logged after the volume, the cache and the start-up order have all been settled, and they are
// taken again after a second failed attempt, a full backoff later. A Traefik that a regression started late
// would have opened its ports by then.
func TestNoCacheNoTierPlan(t *testing.T) {
	e := startNodeEnv(t)
	e.srv.Stop() // the address answers nothing from here on
	e.runIngress(t, "empty-data")

	awaitLogs := func(what string, ready func(logs string) bool) {
		t.Helper()
		testsupport.Poll(t, pollFor, what, func() (bool, string) { // testsupport: external, the image logs on its own schedule
			logs := e.logs(t)
			if ready(logs) {
				return true, ""
			}
			return false, tail(logs, 5)
		})
	}
	awaitLogs("the supervisor's decision to start no Traefik and the first failed attempt to reach TierPlan", func(logs string) bool {
		return strings.Contains(logs, noCacheLine) && strings.Contains(logs, reconnectLine)
	})
	checkNothingServed(t, e)

	// A second failed attempt is a whole backoff later, by the node's own clock, with no sleep in the test.
	awaitLogs("a second failed attempt to reach TierPlan", func(logs string) bool {
		return strings.Count(logs, reconnectLine) >= 2
	})
	checkNothingServed(t, e)
}

// checkNothingServed fails unless the node serves nothing: readiness 503 with no body, the proxy ports
// refused, a health status of starting and no Traefik process.
func checkNothingServed(t *testing.T, e *nodeEnv) {
	t.Helper()
	status, body, err := e.readyz()
	if err != nil {
		t.Fatalf("/readyz did not answer: %v", err)
	}
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
