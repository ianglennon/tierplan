// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// stubContainer is the plain container that stands in for TierPlan on a standalone host.
const stubContainer = "tp-selfstub"

// withHostSelf gives the deployer what the server gives it on its own standalone host: a read of TierPlan's own container
// (the stub, by name, so a recreated stub is found as a recreated TierPlan is) and the connect and disconnect of that container,
// all bound to the host's engine through the environment's own client, as the server's are bound to its local socket.
func withHostSelf(e *hostEnv, d *ingressdeploy.Deps) {
	d.Self = func(ctx context.Context) (ingressdeploy.SelfFacts, error) {
		var facts ingressdeploy.SelfFacts
		err := e.agg.OnNode(ctx, e.envID, e.nodeID, func(c *dockerx.Client) error {
			detail, _, err := c.ContainerDetail(ctx, stubContainer)
			if err != nil {
				return err
			}
			facts = selfFactsOfStub(detail)
			return nil
		})
		return facts, err
	}
	d.SelfConnect = func(ctx context.Context, networkID, alias string) error {
		return e.agg.WriteOnNode(ctx, e.envID, e.nodeID, func(c *dockerx.Client) error {
			return c.NetworkConnect(ctx, networkID, stubContainer, dockerx.EndpointSpec{Alias: alias})
		})
	}
	d.SelfDisconnect = func(ctx context.Context, networkID string) error {
		return e.agg.WriteOnNode(ctx, e.envID, e.nodeID, func(c *dockerx.Client) error {
			return c.NetworkDisconnect(ctx, networkID, stubContainer)
		})
	}
}

// runStub starts the stub container on the host, on the default bridge only.
func (e *hostEnv) runStub(image string) {
	e.t.Helper()
	e.sh("docker run -d --name " + stubContainer + " -v " + stubTLSDir + ":/tls:ro " + image + " " + stubCommand())
}

// selfAttachedRows is the number of ingress.self_network_attached audit rows.
func (e *hostEnv) selfAttachedRows() int {
	e.t.Helper()
	var n int
	if err := e.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressSelfNetworkAttached).Scan(&n); err != nil {
		e.t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// passUntil takes ensure passes until cond holds, as swarmEnv.passUntil does.
func (e *hostEnv) passUntil(what string, cond func() (bool, string)) {
	e.t.Helper()
	dockertest.Wait(e.t, selfBound, what, func() (bool, string) {
		e.deploy.Round(testsupport.Context(e.t))
		return cond()
	})
}

// stubOnSelf is whether the stub is on tierplan-self with the alias the proxy reaches TierPlan by, and what it is on.
func (e *hostEnv) stubOnSelf() (bool, string) {
	e.t.Helper()
	nets := e.networksOf(stubContainer)
	if !slices.Contains(nets, ingress.SelfNetworkName) {
		return false, fmt.Sprintf("stub networks %v", nets)
	}
	aliases := e.aliasesOn(stubContainer, ingress.SelfNetworkName)
	return slices.Contains(aliases, ingressdeploy.SelfBackendAlias),
		fmt.Sprintf("stub networks %v, aliases on %s %v", nets, ingress.SelfNetworkName, aliases)
}

// proxyOnSelfNet is whether the proxy container is on tierplan-self. A proxy the pass is recreating reads as not on it, so a
// wait can look while an up runs.
func (e *hostEnv) proxyOnSelfNet() bool {
	e.t.Helper()
	out, err := e.host.Sh(testsupport.Context(e.t), "docker inspect --format '{{json .NetworkSettings.Networks}}' "+ingress.ProjectName+"-"+ingress.ProxyComposeService+"-1")
	if err != nil {
		return false
	}
	return strings.Contains(out, `"`+ingress.SelfNetworkName+`"`)
}

// TestSelfNetworkOnAStandaloneHost is D-04 on a real standalone host (D-23): with TierPlan's own route on, the ensure pass
// makes the tierplan-self bridge, connects the container standing in for TierPlan to it under the alias tierplan and audits
// that once; after the container is removed and run again (an upgrade), the next pass connects the new one; and when the
// route goes, TierPlan's container is taken off and the bridge removed, leaving nothing behind.
func TestSelfNetworkOnAStandaloneHost(t *testing.T) {
	e := startHostEnv(t, true, withHostSelf)
	ctx := testsupport.Context(t)

	image := traefiktest.SelfStubImage(t)
	dockertest.LoadImage(t, image, e.host)
	writeStubTLS(t, e.host)
	e.runStub(image)

	requireJobDone(t, e.enable())
	if _, ok := selfNetworkOn(t, e.host); ok {
		t.Fatalf("%s exists before TierPlan's own route is on", ingress.SelfNetworkName)
	}

	// The route goes on: one pass makes the bridge and connects TierPlan to it.
	claimSelf(t, e.istore, e.envID, ingressdeploy.SelfBackendAlias)
	e.passUntil("the ensure loop to make "+ingress.SelfNetworkName+" and connect TierPlan's container", func() (bool, string) {
		if _, ok := selfNetworkOn(t, e.host); !ok {
			return false, "no network yet"
		}
		return e.stubOnSelf()
	})
	net, _ := selfNetworkOn(t, e.host)
	if net.Driver != dockerx.DriverBridge || net.Scope != "local" {
		t.Errorf("%s is driver %q scope %q, want a local bridge", ingress.SelfNetworkName, net.Driver, net.Scope)
	}
	if !ingress.OwnsSelfNetwork(ingress.SelfNetworkName, net.Labels) {
		t.Errorf("%s labels = %v, want the ingress component and the self role", ingress.SelfNetworkName, net.Labels)
	}
	if n := e.selfAttachedRows(); n != 1 {
		t.Errorf("%d %s audit rows after the connect, want one", n, audit.ActionIngressSelfNetworkAttached)
	}
	e.passUntil("the proxy to join "+ingress.SelfNetworkName+" and the observation to say TierPlan is on it", func() (bool, string) {
		proxyOn := e.proxyOnSelfNet()
		o, _ := e.deploy.SelfObservationOf(e.envID)
		return proxyOn && o.Exists && o.Attached && o.Kind == ingressdeploy.SelfKindStandalone && o.BackendHost == ingressdeploy.SelfBackendAlias,
			fmt.Sprintf("proxy on it %v, observation %+v", proxyOn, o)
	})
	// A pass over a host that is as wanted connects nothing again.
	e.deploy.Round(ctx)
	if n := e.selfAttachedRows(); n != 1 {
		t.Errorf("%d %s audit rows after a quiet pass, want still one", n, audit.ActionIngressSelfNetworkAttached)
	}

	// An upgrade recreates TierPlan's container, which comes back on the default bridge only: the next pass connects it.
	oldID := e.sh("docker inspect --format '{{.Id}}' " + stubContainer)
	e.sh("docker rm -f " + stubContainer)
	e.runStub(image)
	if on, seen := e.stubOnSelf(); on {
		t.Fatalf("the recreated stub is on %s before any pass: %s", ingress.SelfNetworkName, seen)
	}
	e.passUntil("the ensure loop to connect the recreated container", e.stubOnSelf)
	if newID := e.sh("docker inspect --format '{{.Id}}' " + stubContainer); newID == oldID {
		t.Fatal("the stub was not recreated")
	}
	if n := e.selfAttachedRows(); n != 2 {
		t.Errorf("%d %s audit rows after the reconnect, want two", n, audit.ActionIngressSelfNetworkAttached)
	}

	// The route goes off: TierPlan's container is taken off the bridge and the bridge removed.
	clearSelf(t, e.istore, e.envID)
	e.passUntil("the ensure loop to disconnect TierPlan and remove "+ingress.SelfNetworkName, func() (bool, string) {
		_, ok := selfNetworkOn(t, e.host)
		nets := e.networksOf(stubContainer)
		return !ok && !slices.Contains(nets, ingress.SelfNetworkName), fmt.Sprintf("network there %v, stub networks %v", ok, nets)
	})
	if e.proxyOnSelfNet() {
		t.Errorf("the proxy is still on %s", ingress.SelfNetworkName)
	}
	if o, _ := e.deploy.SelfObservationOf(e.envID); o.Exists || o.Attached || o.Wanted {
		t.Errorf("observation after the route went = %+v, want no network, not wanted", o)
	}
	if n := e.selfAttachedRows(); n != 2 {
		t.Errorf("%d %s audit rows after the route went, want still two", n, audit.ActionIngressSelfNetworkAttached)
	}
}
