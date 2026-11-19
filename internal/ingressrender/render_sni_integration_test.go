// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressrender

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Rendered-bundle cases of the pinned Traefik (21-09, D-21, D-22, D-30). The S1 tests prove what the
// proxy does with hand-written bundles; these prove the renderer's own output uses that behaviour
// correctly. Every test runs one real Traefik on an --internal network, probes it by container IP and
// issues certificates from a throwaway CA, never from an ACME server. The proxy reloads its file
// provider asynchronously, so each observation of a change is a bounded poll, never a sleep.

const (
	// rAppHost is the hostname most of these tests serve.
	rAppHost = "app.test.local"
)

// TestTraefikRenderedOneHostTracer is the tracer of RTE-01: a bundle Render produced for one swarm
// route, applied certificates first and the YAML last, is served by the pinned Traefik started with
// Static(): the known SNI gets the rendered leaf, a GET reaches the backend aliased shop_web, and the
// proxy says it does not check for a new version.
func TestTraefikRenderedOneHostTracer(t *testing.T) {
	traefiktest.SkipUnlessEngineLeg(t)
	env := traefiktest.NewEnv(t)
	ca := traefiktest.NewCA(t)
	env.StartWhoami(t, traefiktest.WhoamiOptions{Alias: "shop_web", Hostname: "shop-web"})

	res := renderOrFail(t, Input{
		Snapshot:     snapshotOf(swarmRoute(rAppHost, "/", "shop", "web", 80)),
		Certs:        servable(t, ca, rAppHost),
		CertsVersion: 1,
	})
	if len(res.Skipped) != 0 {
		t.Fatalf("the one route was skipped: %+v", res.Skipped)
	}
	applyBundle(t, env, res.Bundle)
	tr := env.StartTraefik(t, traefiktest.TraefikOptions{Static: Static()})

	var peerCN string
	testsupport.Poll(t, testsupport.Deadline, "a handshake for "+rAppHost+" to succeed", func() (bool, string) { // testsupport: external, Traefik reloads its file provider asynchronously
		st, err := tr.Handshake(rAppHost, ca.Pool())
		if err != nil {
			return false, err.Error()
		}
		if len(st.PeerCertificates) == 0 {
			return false, "no peer certificate"
		}
		peerCN = st.PeerCertificates[0].Subject.CommonName
		return true, ""
	})
	// The handshake verified against the test CA, so the leaf is one this test issued; its subject
	// says it is the one for this hostname and not Traefik's own.
	if peerCN != rAppHost {
		t.Errorf("the peer certificate's subject is %q, want %q", peerCN, rAppHost)
	}

	status, body, _ := tr.Get(t, ca.Pool(), rAppHost, "/shop", nil)
	if status != 200 || !strings.Contains(body, "Hostname: shop-web") {
		t.Errorf("GET https://%s/shop answered %d, want 200 from the backend shop-web; body:\n%s", rAppHost, status, body)
	}

	// The version check is decided at startup; the poll is for the log stream catching up.
	var logs string
	testsupport.Poll(t, testsupport.Deadline, "Traefik's startup log to hold the version-check line", func() (bool, string) { // testsupport: external, the container's log stream
		logs = tr.Logs(t)
		return strings.Contains(logs, "Version check is disabled."), "no version-check line yet"
	})
	if strings.Contains(logs, "Version check is enabled.") {
		t.Error("Traefik logged `Version check is enabled.`")
	}
}
