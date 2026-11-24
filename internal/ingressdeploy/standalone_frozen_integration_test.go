// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// proxyName is the container of the proxy's compose project on a standalone host.
const proxyName = ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1"

// tokenPath is where the proxy reads its join token from.
const tokenPath = "/run/secrets/tierplan-ingress-token"

// TestStandaloneFrozenNotBroken is D-19 and SC 3 on real engines: the data plane outlives the control plane. With ingress on and
// a routed stack served on the host's 443, TierPlan's side is stopped (the ingress hub, the deployer's loops and the tunnel);
// the route still answers; after a restart of the proxy container and after a restart of the host's engine (the stand-in for a
// reboot) the proxy runs again by its restart policy, the join token file is still in it, and the route answers with the test
// CA's certificate while nothing holds the hub. The certificate comes from the proxy's cache volume, never from TierPlan.
func TestStandaloneFrozenNotBroken(t *testing.T) {
	e := startHostEnv(t, false)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()

	host := "frozen." + testDomain
	_, res, job := e.deployStack("frozen", "services:\n"+e.routedService("web", webBody("frozen"), host)+"    restart: unless-stopped\n")
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the deploy ended %+v", res)
	}
	e.uploadCertificate(host)
	if page := e.awaitServed(host); !strings.Contains(page, webBody("frozen")) {
		t.Fatalf("%s answered %q, want the stack's web (%s)", host, strings.TrimSpace(page), webBody("frozen"))
	}

	e.stopTierPlan()
	e.requireFrozenServing(host, "with TierPlan stopped")

	e.sh("docker restart " + proxyName)
	e.awaitProxyRunning()
	e.requireFrozenServing(host, "after a restart of the proxy container")

	e.restartHost()
	e.awaitProxyRunning()
	e.requireFrozenServing(host, "after a restart of the host's engine")

	// What the proxy says of itself with no control plane to talk to, for the record: no token is ever in these lines.
	t.Logf("the proxy's last log lines:\n%s", strings.TrimSpace(e.sh("docker logs --tail 15 "+proxyName+" 2>&1")))
}

// awaitProxyRunning waits until the proxy container is running, by the restart policy and nothing of TierPlan's.
func (e *hostEnv) awaitProxyRunning() {
	t := e.t
	t.Helper()
	dockertest.Wait(t, enrolBound, "the proxy container to be running", func() (bool, string) {
		out, err := e.host.Sh(testsupport.Context(t), "docker inspect --format '{{.State.Status}}' "+proxyName)
		state := strings.TrimSpace(out)
		return err == nil && state == "running", state
	})
}

// requireFrozenServing asserts, while TierPlan is stopped, that the join token file is still in the proxy and that the route
// answers over the test CA's certificate with the stack's own page.
func (e *hostEnv) requireFrozenServing(host, when string) {
	t := e.t
	t.Helper()
	// The proxy image has no shell and no test, so the file is read from the archive the engine makes of it and only its size is
	// counted: the token itself is never printed.
	dockertest.Wait(t, enrolBound, "the proxy's join token file to be there "+when, func() (bool, string) {
		out, err := e.host.Sh(testsupport.Context(t), "docker cp "+proxyName+":"+tokenPath+" - | tar -xO -f - | wc -c")
		size := strings.TrimSpace(out)
		return err == nil && size != "" && size != "0", "size " + size
	})
	if page := e.awaitServed(host); !strings.Contains(page, webBody("frozen")) {
		t.Errorf("%s answered %q %s, want the stack's web (%s)", host, strings.TrimSpace(page), when, webBody("frozen"))
	}
}
