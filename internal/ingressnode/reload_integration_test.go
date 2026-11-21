// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressnode

import (
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestCertReloadWithoutRestart is the tracer of the phase (D-29): the shipped image enrols with the real
// hub, serves a bundle on 8443 to a real backend, and takes a new certificate without restarting Traefik
// (RTE-04). A connection opened before the change still answers after it, and a new handshake gets the new
// leaf.
func TestCertReloadWithoutRestart(t *testing.T) {
	e := startNodeEnv(t)

	chain1, key1 := e.ca.Leaf(t, routeHost)
	b1 := renderBundle(t, 1, chain1, key1)
	e.srv.Bundles.Set(e.env.ID, b1)
	e.runIngress(t, "ingress-data")

	e.awaitServing(t, serialOf(t, chain1))
	e.awaitReady(t)
	pid := e.traefikPID(t)
	if pid == "" {
		t.Fatal("no Traefik process in the container once it serves")
	}

	// A connection opened before the change, with one request completed on it.
	old, err := e.open()
	if err != nil {
		t.Fatalf("open a connection before the change: %v", err)
	}
	defer old.close()
	if status, _, err := old.get(); err != nil || status != http.StatusOK {
		t.Fatalf("the first request on the old connection: status %d, error %v", status, err)
	}
	if got, want := old.serial(), serialOf(t, chain1); got != want {
		t.Fatalf("the old connection was served serial %s, want %s", got, want)
	}

	// A new certificate for the same host, delivered as a new bundle.
	chain2, key2 := e.ca.Leaf(t, routeHost)
	b2 := renderBundle(t, 2, chain2, key2)
	if b2.ID == b1.ID {
		t.Fatal("the second bundle has the first one's ID")
	}
	e.srv.Bundles.Set(e.env.ID, b2)

	e.awaitServing(t, serialOf(t, chain2))

	// The connection from before the change answers a second request, and Traefik was never restarted.
	if status, _, err := old.get(); err != nil || status != http.StatusOK {
		t.Fatalf("the second request on the old connection: status %d, error %v", status, err)
	}
	if got := e.traefikPID(t); got != pid {
		t.Fatalf("Traefik's pid changed from %s to %s: it was restarted", pid, got)
	}
	e.requireReady(t)

	testsupport.Poll(t, pollFor, "the hub to record bundle 2 as applied", func() (bool, string) { // testsupport: external, the node reports over its own session
		nodes, err := e.srv.Service.ListNodes(t.Context(), e.env.ID)
		if err != nil || len(nodes) != 1 {
			return false, "no node row yet"
		}
		return nodes[0].AppliedBundleID == b2.ID, "applied " + nodes[0].AppliedBundleID
	})
}
