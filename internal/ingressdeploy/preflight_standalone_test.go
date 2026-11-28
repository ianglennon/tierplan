// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// withBase adds to what the fake host holds besides the proxy's own objects: other people's containers, networks and
// volumes.
func (r *standaloneRig) withBase(fn func(*dockertest.FakeConfig)) {
	r.t.Helper()
	r.sys.mu.Lock()
	defer r.sys.mu.Unlock()
	fn(&r.sys.base)
	r.sys.reconfigure()
}

// bound is an inspect document whose container was created with the -p bindings given as container port and host port.
func bound(t *testing.T, pairs ...[2]string) dockertest.FakeInspect {
	t.Helper()
	m := network.PortMap{}
	for _, p := range pairs {
		port, err := network.ParsePort(p[0])
		if err != nil {
			t.Fatalf("ParsePort(%q): %v", p[0], err)
		}
		m[port] = append(m[port], network.PortBinding{HostPort: p[1]})
	}
	return dockertest.FakeInspect{Document: &container.InspectResponse{HostConfig: &container.HostConfig{PortBindings: m}}}
}

// previewStandalone is the preview of a standalone rig, which must not fail.
func previewStandalone(t *testing.T, r *standaloneRig, req ingressdeploy.EnableRequest) ingressdeploy.PreviewResult {
	t.Helper()
	res, err := r.svc.Preview(testsupport.Context(t), r.envID, req)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	return res
}

// TestStandalonePreflightRefusesPublishedPorts is the tracer: every container of the host that binds host TCP 80 or 443,
// whatever its state, refuses the preview and the enable and is named with its state; a UDP 443 and a TCP 8443 do not
// clash (D-12, D-31).
func TestStandalonePreflightRefusesPublishedPorts(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "r1", Names: []string{"/web"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8080, Public: 80, Type: "tcp"}}},
			// TierPlan's own server container is a container like any other.
			dockertest.FakeContainer{ID: "r2", Names: []string{"/tierplan"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 7480, Public: 443, Type: "tcp"}}},
			// An engine lists a created or exited container with no ports, so only its bindings show them.
			dockertest.FakeContainer{ID: "c1", Names: []string{"/fresh"}, State: "created", Inspect: bound(t, [2]string{"80/tcp", "80"})},
			dockertest.FakeContainer{ID: "e1", Names: []string{"/old-proxy"}, State: "exited", Inspect: bound(t, [2]string{"443/tcp", "443"})},
			dockertest.FakeContainer{ID: "u1", Names: []string{"/quic"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 443, Public: 443, Type: "udp"}}},
			dockertest.FakeContainer{ID: "u2", Names: []string{"/stopped-quic"}, State: "exited", Inspect: bound(t, [2]string{"443/udp", "443"})},
			dockertest.FakeContainer{ID: "o1", Names: []string{"/other"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8443, Public: 8443, Type: "tcp"}}},
		)
	})

	want := []string{"fresh publishes 80 (created)", "old-proxy publishes 443 (exited)", "tierplan publishes 443 (running)", "web publishes 80 (running)"}
	f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("no ingress_port_in_use error in the preview")
	}
	if !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	got := wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)
	if !slices.Equal(got.Items, want) {
		t.Errorf("Enable's items = %q, want %q", got.Items, want)
	}
	if r.sys.deployCount() != 0 {
		t.Error("a refused enable deployed")
	}

	// A host whose only bindings are UDP 443 and TCP 8443 has the ports free.
	free := newStandaloneRig(t, standaloneRigOptions{})
	free.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "u1", Names: []string{"/quic"}, State: "running",
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 443, Public: 443, Type: "udp"}}},
			dockertest.FakeContainer{ID: "u2", Names: []string{"/stopped-quic"}, State: "exited", Inspect: bound(t, [2]string{"443/udp", "443"})},
			dockertest.FakeContainer{ID: "o1", Names: []string{"/other"}, State: "exited", Inspect: bound(t, [2]string{"8443/tcp", "8443"})},
		)
	})
	if _, ok := findingOf(previewStandalone(t, free, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse); ok {
		t.Error("a UDP 443 or a TCP 8443 refused the enable")
	}
}

// TestStandalonePreflightIgnoresItsOwnProxy: the proxy's container, whatever its state, never counts as a holder of
// its own ports, so an enable that is run again after a failed one does not refuse itself; a container that only has
// the project's name does count.
func TestStandalonePreflightIgnoresItsOwnProxy(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	own := ingress.ObjectLabels()
	own["com.docker.compose.project"] = ingress.ProjectName
	own["com.docker.compose.service"] = ingress.ProxyComposeService
	r.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "p1", Names: []string{"/tierplan-ingress-proxy-1"}, State: "running", Labels: own,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 80, Type: "tcp"}, {IP: "0.0.0.0", Private: 443, Public: 443, Type: "tcp"}}},
		)
	})
	for _, f := range previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings {
		if f.Code == ingressdeploy.CodePortInUse {
			t.Fatalf("finding = %+v, want the proxy's own container left out", f)
		}
	}

	// The same proxy stopped, its bindings known only to its inspect.
	stopped := newStandaloneRig(t, standaloneRigOptions{})
	stopped.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "p1", Names: []string{"/tierplan-ingress-proxy-1"}, State: "exited", Labels: own,
				Inspect: bound(t, [2]string{"80/tcp", "80"}, [2]string{"443/tcp", "443"})},
		)
	})
	for _, f := range previewStandalone(t, stopped, ingressdeploy.EnableRequest{}).Findings {
		if f.Code == ingressdeploy.CodePortInUse {
			t.Fatalf("finding = %+v, want the stopped proxy left out", f)
		}
	}

	// A container that carries the component label but is not the proxy's service is somebody else's.
	impostor := newStandaloneRig(t, standaloneRigOptions{})
	forged := ingress.ObjectLabels()
	impostor.withBase(func(c *dockertest.FakeConfig) {
		c.Containers = append(c.Containers,
			dockertest.FakeContainer{ID: "x1", Names: []string{"/lookalike"}, State: "running", Labels: forged,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 80, Type: "tcp"}}},
		)
	})
	f, ok := findingOf(previewStandalone(t, impostor, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodePortInUse)
	if !ok || !slices.Equal(f.Items, []string{"lookalike publishes 80 (running)"}) {
		t.Errorf("finding = %+v, want the container that only carries the label named", f)
	}
}

// subnetCodes are the codes of the findings of the preview that refuse about the ingress network's block.
func subnetCodes(fs []ingressdeploy.Finding) []string {
	var out []string
	for _, f := range fs {
		switch f.Code {
		case ingressdeploy.CodeNoAddressSpace, ingressdeploy.CodeSubnetInvalid, ingressdeploy.CodeSubnetOverlaps:
			out = append(out, f.Code)
		}
	}
	return out
}

// TestStandalonePreflightSubnet: the bridge's block is checked against the host's own pools and networks, a live owned
// network skips the check (D-12).
func TestStandalonePreflightSubnet(t *testing.T) {
	full := stubAddressing{in: envs.AllocationInputs{
		Pools:    []netspace.Pool{{Base: netip.MustParsePrefix("10.0.0.0/24"), Size: 24}},
		Occupied: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")},
	}}
	customer := func(c *dockertest.FakeConfig) {
		c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "n1", Name: "customer", Driver: "bridge", Scope: "local", Subnets: []string{"10.5.0.0/24"}})
	}

	t.Run("no free block", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) { d.AddressSpaces = full }})
		f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodeNoAddressSpace)
		if !ok || f.Severity != ingressdeploy.SeverityError || !strings.Contains(f.Message, "/24") {
			t.Fatalf("finding = %+v, want an error that names the /24 block", f)
		}
		wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodeNoAddressSpace)
	})
	t.Run("not an ipv4 block", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{})
		for _, bad := range []string{"10.1.2.3", "10.0.0.0/33", "fd00::/64", "ten"} {
			if got := subnetCodes(previewStandalone(t, r, ingressdeploy.EnableRequest{Subnet: bad}).Findings); !slices.Equal(got, []string{ingressdeploy.CodeSubnetInvalid}) {
				t.Errorf("%q: codes = %q, want ingress_subnet_invalid", bad, got)
			}
		}
		wantRefusal(t, r.rig, ingressdeploy.EnableRequest{Subnet: "10.1.2.3"}, ingressdeploy.CodeSubnetInvalid)
	})
	t.Run("overlaps a network of the host", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{})
		r.withBase(customer)
		f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{Subnet: "10.5.0.0/22"}).Findings, ingressdeploy.CodeSubnetOverlaps)
		if !ok || len(f.Items) != 1 || !strings.Contains(f.Items[0], "customer") || !strings.Contains(f.Items[0], "10.5.0.0/24") {
			t.Fatalf("finding = %+v, want it to name the network", f)
		}
		wantRefusal(t, r.rig, ingressdeploy.EnableRequest{Subnet: "10.5.0.0/22"}, ingressdeploy.CodeSubnetOverlaps)
	})
	t.Run("overlaps an occupied block the read does not name", func(t *testing.T) {
		occupied := stubAddressing{in: envs.AllocationInputs{
			Pools:    []netspace.Pool{{Base: netip.MustParsePrefix("10.0.0.0/16"), Size: 24}},
			Occupied: []netip.Prefix{netip.MustParsePrefix("10.7.0.0/24")},
		}}
		r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) { d.AddressSpaces = occupied }})
		if got := subnetCodes(previewStandalone(t, r, ingressdeploy.EnableRequest{Subnet: "10.7.0.0/25"}).Findings); !slices.Equal(got, []string{ingressdeploy.CodeSubnetOverlaps}) {
			t.Errorf("codes = %q, want ingress_subnet_overlaps", got)
		}
	})
	t.Run("a free block passes", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{})
		r.withBase(customer)
		for _, subnet := range []string{"", "10.8.0.0/24"} {
			if res := previewStandalone(t, r, ingressdeploy.EnableRequest{Subnet: subnet}); len(res.Findings) != 0 {
				t.Errorf("subnet %q: findings = %+v, want none", subnet, res.Findings)
			}
		}
	})
	t.Run("a live owned network skips the checks", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{
			host: hostFixture{Proxy: &proxyFixture{Image: "img"}, NetworkSubnet: "10.9.0.0/24", Volume: true},
			deps: func(d *ingressdeploy.Deps) { d.AddressSpaces = full },
		})
		for _, subnet := range []string{"", "10.9.0.0/24", "10.1.2.3"} {
			if got := subnetCodes(previewStandalone(t, r, ingressdeploy.EnableRequest{Subnet: subnet}).Findings); len(got) != 0 {
				t.Errorf("subnet %q: codes = %q, want none", subnet, got)
			}
		}
	})
}

// TestStandalonePreflightHTTP01: TierPlan's own certificate on HTTP-01 needs port 80 on the host it runs on, which is the
// local-socket environment, or an agent host with the local engine's ID (D-04). Where TierPlan's own container is found, it
// connects that container to the self network and the carve-out keeps the renewal working, which is a notice; where it is not
// found, the switch is refused with the S4 sentence (D-12).
func TestStandalonePreflightHTTP01(t *testing.T) {
	type want int
	const (
		nothing want = iota
		notice
		refused
		warned
	)
	found := ingressdeploy.SelfFacts{ContainerID: "c1", Networks: []string{"bridge"}}
	cases := []struct {
		name      string
		inUse     bool
		inUseErr  error
		local     bool
		envEngine string
		member    envs.SwarmMembership
		selfErr   error
		facts     ingressdeploy.SelfFacts
		want      want
	}{
		{name: "http-01, the local environment, its container found", inUse: true, local: true, facts: found, want: notice},
		{name: "http-01, the local environment whatever the probe says", inUse: true, local: true, member: envs.SwarmMembership{Known: true, EngineID: "other"}, facts: found, want: notice},
		{name: "http-01, an agent host with the local engine, its container found", inUse: true, envEngine: "e1", member: envs.SwarmMembership{Known: true, EngineID: "e1"}, facts: found, want: notice},
		{name: "http-01, the local environment, its container not found", inUse: true, local: true, selfErr: errors.New("not in a container"), want: refused},
		{name: "http-01, an agent host with the local engine, an empty container ID", inUse: true, envEngine: "e1", member: envs.SwarmMembership{Known: true, EngineID: "e1"}, want: refused},
		{name: "http-01, an agent host of another engine", inUse: true, envEngine: "e2", member: envs.SwarmMembership{Known: true, EngineID: "e1"}, facts: found, want: nothing},
		{name: "http-01, the host's engine ID not known", inUse: true, member: envs.SwarmMembership{Known: true, EngineID: "e1"}, facts: found, want: warned},
		{name: "http-01, the local engine cannot be read", inUse: true, envEngine: "e1", member: envs.SwarmMembership{}, facts: found, want: warned},
		{name: "http-01, the local engine has no ID", inUse: true, envEngine: "e1", member: envs.SwarmMembership{Known: true}, facts: found, want: warned},
		{name: "the own certificate cannot be read", inUseErr: errors.New("no settings"), envEngine: "e1", member: envs.SwarmMembership{Known: true, EngineID: "e1"}, facts: found, want: warned},
		{name: "the own certificate cannot be read, the local environment", inUseErr: errors.New("no settings"), local: true, facts: found, want: warned},
		{name: "not http-01, the local environment", local: true, facts: found, want: nothing},
		{name: "not http-01, the same engine", envEngine: "e1", member: envs.SwarmMembership{Known: true, EngineID: "e1"}, facts: found, want: nothing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) {
				d.OwnHTTP01InUse = func(context.Context) (bool, error) { return tc.inUse, tc.inUseErr }
				d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) { return tc.member, nil }
				d.Self = func(context.Context) (ingressdeploy.SelfFacts, error) { return tc.facts, tc.selfErr }
				d.SelfURLs = func() ingressdeploy.SelfURLs {
					return ingressdeploy.SelfURLs{PublicURL: "https://box.example.org", AgentURL: "https://box.lan:7480"}
				}
			}})
			r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Local, i.EngineID = tc.local, tc.envEngine })
			res := previewStandalone(t, r, ingressdeploy.EnableRequest{})
			conflict, isRefused := findingOf(res.Findings, ingressdeploy.CodeHTTP01Conflict)
			unknown, isWarned := findingOf(res.Findings, ingressdeploy.CodeHTTP01Unknown)
			// The carve-out is the preview's notice, never one of its findings (UI-SPEC S4).
			if f, ok := findingOf(res.Findings, ingressdeploy.CodeHTTP01CarveOut); ok {
				t.Errorf("the carve-out is a finding %+v; it belongs in the notice", f)
			}
			isNotice := res.Notice != ""
			switch tc.want {
			case notice:
				if isRefused || isWarned || !isNotice {
					t.Fatalf("findings = %+v, notice %q, want only the carve-out notice", res.Findings, res.Notice)
				}
				if !strings.Contains(res.Notice, "requests for box.example.org and box.lan to TierPlan, over the tierplan-self network") {
					t.Errorf("notice = %q, want the notice naming both hosts", res.Notice)
				}
			case refused:
				if !isRefused || isWarned || isNotice || conflict.Severity != ingressdeploy.SeverityError {
					t.Fatalf("findings = %+v, want ingress_http01_conflict as an error", res.Findings)
				}
				wantMsg := "TierPlan runs on this host and its own certificate uses the HTTP-01 challenge, which needs port 80 that the ingress takes. " +
					"TierPlan couldn't find its own container to connect it to tierplan-self. Switch TierPlan's own certificate to DNS-01 first."
				if conflict.Message != wantMsg {
					t.Errorf("message = %q, want %q", conflict.Message, wantMsg)
				}
				wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodeHTTP01Conflict)
			case warned:
				if isRefused || isNotice || !isWarned || unknown.Severity != ingressdeploy.SeverityWarning {
					t.Fatalf("findings = %+v, want ingress_http01_unknown as a warning and no refusal", res.Findings)
				}
			default:
				if isRefused || isWarned || isNotice {
					t.Fatalf("findings = %+v, want none of the HTTP-01 findings", res.Findings)
				}
			}
		})
	}
}

// TestStandalonePreflightNamesTaken: a container of the proxy's project, a network named like its bridge and a volume
// named like its cache that are not TierPlan's refuse; TierPlan's own are reused (D-12, Pitfall 11).
func TestStandalonePreflightNamesTaken(t *testing.T) {
	t.Run("not TierPlan's", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{})
		r.withBase(func(c *dockertest.FakeConfig) {
			c.Containers = append(c.Containers, dockertest.FakeContainer{ID: "f1", Names: []string{"/old-stack-proxy-1"}, State: "exited",
				Labels: map[string]string{"com.docker.compose.project": ingress.ProjectName, "com.docker.compose.service": "proxy"}})
			c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "f2", Name: ingress.NetworkName, Driver: "bridge", Scope: "local", Subnets: []string{"10.9.0.0/24"}})
			c.Volumes = append(c.Volumes, dockertest.FakeVolume{Name: ingress.VolumeName, Driver: "local"})
		})
		want := []string{"container old-stack-proxy-1", "network " + ingress.NetworkName, "volume " + ingress.VolumeName}
		f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodeNameTaken)
		if !ok || f.Severity != ingressdeploy.SeverityError || !slices.Equal(f.Items, want) {
			t.Fatalf("finding = %+v, want ingress_name_taken as an error that lists %q", f, want)
		}
		wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, ingressdeploy.CodeNameTaken)
		if r.sys.deployCount() != 0 {
			t.Error("a refused enable deployed")
		}
	})
	t.Run("one at a time", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			add  func(*dockertest.FakeConfig)
			want string
		}{
			{"a container", func(c *dockertest.FakeConfig) {
				c.Containers = append(c.Containers, dockertest.FakeContainer{ID: "f1", Names: []string{"/squatter"}, State: "running",
					Labels: map[string]string{"com.docker.compose.project": ingress.ProjectName}})
			}, "container squatter"},
			{"a network", func(c *dockertest.FakeConfig) {
				c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "f2", Name: ingress.NetworkName, Driver: "bridge", Scope: "local", Subnets: []string{"10.9.0.0/24"}})
			}, "network " + ingress.NetworkName},
			{"a volume", func(c *dockertest.FakeConfig) {
				c.Volumes = append(c.Volumes, dockertest.FakeVolume{Name: ingress.VolumeName, Driver: "local"})
			}, "volume " + ingress.VolumeName},
		} {
			r := newStandaloneRig(t, standaloneRigOptions{})
			r.withBase(tc.add)
			f, ok := findingOf(previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings, ingressdeploy.CodeNameTaken)
			if !ok || !slices.Equal(f.Items, []string{tc.want}) {
				t.Errorf("%s: finding = %+v, want %q", tc.name, f, tc.want)
			}
		}
	})
	t.Run("TierPlan's", func(t *testing.T) {
		r := newStandaloneRig(t, standaloneRigOptions{host: hostFixture{Proxy: &proxyFixture{Image: "img"}, NetworkSubnet: "10.9.0.0/24", Volume: true}})
		for _, f := range previewStandalone(t, r, ingressdeploy.EnableRequest{}).Findings {
			if f.Code == ingressdeploy.CodeNameTaken {
				t.Errorf("finding = %+v, want TierPlan's own objects reused", f)
			}
		}
	})
}

// TestStandalonePreflightListsRedeploy: the stacks that hold routes and are not attached to the ingress are a warning and
// the preview's Redeploy list, by name, and are never changed (D-18).
func TestStandalonePreflightListsRedeploy(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	r.stacks.refs = []ingressdeploy.StackRef{
		{ID: "2", Name: "shop", HoldsClaims: true},
		{ID: "1", Name: "blog", HoldsClaims: true},
		{ID: "3", Name: "done", HoldsClaims: true, Attached: true},
	}
	res := previewStandalone(t, r, ingressdeploy.EnableRequest{})
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
	if hasError(res.Findings) {
		t.Errorf("findings = %+v, want no refusal", res.Findings)
	}
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
}

// hasError reports whether any finding refuses.
func hasError(fs []ingressdeploy.Finding) bool {
	for _, f := range fs {
		if f.Severity == ingressdeploy.SeverityError {
			return true
		}
	}
	return false
}
