// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingressrender/traefiktest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

const (
	// stubService is the swarm service that stands in for TierPlan: service tierplan of the stack tierplan, the name the
	// stack file of install-in-swarm.md gives it.
	stubService   = "tierplan_tierplan"
	stubNamespace = "tierplan"
	// stubTLSDir is where the stub's certificate and key are written on the node that runs it.
	stubTLSDir = "/tmp/tp-selfstub"
	// selfHost is the hostname of TierPlan's own route in the tests.
	selfHost = "tierplan." + testDomain
	// selfBound bounds each wait for the ensure loop to bring the self network where a step wants it. The passes are taken by
	// the test as fast as the engines answer; the bound is a failure deadline only.
	selfBound = 5 * time.Minute

	// Labels the engine puts on every container of a swarm service's task, which the server's own read maps (D-03).
	labelTaskServiceName = "com.docker.swarm.service.name"
	labelTaskServiceID   = "com.docker.swarm.service.id"
)

// stubCommand is the stub's arguments: TierPlan's port, and the certificate written to stubTLSDir.
func stubCommand() string {
	return "-listen :" + traefiktest.StubPort + " -cert /tls/tls.crt -key /tls/tls.key"
}

// writeStubTLS writes a certificate and key the stub serves into stubTLSDir on the node.
func writeStubTLS(t *testing.T, n *dockertest.Node) {
	t.Helper()
	ctx := testsupport.Context(t)
	cert, key := traefiktest.SelfSignedLeaf(t, "tierplan")
	if _, err := n.Sh(ctx, "mkdir -p "+stubTLSDir); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{stubTLSDir + "/tls.crt": cert, stubTLSDir + "/tls.key": key} {
		if err := n.WriteFile(ctx, path, data); err != nil {
			t.Fatal(err)
		}
	}
}

// withSwarmSelf gives the deployer what the server gives it to find itself on a swarm: the local engine's membership (the
// manager's, which is where the stub runs) and a read of TierPlan's own container, which here is the stub's running task
// container on the manager, read through the environment's own client and mapped as the server's readSelfFacts maps it.
func withSwarmSelf(e *swarmEnv, d *ingressdeploy.Deps) {
	t := e.t
	info, err := e.mgr.Info(testsupport.Context(t), t)
	if err != nil {
		t.Fatalf("read the engine info of %s: %v", e.mgr.Hostname, err)
	}
	local := envs.SwarmMembership{Known: true, Member: true, ClusterID: info.ClusterID, NodeID: info.NodeID, EngineID: info.ID}
	d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return local, nil }
	agentID := e.agents[e.mgr].identity.AgentID
	d.Self = func(ctx context.Context) (ingressdeploy.SelfFacts, error) {
		var facts ingressdeploy.SelfFacts
		err := e.agg.OnNode(ctx, e.envID, agentID, func(c *dockerx.Client) error {
			rows, err := c.ContainersWithLabel(ctx, labelTaskServiceName, stubService)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if r.State != "running" {
					continue
				}
				detail, _, err := c.ContainerDetail(ctx, r.ID)
				if err != nil {
					return err
				}
				facts = selfFactsOfStub(detail)
				return nil
			}
			return errors.New("the stub service has no running task on the manager")
		})
		return facts, err
	}
}

// selfFactsOfStub maps a container's inspect as the server's selfFactsOf does.
func selfFactsOfStub(detail dockerx.ContainerDetail) ingressdeploy.SelfFacts {
	f := ingressdeploy.SelfFacts{
		ContainerID:    detail.ID,
		ServiceName:    detail.Labels[labelTaskServiceName],
		ServiceID:      detail.Labels[labelTaskServiceID],
		StackNamespace: detail.Labels[stacks.LabelSwarmNamespace],
	}
	f.Swarm = f.ServiceName != "" || f.ServiceID != ""
	for _, n := range detail.Networks {
		f.Networks = append(f.Networks, n.Name)
	}
	return f
}

// claimSelf stores TierPlan's own route in the environment, as the self route's enable job does at its claim step.
func claimSelf(t *testing.T, st *ingressstore.Store, envID, service string) {
	t.Helper()
	ctx := testsupport.Context(t)
	doms, err := st.Domains(ctx, envID)
	if err != nil || len(doms) != 1 {
		t.Fatalf("the environment's domains = %d (%v), want the one the harness registered", len(doms), err)
	}
	if err := st.SetSystemClaim(ctx, ingressstore.SystemClaimInput{
		EnvID: envID, Hostname: selfHost, DomainID: doms[0].ID, Service: service, Port: 7480, Actor: "user:test",
	}, nil); err != nil {
		t.Fatalf("SetSystemClaim: %v", err)
	}
}

// clearSelf removes TierPlan's own route from the environment.
func clearSelf(t *testing.T, st *ingressstore.Store, envID string) {
	t.Helper()
	if _, ok, err := st.ClearSystemClaim(testsupport.Context(t), envID, "user:test", time.Time{}, nil); err != nil || !ok {
		t.Fatalf("ClearSystemClaim = %v, %v; want the claim cleared", ok, err)
	}
}

// selfNetworkInspect is the part of the self network's inspect the tests assert on.
type selfNetworkInspect struct {
	ID         string
	Driver     string
	Scope      string
	Attachable bool
	Labels     map[string]string
}

// selfNetworkOn inspects tierplan-self on the node; ok is false when the node holds none.
func selfNetworkOn(t *testing.T, n *dockertest.Node) (selfNetworkInspect, bool) {
	t.Helper()
	out, err := n.Sh(testsupport.Context(t), "docker network ls -q --filter name=^"+ingress.SelfNetworkName+"$")
	if err != nil {
		t.Fatalf("list the networks of %s: %v", n.Hostname, err)
	}
	if strings.TrimSpace(out) == "" {
		return selfNetworkInspect{}, false
	}
	out, err = n.Sh(testsupport.Context(t), "docker network inspect --format '{{json .}}' "+ingress.SelfNetworkName)
	if err != nil {
		t.Fatalf("inspect %s on %s: %v", ingress.SelfNetworkName, n.Hostname, err)
	}
	var in selfNetworkInspect
	if err := json.Unmarshal([]byte(out), &in); err != nil {
		t.Fatalf("read the inspect of %s: %v", ingress.SelfNetworkName, err)
	}
	return in, true
}

// proxyOnSelf is whether the proxy service's spec attaches to the network of that ID.
func (e *swarmEnv) proxyOnSelf(networkID string) bool {
	e.t.Helper()
	d, ok := e.proxyService()
	if !ok {
		e.t.Fatal("the proxy service is gone")
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil {
		e.t.Fatalf("read the proxy service's spec: %v", err)
	}
	return slices.ContainsFunc(spec.TaskTemplate.Networks, func(n swarm.NetworkAttachmentConfig) bool {
		return n.Target == networkID || n.Target == ingress.SelfNetworkName
	})
}

// passUntil takes ensure passes until cond holds, each pass as soon as the last one ended. The deployer's own loop may run a
// pass at the same time, which then leaves the environment to it (the operation lock): the next pass looks again.
func (e *swarmEnv) passUntil(what string, cond func() (bool, string)) {
	e.t.Helper()
	dockertest.Wait(e.t, selfBound, what, func() (bool, string) {
		e.deploy.Round(testsupport.Context(e.t))
		return cond()
	})
}

// TestSelfNetworkOnASwarm is D-23 on a real swarm: switching TierPlan's own route on makes tierplan-self as D-02 says (a
// swarm-scoped overlay that is not attachable, with the label pair), the proxy joins it and a plain container is refused it
// (T-28-68). The stub standing in for TierPlan is attached by the D-03 service update and the observation says so. When the
// route goes, the proxy leaves the network and the network stays while TierPlan's service names it (A-06); once its service
// update takes it off, the next pass removes the network, and switching ingress off leaves none.
func TestSelfNetworkOnASwarm(t *testing.T) {
	e := startSwarmEnv(t, withSwarmSelf)
	ctx := testsupport.Context(t)

	// The stub, as TierPlan's own swarm service, on the manager.
	image := traefiktest.SelfStubImage(t)
	dockertest.LoadImage(t, image, e.mgr)
	writeStubTLS(t, e.mgr)
	e.sh(e.mgr, "docker service create --detach=false --quiet --no-resolve-image --name "+stubService+
		" --label "+stacks.LabelSwarmNamespace+"="+stubNamespace+" --container-label "+stacks.LabelSwarmNamespace+"="+stubNamespace+
		" --constraint node.role==manager --mount type=bind,src="+stubTLSDir+",dst=/tls,readonly "+image+" "+stubCommand())

	requireJobDone(t, e.enable())
	if _, ok := selfNetworkOn(t, e.mgr); ok {
		t.Fatalf("%s exists before TierPlan's own route is on", ingress.SelfNetworkName)
	}

	// The route goes on: the next pass makes the network and puts the proxy on it.
	claimSelf(t, e.istore, e.envID, stubService)
	var net selfNetworkInspect
	e.passUntil("the ensure loop to make "+ingress.SelfNetworkName+" and attach the proxy", func() (bool, string) {
		var ok bool
		if net, ok = selfNetworkOn(t, e.mgr); !ok {
			return false, "no network yet"
		}
		return e.proxyOnSelf(net.ID), "the proxy is not on it yet"
	})
	if net.Driver != "overlay" || net.Scope != "swarm" || net.Attachable {
		t.Errorf("%s is driver %q scope %q attachable %v, want a swarm-scoped overlay that is not attachable", ingress.SelfNetworkName, net.Driver, net.Scope, net.Attachable)
	}
	if !ingress.OwnsSelfNetwork(ingress.SelfNetworkName, net.Labels) {
		t.Errorf("%s labels = %v, want the ingress component and the self role", ingress.SelfNetworkName, net.Labels)
	}

	// A plain container is refused the network (T-28-68): only service specs join it.
	out, err := e.mgr.Sh(ctx, "docker run --rm --network "+ingress.SelfNetworkName+" --entrypoint true "+image+" 2>&1")
	if err == nil {
		t.Fatalf("a plain container joined %s: %s", ingress.SelfNetworkName, out)
	}
	if !strings.Contains(out, "not manually attachable") {
		t.Fatalf("a plain container on %s failed for another reason than the network: %s", ingress.SelfNetworkName, strings.TrimSpace(out))
	}

	// TierPlan is not on it until its own service names it (D-03): TierPlan never updates its own service.
	if o, ok := e.deploy.SelfObservationOf(e.envID); !ok || !o.Exists || !o.Wanted || o.Attached || o.Kind != ingressdeploy.SelfKindSwarmService || o.BackendHost != stubService {
		t.Fatalf("observation before the service update = %+v (made %v), want the network there and wanted, TierPlan's service not on it", o, ok)
	}
	e.sh(e.mgr, ingressdeploy.ServiceUpdateCommand(stubService)+" 2>&1")
	e.passUntil("the observation to say TierPlan is on "+ingress.SelfNetworkName, func() (bool, string) {
		o, ok := e.deploy.SelfObservationOf(e.envID)
		return ok && o.Exists && o.Attached, fmt.Sprintf("%+v", o)
	})
	if o, _ := e.deploy.SelfObservationOf(e.envID); o.NetworkID != net.ID || !o.Subnet.IsValid() || o.KeptInUse {
		t.Errorf("observation once attached = %+v, want network %s with its subnet, not kept", o, net.ID)
	}

	// The route goes off (A-06): the proxy leaves the network, and the network stays while TierPlan's service is on it.
	clearSelf(t, e.istore, e.envID)
	e.passUntil("the proxy to leave "+ingress.SelfNetworkName+" while it is kept for TierPlan", func() (bool, string) {
		o, _ := e.deploy.SelfObservationOf(e.envID)
		return !e.proxyOnSelf(net.ID) && o.KeptInUse && !o.Wanted, fmt.Sprintf("%+v", o)
	})
	// Another pass changes nothing: the network is the person's to take out of TierPlan's service.
	e.deploy.Round(ctx)
	if now, ok := selfNetworkOn(t, e.mgr); !ok || now.ID != net.ID {
		t.Fatalf("%s was removed while TierPlan's service is on it", ingress.SelfNetworkName)
	}

	// TierPlan's service leaves it: the next pass removes it.
	e.sh(e.mgr, "docker service update --detach=false --quiet --network-rm "+ingress.SelfNetworkName+" "+stubService+" 2>&1")
	e.passUntil("the ensure loop to remove "+ingress.SelfNetworkName, func() (bool, string) {
		_, ok := selfNetworkOn(t, e.mgr)
		o, _ := e.deploy.SelfObservationOf(e.envID)
		return !ok && !o.Exists, fmt.Sprintf("network there %v, observation %+v", ok, o)
	})

	// Ingress off leaves no tierplan-self.
	requireJobDone(t, e.disable())
	if _, ok := selfNetworkOn(t, e.mgr); ok {
		t.Errorf("%s is left after ingress was switched off", ingress.SelfNetworkName)
	}
	if _, ok := e.proxyService(); ok {
		t.Errorf("the proxy service is left after ingress was switched off")
	}
}
