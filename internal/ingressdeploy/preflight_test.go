// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// findingOf returns the first finding with the code, and whether there is one.
func findingOf(fs []ingressdeploy.Finding, code string) (ingressdeploy.Finding, bool) {
	for _, f := range fs {
		if f.Code == code {
			return f, true
		}
	}
	return ingressdeploy.Finding{}, false
}

// wantRefusal fails the test unless Enable is refused by a *PreflightError that holds an error finding with
// the code, and nothing was written, locked or started.
func wantRefusal(t *testing.T, r *rig, req ingressdeploy.EnableRequest, code string) ingressdeploy.Finding {
	t.Helper()
	_, err := r.svc.Enable(testsupport.Context(t), testActor, testOwner, r.envID, req)
	var pe *ingressdeploy.PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Enable = %v, want a *PreflightError with %s", err, code)
	}
	f, ok := findingOf(pe.Findings, code)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want %s as an error", pe.Findings, code)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the refusal left the lock held")
	}
	if got := r.hub.List(func(jobs.Job) bool { return true }); len(got) != 0 {
		t.Errorf("%d jobs were started", len(got))
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
	return f
}

// TestPreflightRefusesServicePorts is the tracer: a service of a stack, or of none, that publishes TCP 80 or
// 443 in either mode refuses the preview and the enable, and names each; a UDP 443 does not clash.
func TestPreflightRefusesServicePorts(t *testing.T) {
	r := newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{
			{ID: "s1", Name: "shop_web", Mode: "replicated", Desired: 1, Labels: map[string]string{"com.docker.stack.namespace": "shop"},
				Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 8080, PublishedPort: 80, PublishMode: "ingress"}}},
			{ID: "s2", Name: "plain", Mode: "global",
				Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 8443, PublishedPort: 443, PublishMode: "host"}}},
			{ID: "s3", Name: "quic", Mode: "replicated", Desired: 1,
				Ports: []swarm.PortConfig{{Protocol: "udp", TargetPort: 8443, PublishedPort: 443, PublishMode: "ingress"}}},
		}
	}})

	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want ingress_port_in_use as an error", res.Findings)
	}
	want := []string{"plain publishes 443 (host mode)", "shop_web publishes 80 (ingress mode)"}
	if !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	if ingressdeploy.CodePortInUse != "ingress_port_in_use" {
		t.Errorf("code = %q", ingressdeploy.CodePortInUse)
	}

	got := wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)
	if !slices.Equal(got.Items, want) {
		t.Errorf("Enable's items = %q, want %q", got.Items, want)
	}
}

// nodeEngine reads each node through its own fake engine; a node that has none, or is marked down, fails the read.
type nodeEngine struct {
	ingressdeploy.Engine
	byAgent map[string]*dockerx.Client
	down    map[string]bool
}

func (e *nodeEngine) OnNode(ctx context.Context, envID, agentID string, fn func(*dockerx.Client) error) error {
	if e.down[agentID] {
		return errors.New("the tunnel to the node dropped")
	}
	if c, ok := e.byAgent[agentID]; ok {
		return fn(c)
	}
	return e.Engine.OnNode(ctx, envID, agentID, fn)
}

// workerNode is the second node of a preflight test's swarm.
var workerNode = ingressdeploy.NodeInfo{SwarmNodeID: "node-2", AgentID: "agent-2", Name: "worker-1", Role: "worker", Connected: true}

// withWorker adds a connected worker to the environment, read through a fake engine that runs the containers.
func withWorker(t *testing.T, r *rig, ne *nodeEngine, containers ...dockertest.FakeContainer) {
	t.Helper()
	fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: containers})
	cli, err := dockerx.New(fake)
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	ne.byAgent["agent-2"] = cli
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes = append(i.Nodes, workerNode) })
}

// newNodeRig is a rig whose node reads go through the returned nodeEngine.
func newNodeRig(t *testing.T, o rigOptions) (*rig, *nodeEngine) {
	t.Helper()
	ne := &nodeEngine{byAgent: map[string]*dockerx.Client{}, down: map[string]bool{}}
	inner := o.deps
	o.deps = func(d *ingressdeploy.Deps) {
		if inner != nil {
			inner(d)
		}
		ne.Engine = d.Engine
		d.Engine = ne
	}
	return newRig(t, o), ne
}

// published is a container port that is published on the host.
func published(port uint16, proto string) []dockertest.FakePort {
	return []dockertest.FakePort{{IP: "0.0.0.0", Private: port, Public: port, Type: proto}}
}

// TestPreflightContainerPortsTCPOnly: a plain container that publishes TCP 80 on a node refuses, naming the node and
// the container; a UDP 443, an ingress task and a swarm task (which the service check covers) do not.
func TestPreflightContainerPortsTCPOnly(t *testing.T) {
	r, ne := newNodeRig(t, rigOptions{})
	withWorker(t, r, ne,
		dockertest.FakeContainer{ID: "c1", Names: []string{"/web"}, State: "running", Ports: published(80, "tcp")},
		dockertest.FakeContainer{ID: "c2", Names: []string{"/quic"}, State: "running", Ports: published(443, "udp")},
		dockertest.FakeContainer{ID: "c3", Names: []string{"/tierplan-ingress.x"}, State: "running", Ports: published(80, "tcp"),
			Labels: ingress.ObjectLabels()},
		dockertest.FakeContainer{ID: "c4", Names: []string{"/shop_web.1.abc"}, State: "running", Ports: published(443, "tcp"),
			Labels: map[string]string{"com.docker.swarm.service.name": "shop_web"}},
		dockertest.FakeContainer{ID: "c5", Names: []string{"/idle"}, State: "running", Ports: []dockertest.FakePort{{Private: 80, Type: "tcp"}}},
	)
	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want ingress_port_in_use as an error", res.Findings)
	}
	if want := []string{"worker-1: web publishes 80"}; !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)

	// Only the UDP port and the ingress task: nothing clashes.
	r2, ne2 := newNodeRig(t, rigOptions{})
	withWorker(t, r2, ne2,
		dockertest.FakeContainer{ID: "c2", Names: []string{"/quic"}, State: "running", Ports: published(443, "udp")},
		dockertest.FakeContainer{ID: "c3", Names: []string{"/tierplan-ingress.x"}, State: "running", Ports: published(80, "tcp"),
			Labels: ingress.ObjectLabels()},
	)
	res, err = r2.svc.Preview(testsupport.Context(t), r2.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if _, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse); ok {
		t.Errorf("findings = %+v, want no port finding", res.Findings)
	}
}

// TestPreflightUnreachableNodeWarns: a node that cannot be read, or has no connected agent, is the warning
// ingress_nodes_not_checked and never a refusal (PROH-24-06): the enable goes through.
func TestPreflightUnreachableNodeWarns(t *testing.T) {
	r, ne := newNodeRig(t, rigOptions{})
	withWorker(t, r, ne)
	ne.down["agent-2"] = true
	r.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = append(i.Nodes, ingressdeploy.NodeInfo{SwarmNodeID: "node-3", Name: "worker-0", Role: "worker"})
	})

	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodeNodesNotChecked)
	if !ok || f.Severity != ingressdeploy.SeverityWarning {
		t.Fatalf("findings = %+v, want ingress_nodes_not_checked as a warning", res.Findings)
	}
	if want := []string{"worker-0", "worker-1"}; !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	for _, g := range res.Findings {
		if g.Severity == ingressdeploy.SeverityError {
			t.Errorf("unexpected refusal %+v", g)
		}
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
}

// stubAddressing hands out the inputs a test sets.
type stubAddressing struct{ in envs.AllocationInputs }

func (stubAddressing) LockAllocation(string) func() { return func() {} }
func (a stubAddressing) AllocationInputs(context.Context, string, bool) (envs.AllocationInputs, error) {
	return a.in, nil
}

// TestPreflightAddressPool: the checks of the ingress network's block.
func TestPreflightAddressPool(t *testing.T) {
	full := stubAddressing{in: envs.AllocationInputs{
		Pools:    []netspace.Pool{{Base: netip.MustParsePrefix("10.0.0.0/22"), Size: 24}},
		Occupied: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
	}}
	preview := func(t *testing.T, r *rig, subnet string) ingressdeploy.PreviewResult {
		t.Helper()
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{Subnet: subnet})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		return res
	}

	t.Run("no free block", func(t *testing.T) {
		r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.AddressSpaces = full }})
		if _, ok := findingOf(preview(t, r, "").Findings, ingressdeploy.CodeNoAddressSpace); !ok {
			t.Fatal("no ingress_no_address_space finding")
		}
		wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodeNoAddressSpace)
	})
	t.Run("not an ipv4 block", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		for _, bad := range []string{"10.0.0.0/33", "fd00::/64", "ten"} {
			if _, ok := findingOf(preview(t, r, bad).Findings, ingressdeploy.CodeSubnetInvalid); !ok {
				t.Errorf("%q: no ingress_subnet_invalid finding", bad)
			}
		}
		wantRefusal(t, r, ingressdeploy.EnableRequest{Subnet: "10.0.0.0/33"}, ingressdeploy.CodeSubnetInvalid)
	})
	t.Run("overlaps a network", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		f, ok := findingOf(preview(t, r, "10.0.0.0/22").Findings, ingressdeploy.CodeSubnetOverlaps)
		if !ok || len(f.Items) != 1 || !strings.Contains(f.Items[0], "ingress") || !strings.Contains(f.Items[0], "10.0.0.0/24") {
			t.Fatalf("finding = %+v, want it to name the network", f)
		}
		wantRefusal(t, r, ingressdeploy.EnableRequest{Subnet: "10.0.0.0/22"}, ingressdeploy.CodeSubnetOverlaps)
	})
	t.Run("a free block passes", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		res := preview(t, r, "10.8.0.0/22")
		if len(res.Findings) != 0 || res.Plan.Network.Subnet != "10.8.0.0/22" {
			t.Errorf("findings = %+v, subnet = %q, want none and the typed block", res.Findings, res.Plan.Network.Subnet)
		}
		if res = preview(t, r, ""); len(res.Findings) != 0 || res.Plan.Network.Subnet != "10.0.4.0/22" {
			t.Errorf("findings = %+v, subnet = %q, want none and the lowest free block", res.Findings, res.Plan.Network.Subnet)
		}
	})
	t.Run("an existing ingress network skips the checks", func(t *testing.T) {
		r := newRig(t, rigOptions{
			deps: func(d *ingressdeploy.Deps) { d.AddressSpaces = full },
			config: func(c *dockertest.FakeConfig) {
				c.Networks = append(c.Networks, dockertest.FakeNetwork{
					ID: "ours", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/22"}, Labels: ingress.ObjectLabels(),
				})
			},
		})
		for _, subnet := range []string{"", "10.0.0.0/22", "10.0.0.0/33"} {
			res := preview(t, r, subnet)
			for _, f := range res.Findings {
				if f.Severity == ingressdeploy.SeverityError {
					t.Errorf("subnet %q: unexpected refusal %+v", subnet, f)
				}
			}
		}
	})
}

// The facts of TierPlan's own container the HTTP-01 tests give the deployer: a swarm service off and on the self network, and a
// plain container.
var (
	selfService      = ingressdeploy.SelfFacts{ContainerID: "c1", Swarm: true, ServiceName: "tierplan_tierplan", ServiceID: "svc1", StackNamespace: "tierplan", Networks: []string{"tierplan_default"}}
	selfServiceOnNet = ingressdeploy.SelfFacts{ContainerID: "c1", Swarm: true, ServiceName: "tierplan_tierplan", ServiceID: "svc1", StackNamespace: "tierplan", Networks: []string{"tierplan_default", "tierplan-self"}}
	selfPlain        = ingressdeploy.SelfFacts{ContainerID: "c2", Networks: []string{"bridge"}}
)

// ownURLs are an instance's public URL and agent URL, for the carve-out notice's hosts.
func ownURLs() ingressdeploy.SelfURLs {
	return ingressdeploy.SelfURLs{PublicURL: "https://TierPlan.example.org", AgentURL: "https://tierplan.internal:7480"}
}

// selfReads answers TierPlan's own container with facts, or with err when it is set.
func selfReads(facts ingressdeploy.SelfFacts, err error) func(context.Context) (ingressdeploy.SelfFacts, error) {
	return func(context.Context) (ingressdeploy.SelfFacts, error) {
		if err != nil {
			return ingressdeploy.SelfFacts{}, err
		}
		return facts, nil
	}
}

// TestPreflightHTTP01Conflict: with TierPlan's own certificate on HTTP-01, the swarm TierPlan runs on as a service on the self
// network is told the carve-out keeps the renewal working; where the carve-out cannot reach TierPlan the switch is refused with
// the fix; elsewhere is nothing, and what cannot be told is a warning (D-09, D-12, SELF-04).
func TestPreflightHTTP01Conflict(t *testing.T) {
	type want int
	const (
		nothing want = iota
		notice
		refused
		plainRefused
		warned
	)
	sameCluster := envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "elsewhere"}
	cases := []struct {
		name    string
		inUse   bool
		local   envs.SwarmMembership
		facts   ingressdeploy.SelfFacts
		selfErr error
		want    want
	}{
		{name: "a service on tierplan-self", inUse: true, local: sameCluster, facts: selfServiceOnNet, want: notice},
		{name: "a service not on tierplan-self", inUse: true, local: sameCluster, facts: selfService, want: refused},
		{name: "a worker of this swarm, not on tierplan-self", inUse: true, local: envs.SwarmMembership{Known: true, Member: true, NodeID: "node-1"}, facts: selfService, want: refused},
		{name: "a plain container on this swarm", inUse: true, local: sameCluster, facts: selfPlain, want: plainRefused},
		{name: "on this swarm, its own container unread", inUse: true, local: sameCluster, selfErr: errors.New("no container"), want: refused},
		{name: "not on HTTP-01, on this swarm", inUse: false, local: sameCluster, facts: selfService, want: nothing},
		{name: "elsewhere", inUse: true, local: envs.SwarmMembership{Known: true, Member: true, ClusterID: "another-cluster", NodeID: "other-node"}, facts: selfService, want: nothing},
		{name: "a host that is no swarm node", inUse: true, local: envs.SwarmMembership{Known: true}, facts: selfPlain, want: nothing},
		{name: "cannot tell", inUse: true, local: envs.SwarmMembership{}, facts: selfService, want: warned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
				d.OwnHTTP01InUse = func(context.Context) (bool, error) { return tc.inUse, nil }
				d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return tc.local, nil }
				d.Self = selfReads(tc.facts, tc.selfErr)
				d.SelfURLs = ownURLs
			}})
			res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			conflict, isRefused := findingOf(res.Findings, ingressdeploy.CodeHTTP01Conflict)
			carve, isNotice := findingOf(res.Findings, ingressdeploy.CodeHTTP01CarveOut)
			_, isWarned := findingOf(res.Findings, ingressdeploy.CodeHTTP01Unknown)
			switch tc.want {
			case notice:
				if isRefused || isWarned || !isNotice || carve.Severity != ingressdeploy.SeverityNotice {
					t.Fatalf("findings = %+v, want only the carve-out notice", res.Findings)
				}
				wantMsg := "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for " +
					"tierplan.example.org and tierplan.internal to TierPlan, over the tierplan-self network, so the certificate keeps renewing."
				if carve.Message != wantMsg {
					t.Errorf("message = %q, want %q", carve.Message, wantMsg)
				}
				if want := []string{"tierplan.example.org", "tierplan.internal"}; !slices.Equal(carve.Items, want) {
					t.Errorf("items = %q, want %q", carve.Items, want)
				}
				wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
			case refused, plainRefused:
				if !isRefused || isNotice || isWarned || conflict.Severity != ingressdeploy.SeverityError {
					t.Fatalf("findings = %+v, want ingress_http01_conflict as an error", res.Findings)
				}
				wantMsg := "TierPlan runs on this swarm and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes on every node. " +
					"Add the tierplan-self network to TierPlan's service, or switch TierPlan's own certificate to DNS-01. Then turn ingress on again."
				if tc.want == plainRefused {
					wantMsg = "TierPlan runs on this swarm and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes on every node. " +
						"Switch TierPlan's own certificate to DNS-01. Then turn ingress on again."
				}
				if conflict.Message != wantMsg {
					t.Errorf("message = %q, want %q", conflict.Message, wantMsg)
				}
				wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodeHTTP01Conflict)
			case warned:
				if isRefused || isNotice || !isWarned {
					t.Fatalf("findings = %+v, want only ingress_http01_unknown", res.Findings)
				}
			default:
				if isRefused || isNotice || isWarned {
					t.Fatalf("findings = %+v, want no HTTP-01 finding", res.Findings)
				}
			}
		})
	}

	t.Run("one host reads as one name", func(t *testing.T) {
		r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
			d.OwnHTTP01InUse = func(context.Context) (bool, error) { return true, nil }
			d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return sameCluster, nil }
			d.Self = selfReads(selfServiceOnNet, nil)
			d.SelfURLs = func() ingressdeploy.SelfURLs {
				return ingressdeploy.SelfURLs{PublicURL: "https://tierplan.example.org", AgentURL: "https://tierplan.example.org:7480"}
			}
		}})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		f, ok := findingOf(res.Findings, ingressdeploy.CodeHTTP01CarveOut)
		if !ok || !strings.Contains(f.Message, "requests for tierplan.example.org to TierPlan") || !slices.Equal(f.Items, []string{"tierplan.example.org"}) {
			t.Errorf("finding = %+v, want the one host named once", f)
		}
	})

	t.Run("unreadable is a warning", func(t *testing.T) {
		r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
			d.OwnHTTP01InUse = func(context.Context) (bool, error) { return false, errors.New("no settings") }
			d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
				return envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "node-1"}, nil
			}
			d.Self = selfReads(selfService, nil)
		}})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if f, ok := findingOf(res.Findings, ingressdeploy.CodeHTTP01Unknown); !ok || f.Severity != ingressdeploy.SeverityWarning {
			t.Errorf("findings = %+v, want ingress_http01_unknown as a warning", res.Findings)
		}
		wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	})
}

// TestNoLaterVersionSentence: the Phase 24 and 25 refusal promised a later version would lift the limit; D-12 lifts it where
// the carve-out works and says what to do where it does not, so the promise is gone from every source file of the package.
func TestNoLaterVersionSentence(t *testing.T) {
	old := "A later version " + "lifts this limit."
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	read := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		read++
		if strings.Contains(string(b), old) {
			t.Errorf("%s still says %q", f, old)
		}
	}
	if read == 0 {
		t.Fatal("no source file read")
	}
}

// TestPreflightHTTP01SaysWhenItCannotTell: where TierPlan's own certificate uses HTTP-01 and it cannot be told whether the
// server runs on this swarm, the preview carries the ingress_http01_unknown warning rather than passing silently (D-09, WR-08):
// no Docker socket, an engine that does not answer, a worker whose node the environment does not list, a manager of an
// environment whose cluster is not known. It never refuses, and a certificate that is not on HTTP-01 has nothing to warn of.
func TestPreflightHTTP01SaysWhenItCannotTell(t *testing.T) {
	cases := []struct {
		name      string
		inUse     bool
		local     envs.SwarmMembership
		localErr  error
		noCluster bool
		warned    bool
	}{
		{name: "no socket", inUse: true, warned: true},
		{name: "the probe failed", inUse: true, localErr: errors.New("probe failed"), warned: true},
		{name: "a worker of a swarm the environment does not list", inUse: true, local: envs.SwarmMembership{Known: true, Member: true, NodeID: "stranger"}, warned: true},
		{name: "a manager, with the environment's cluster not known", inUse: true, noCluster: true, local: envs.SwarmMembership{Known: true, Member: true, ClusterID: "c", NodeID: "stranger"}, warned: true},
		{name: "no socket, not on HTTP-01", inUse: false, warned: false},
		{name: "a standalone engine", inUse: true, local: envs.SwarmMembership{Known: true}, warned: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
				d.OwnHTTP01InUse = func(context.Context) (bool, error) { return tc.inUse, nil }
				d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return tc.local, tc.localErr }
				d.Self = selfReads(selfService, nil)
			}})
			if tc.noCluster {
				r.envs.set(func(i *ingressdeploy.EnvInfo) { i.ClusterID = "" })
			}
			res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			f, ok := findingOf(res.Findings, ingressdeploy.CodeHTTP01Unknown)
			if ok != tc.warned {
				t.Fatalf("findings = %+v, warned = %v, want %v", res.Findings, ok, tc.warned)
			}
			if _, refused := findingOf(res.Findings, ingressdeploy.CodeHTTP01Conflict); refused {
				t.Errorf("findings = %+v, want no refusal for something that cannot be told", res.Findings)
			}
			if ok && f.Severity != ingressdeploy.SeverityWarning {
				t.Errorf("severity = %v, want a warning", f.Severity)
			}
		})
	}
}

// TestPreflightHTTP01SelfSignedIsNotRefused: a self-signed install that never set Let's Encrypt up reads HTTP-01 as its challenge
// by default, but nothing of TierPlan's needs port 80, so the swarm TierPlan runs on is neither refused nor warned (A-04).
func TestPreflightHTTP01SelfSignedIsNotRefused(t *testing.T) {
	r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
		d.OwnHTTP01InUse = func(context.Context) (bool, error) { return false, nil }
		d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
			return envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "node-1"}, nil
		}
	}})
	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	for _, code := range []string{ingressdeploy.CodeHTTP01Conflict, ingressdeploy.CodeHTTP01Unknown} {
		if _, ok := findingOf(res.Findings, code); ok {
			t.Errorf("findings = %+v, want no %s for a self-signed install", res.Findings, code)
		}
	}
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
}

// TestPreflightNameTaken: an object with a reserved ingress name and without the component label refuses; a labelled
// one is TierPlan's and is reused (Pitfall 11).
func TestPreflightNameTaken(t *testing.T) {
	secretName := ingress.SecretPrefix + "old"
	t.Run("not TierPlan's", func(t *testing.T) {
		r := newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
			c.ServiceRows = []dockertest.FakeService{{ID: "svc", Name: ingress.ServiceName, Mode: "global"}}
			c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm", Subnets: []string{"10.9.0.0/24"}})
			c.Secrets = []dockertest.FakeSecret{{ID: "sec", Name: secretName}}
		}})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		f, ok := findingOf(res.Findings, ingressdeploy.CodeNameTaken)
		if !ok || f.Severity != ingressdeploy.SeverityError {
			t.Fatalf("findings = %+v, want ingress_name_taken as an error", res.Findings)
		}
		want := []string{"network " + ingress.NetworkName, "secret " + secretName, "service " + ingress.ServiceName}
		if !slices.Equal(f.Items, want) {
			t.Errorf("items = %q, want %q", f.Items, want)
		}
		wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodeNameTaken)
	})
	t.Run("TierPlan's", func(t *testing.T) {
		r := newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
			c.ServiceRows = []dockertest.FakeService{{ID: "svc", Name: ingress.ServiceName, Mode: "global", Labels: ingress.ObjectLabels()}}
			c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "net", Name: ingress.NetworkName, Driver: "overlay", Scope: "swarm",
				Subnets: []string{"10.9.0.0/24"}, Labels: ingress.ObjectLabels()})
			c.Secrets = []dockertest.FakeSecret{{ID: "sec", Name: secretName, Labels: ingress.ObjectLabels()}}
		}})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if _, ok := findingOf(res.Findings, ingressdeploy.CodeNameTaken); ok {
			t.Errorf("findings = %+v, want no ingress_name_taken", res.Findings)
		}
	})
}

// TestPreflightURL: TierPlan's own address must be set, and must not be a hostname the ingress routes, or the proxies
// would dial TierPlan through themselves.
func TestPreflightURL(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.URL = func() string { return "" } }})
		wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodeURLUnset)
	})
	t.Run("routed", func(t *testing.T) {
		const host = "tierplan.apps.example.com"
		for _, how := range []string{"claimed", "reserved"} {
			t.Run(how, func(t *testing.T) {
				r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
					d.URL = func() string { return "wss://" + strings.ToUpper(host) + ":7480/ingress/connect" }
				}})
				ctx := testsupport.Context(t)
				dom, err := r.istore.CreateDomain(ctx, ingressstore.CreateDomainInput{EnvID: r.envID, Suffix: "apps.example.com", Issuer: ingress.IssuerStaging, Actor: "admin"}, nil)
				if err != nil {
					t.Fatalf("CreateDomain: %v", err)
				}
				if how == "claimed" {
					if _, err := r.st.Write.Exec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('st1', ?, 'web', 'swarm', 'tester', 1)`, r.envID); err != nil {
						t.Fatalf("insert stack: %v", err)
					}
					decl := ingress.Declaration{Service: "web", Host: host, Label: "tierplan.ingress.host", Port: 80}
					if _, err := r.istore.ReplaceStackRoutes(ctx, ingressstore.ReplaceInput{EnvID: r.envID, StackID: "st1", Actor: "admin", Decls: []ingress.Declaration{decl}}); err != nil {
						t.Fatalf("ReplaceStackRoutes: %v", err)
					}
				} else if _, _, err := r.istore.Reserve(ctx, ingressstore.ReserveInput{EnvID: r.envID, DomainID: dom.ID, Hostname: host, Actor: "admin"}, nil); err != nil {
					t.Fatalf("Reserve: %v", err)
				}
				f := wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodeURLRouted)
				if !slices.Equal(f.Items, []string{host}) {
					t.Errorf("items = %q, want [%s]", f.Items, host)
				}
			})
		}
	})
	t.Run("another host passes", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if len(res.Findings) != 0 {
			t.Errorf("findings = %+v, want none", res.Findings)
		}
	})
}

// TestPreflightListsStacksToRedeploy: the stacks that hold routes and are not on the ingress network yet are a warning
// and fill PreviewResult.Redeploy, by name; with no Stacks the check adds nothing.
func TestPreflightListsStacksToRedeploy(t *testing.T) {
	r := newRig(t, rigOptions{})
	r.stacks.refs = []ingressdeploy.StackRef{
		{ID: "2", Name: "shop", HoldsClaims: true},
		{ID: "1", Name: "blog", HoldsClaims: true},
		{ID: "3", Name: "done", HoldsClaims: true, Attached: true},
	}
	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodeRedeployNeeded)
	if !ok || f.Severity != ingressdeploy.SeverityWarning || !slices.Equal(f.Items, []string{"blog", "shop"}) {
		t.Fatalf("findings = %+v, want a warning that lists blog and shop", res.Findings)
	}
	var names []string
	for _, st := range res.Redeploy {
		names = append(names, st.Name)
	}
	if !slices.Equal(names, []string{"blog", "shop"}) {
		t.Errorf("Redeploy = %q, want [blog shop]", names)
	}
	// A warning does not refuse.
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))

	none := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.Stacks = nil }})
	res, err = none.svc.Preview(testsupport.Context(t), none.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(res.Findings) != 0 || len(res.Redeploy) != 0 {
		t.Errorf("with no Stacks: findings = %+v, redeploy = %+v, want none", res.Findings, res.Redeploy)
	}
}

// TestPreflightRefusesWhilePaused: a restore pauses the environment's ingress, and until an admin resumes it no
// preview or enable goes through (D-42).
func TestPreflightRefusesWhilePaused(t *testing.T) {
	r := newRig(t, rigOptions{})
	ctx := testsupport.Context(t)
	if _, err := r.istore.SetPaused(ctx, r.envID, true, "restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	res, err := r.svc.Preview(ctx, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodePaused)
	if !ok || f.Severity != ingressdeploy.SeverityError || f.Message != "Ingress on this environment was paused by a restore. Resume it first." {
		t.Fatalf("findings = %+v, want ingress_paused with the plain sentence", res.Findings)
	}
	wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodePaused)

	if _, err := r.istore.SetPaused(ctx, r.envID, false, "admin", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	if res, err = r.svc.Preview(ctx, r.envID, ingressdeploy.EnableRequest{}); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if _, ok := findingOf(res.Findings, ingressdeploy.CodePaused); ok {
		t.Errorf("findings = %+v, want ingress_paused gone", res.Findings)
	}
}

// TestPreflightOrderIsStable: findings come in the order of the list and items sorted, so two previews of the same
// state read the same.
func TestPreflightOrderIsStable(t *testing.T) {
	r, ne := newNodeRig(t, rigOptions{
		deps: func(d *ingressdeploy.Deps) {
			d.URL = func() string { return "" }
			d.OwnHTTP01InUse = func(context.Context) (bool, error) { return true, nil }
			d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
				return envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "node-1"}, nil
			}
			d.Self = selfReads(selfService, nil)
		},
		config: func(c *dockertest.FakeConfig) {
			c.ServiceRows = []dockertest.FakeService{
				{ID: "b", Name: "zeta", Mode: "global", Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 1, PublishedPort: 443, PublishMode: "host"}}},
				{ID: "a", Name: "alpha", Mode: "global", Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 1, PublishedPort: 80, PublishMode: "ingress"}}},
			}
		},
	})
	withWorker(t, r, ne)
	ne.down["agent-2"] = true
	r.stacks.refs = []ingressdeploy.StackRef{{ID: "1", Name: "blog", HoldsClaims: true}}

	var first []ingressdeploy.Finding
	for i := range 3 {
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if i == 0 {
			first = res.Findings
			continue
		}
		if !slices.EqualFunc(first, res.Findings, func(a, b ingressdeploy.Finding) bool {
			return a.Code == b.Code && a.Message == b.Message && a.Severity == b.Severity && slices.Equal(a.Items, b.Items)
		}) {
			t.Fatalf("preview %d = %+v, want %+v", i, res.Findings, first)
		}
	}
	var codes []string
	for _, f := range first {
		codes = append(codes, f.Code)
	}
	want := []string{
		ingressdeploy.CodePortInUse, ingressdeploy.CodeNodesNotChecked, ingressdeploy.CodeHTTP01Conflict,
		ingressdeploy.CodeURLUnset, ingressdeploy.CodeRedeployNeeded,
	}
	if !slices.Equal(codes, want) {
		t.Errorf("codes = %q, want %q", codes, want)
	}
	if want := []string{"alpha publishes 80 (ingress mode)", "zeta publishes 443 (host mode)"}; !slices.Equal(first[0].Items, want) {
		t.Errorf("items = %q, want %q", first[0].Items, want)
	}
}
