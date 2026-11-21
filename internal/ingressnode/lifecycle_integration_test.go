// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressnode

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// serveBundle1 starts the node with a first bundle and waits until it serves it and is ready. It returns the
// bundle and the PEM chain and key of its certificate.
func serveBundle1(t *testing.T, e *nodeEnv) (b ingressbundle.Bundle, chain, key []byte) {
	t.Helper()
	chain, key = e.ca.Leaf(t, routeHost)
	b = renderBundle(t, 1, chain, key)
	e.srv.Bundles.Set(e.env.ID, b)
	e.runIngress(t, "ingress-data")
	e.awaitServing(t, serialOf(t, chain))
	e.awaitReady(t)
	return b, chain, key
}

// TestRejectedBundleKeepsServing (D-15, PRX-04): a bundle that verifies but whose certificate and key do
// not belong together is rejected by the node with key_mismatch, the hub's row for the node says so, and the
// route keeps serving the previous certificate.
func TestRejectedBundleKeepsServing(t *testing.T) {
	e := startNodeEnv(t)
	b1, chain1, _ := serveBundle1(t, e)

	// The same bundle with the private key of another certificate: every checksum is right, so it verifies,
	// and only the node's own check can see the pair does not match.
	_, otherKey := e.ca.Leaf(t, routeHost)
	files := slices.Clone(b1.Files)
	swapped := false
	for i := range files {
		if strings.HasSuffix(files[i].Path, ".key") {
			files[i].Data = otherKey
			swapped = true
		}
	}
	if !swapped {
		t.Fatalf("the bundle has no key file to swap: %v", b1.Manifest.Files)
	}
	v := b1.Version
	v.Certs++
	bad, err := ingressbundle.Build(b1.Schema, v, files, b1.Manifest.Routes)
	if err != nil {
		t.Fatalf("build the bundle with the swapped key: %v", err)
	}
	if err := bad.Verify(); err != nil {
		t.Fatalf("the swapped bundle must verify, it failed: %v", err)
	}
	e.srv.Bundles.Set(e.env.ID, bad)

	testsupport.Poll(t, pollFor, "the hub to record bundle "+bad.ID+" as rejected", func() (bool, string) { // testsupport: external, the node answers over its own session
		nodes, err := e.srv.Service.ListNodes(t.Context(), e.env.ID)
		if err != nil || len(nodes) != 1 {
			return false, "no node row yet"
		}
		n := nodes[0]
		return n.RejectedBundleID == bad.ID && n.RejectedReason == "key_mismatch", "rejected " + n.RejectedBundleID + " (" + n.RejectedReason + ")"
	})

	// The old route still serves with the old certificate, and the node is ready.
	e.requireServing(t, serialOf(t, chain1))
	e.requireReady(t)
	if row := e.nodeRow(t); row.AppliedBundleID != b1.ID {
		t.Fatalf("the hub's row says bundle %q is applied, want the first bundle %q", row.AppliedBundleID, b1.ID)
	}
	logs := e.logs(t)
	if !strings.Contains(logs, "bundle rejected") || !strings.Contains(logs, "key_mismatch") {
		t.Fatalf("the container log has no `bundle rejected` line with key_mismatch:\n%s", tail(logs, 20))
	}
}

// TestTraefikChildRestartNoZombie (D-20, A7): when Traefik is killed the supervisor, PID 1 of the container,
// starts a new one, the route and readiness come back, and no process of the container is left a zombie.
func TestTraefikChildRestartNoZombie(t *testing.T) {
	e := startNodeEnv(t)
	_, chain1, _ := serveBundle1(t, e)

	old := e.traefikPID(t)
	if old == "" {
		t.Fatal("no Traefik process in the container once it serves")
	}
	// The node's PID namespace holds the container's processes, so the signal reaches Traefik from the node.
	e.sh(t, "kill -KILL "+old)

	testsupport.Poll(t, pollFor, "a new Traefik process", func() (bool, string) { // testsupport: external, the supervisor restarts the child on its own schedule
		out, err := e.node.Sh(t.Context(), "docker top "+ingressName+" -eo pid,comm")
		if err != nil {
			return false, err.Error()
		}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[1] == "traefik" && f[0] != old {
				return true, ""
			}
		}
		return false, "no traefik process other than " + old
	})
	e.awaitReady(t)
	e.awaitServing(t, serialOf(t, chain1))

	if now := e.traefikPID(t); now == "" || now == old {
		t.Fatalf("Traefik's pid is %q after the kill, want a new one (was %s)", now, old)
	}
	procs := e.processes(t)
	if len(procs) < 2 { // the supervisor and Traefik at least, or the zombie check below checks nothing
		t.Fatalf("docker top lists %d processes of the container, want at least 2: %v", len(procs), procs)
	}
	for _, p := range procs {
		stat, err := e.node.Sh(t.Context(), "cat /proc/"+p.pid+"/stat 2>/dev/null || true")
		if err != nil {
			t.Fatalf("read the state of process %s (%s): %v", p.pid, p.comm, err)
		}
		// The state is the first field after the command name, which is in parentheses and may hold any
		// character, so it is read after the last closing parenthesis.
		i := strings.LastIndex(stat, ")")
		if i < 0 {
			continue // the process ended between the listing and the read
		}
		if f := strings.Fields(stat[i+1:]); len(f) > 0 && f[0] == "Z" {
			t.Fatalf("process %s (%s) is a zombie", p.pid, p.comm)
		}
	}
}

// TestRevokedKeepsServing (D-35): revoking the node closes its session, and the node logs that it is revoked
// and keeps serving what it has. That it stops dialing is proven on a fake clock by the supervisor's own
// tests; this test does not wait out a backoff window.
func TestRevokedKeepsServing(t *testing.T) {
	e := startNodeEnv(t)
	_, chain1, _ := serveBundle1(t, e)

	row := e.nodeRow(t)
	if _, err := e.srv.Service.Revoke(t.Context(), audit.Actor{Type: audit.ActorUser, ID: "u-admin", Name: "admin"},
		e.env.ID, row.ID, ingresshub.RevokeOptions{Reason: "integration test"}, "203.0.113.9", "ingressnode"); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	testsupport.Poll(t, pollFor, "the node to log that it is revoked", func() (bool, string) { // testsupport: external, the node reacts to the close on its own schedule
		out, err := e.node.Sh(t.Context(), "docker logs "+ingressName+" 2>&1")
		if err != nil {
			return false, err.Error()
		}
		return strings.Contains(out, "ingress node revoked"), "no revoked line yet"
	})

	if got := e.nodeRow(t); got.RevokedAt.IsZero() || got.Status != "revoked" {
		t.Fatalf("the hub's row is status %q revoked at %v, want revoked", got.Status, got.RevokedAt)
	}
	e.requireServing(t, serialOf(t, chain1))
	e.requireReady(t)
	if running := strings.TrimSpace(e.sh(t, "docker inspect -f '{{.State.Running}}' "+ingressName)); running != "true" {
		t.Fatalf("the container is running=%s after the revoke, want true", running)
	}
}

// tail is the last n lines of s, for a failure message.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
