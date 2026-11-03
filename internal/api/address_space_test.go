// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The address-space route (plan 10-01, NET-02, D-02, D-03, D-07 as amended, T-10-02). Docker is a
// fake daemon behind the real monitor, snapshot cache and aggregator; the router, policy and
// the address reader are the real ones.

type addressPool struct {
	CIDR               string `json:"cidr"`
	Mask               int    `json:"mask"`
	Used               int64  `json:"used"`
	Capacity           int64  `json:"capacity"`
	Percent            int    `json:"percent"`
	State              string `json:"state"`
	LargestFree        string `json:"largestFree"`
	LargestFreeSubnets int64  `json:"largestFreeSubnets"`
	Source             string `json:"source"`
	Estimated          bool   `json:"estimated"`
}

type addressOverlay struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Subnets     []string `json:"subnets"`
	Used        int64    `json:"used"`
	Capacity    int64    `json:"capacity"`
	Percent     int      `json:"percent"`
	State       string   `json:"state"`
	Measured    bool     `json:"measured"`
	Ingress     bool     `json:"ingress"`
	OutsidePool bool     `json:"outsidePool"`
}

type mapOverlap struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet"`
	Node   string `json:"node"`
}

type mapRow struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Node               string       `json:"node"`
	NodeID             string       `json:"nodeId"`
	Scope              string       `json:"scope"`
	Driver             string       `json:"driver"`
	Subnets            []string     `json:"subnets"`
	Owner              string       `json:"owner"`
	OwnerKind          string       `json:"ownerKind"`
	OwnerStackID       string       `json:"ownerStackId"`
	AttachedContainers int          `json:"attachedContainers"`
	AttachedServices   int          `json:"attachedServices"`
	Created            string       `json:"created"`
	BuiltIn            bool         `json:"builtIn"`
	Ingress            bool         `json:"ingress"`
	Usage              *mapUsage    `json:"usage"`
	Overlaps           []mapOverlap `json:"overlaps"`
	Order              int          `json:"order"`
}

type mapUsage struct {
	Used     int64  `json:"used"`
	Capacity int64  `json:"capacity"`
	Percent  int    `json:"percent"`
	State    string `json:"state"`
}

type orphanRow struct {
	NetworkID       string   `json:"networkId"`
	Name            string   `json:"name"`
	Node            string   `json:"node"`
	NodeID          string   `json:"nodeId"`
	Scope           string   `json:"scope"`
	Subnets         []string `json:"subnets"`
	Class           string   `json:"class"`
	FormerOwner     string   `json:"formerOwner"`
	FormerOwnerKind string   `json:"formerOwnerKind"`
	IdleSince       string   `json:"idleSince"`
	IdleSinceSource string   `json:"idleSinceSource"`
}

type orphansWithheld struct {
	Nodes          []string `json:"nodes"`
	Stacks         []string `json:"stacks"`
	ManagerMissing bool     `json:"managerMissing"`
}

type addressSpace struct {
	Orphans           []orphanRow      `json:"orphans"`
	OrphansWithheld   *orphansWithheld `json:"orphansWithheld"`
	Networks          []mapRow         `json:"networks"`
	NetworksTotal     int              `json:"networksTotal"`
	NetworksTruncated bool             `json:"networksTruncated"`
	Overlays          []addressOverlay `json:"overlays"`
	Ingress           *addressOverlay  `json:"ingress"`
	EnvType           string           `json:"envType"`
	Measured          bool             `json:"measured"`
	NotMeasured       string           `json:"notMeasured"`
	Pools             []addressPool    `json:"pools"`
	NotReporting      []string         `json:"notReporting"`
	GraceHours        int              `json:"graceHours"`
}

func TestAddressSpaceRoute(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.SwarmDefaultAddrPool = []string{"10.60.0.0/16"}
		cfg.Networks = []dockertest.FakeNetwork{
			{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.60.0.0/24"}},
			{ID: "ovl", Name: "ovl", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.60.1.0/24"}},
		}
	})

	raw := s.call(t, authReq{method: http.MethodGet, path: "/api/env/" + s.env.ID + "/address-space", cookie: s.cookie, csrf: s.csrf})
	if raw.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.Code, raw.Body)
	}
	if cc := raw.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := decode[addressSpace](t, &envRec{code: raw.Code, body: raw.Body.Bytes()})
	if got.EnvType != "swarm" || !got.Measured || got.NotMeasured != "" || len(got.Pools) != 1 || got.NotReporting == nil || len(got.NotReporting) != 0 {
		t.Fatalf("answer = %+v, want a measured swarm with one pool and an empty (not null) notReporting", got)
	}
	want := addressPool{
		CIDR: "10.60.0.0/16", Mask: 24, Used: 2, Capacity: 256, Percent: 0, State: "normal",
		LargestFree: "10.60.128.0/17", LargestFreeSubnets: 128, Source: "swarm", Estimated: false,
	}
	if got.Pools[0] != want {
		t.Fatalf("pool = %+v\nwant   %+v", got.Pools[0], want)
	}
}

func TestAddressSpaceRouteOverlays(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.Networks = []dockertest.FakeNetwork{
			{ID: "ingress-id", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
			{ID: "calm-id", Name: "calm", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
			{ID: "busy-id", Name: "busy", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
		}
	})
	for _, m := range []*agentConn{s.m1, s.m2} {
		m.fake.SetNetworkUsage("calm", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 8, Available: 248})
		m.fake.SetNetworkUsage("busy", dockertest.FakeSubnetUsage{Subnet: "10.0.2.0/24", InUse: 200, Available: 56})
	}

	raw := s.call(t, authReq{method: http.MethodGet, path: "/api/env/" + s.env.ID + "/address-space", cookie: s.cookie, csrf: s.csrf})
	if raw.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.Code, raw.Body)
	}
	got := decode[addressSpace](t, &envRec{code: raw.Code, body: raw.Body.Bytes()})
	want := []addressOverlay{
		{ID: "busy-id", Name: "busy", Subnets: []string{"10.0.2.0/24"}, Used: 200, Capacity: 256, Percent: 78, State: "getting_full", Measured: true},
		{ID: "calm-id", Name: "calm", Subnets: []string{"10.0.1.0/24"}, Used: 8, Capacity: 256, Percent: 3, State: "normal", Measured: true},
		// The ingress network has no Status on this engine: not measured, never a zero.
		{ID: "ingress-id", Name: "ingress", Subnets: []string{"10.0.0.0/24"}, State: "unknown", Ingress: true, OutsidePool: false},
	}
	if len(got.Overlays) != len(want) {
		t.Fatalf("overlays = %+v, want %d", got.Overlays, len(want))
	}
	for i := range want {
		g, w := got.Overlays[i], want[i]
		if !slices.Equal(g.Subnets, w.Subnets) {
			t.Errorf("overlay %d subnets = %v, want %v", i, g.Subnets, w.Subnets)
		}
		g.Subnets, w.Subnets = nil, nil
		if !reflect.DeepEqual(g, w) {
			t.Errorf("overlay %d = %+v\nwant      %+v", i, g, w)
		}
	}
	if got.Ingress == nil || got.Ingress.ID != "ingress-id" || !got.Ingress.Ingress || got.Ingress.Measured {
		t.Errorf("ingress = %+v, want the unmeasured ingress network", got.Ingress)
	}
	if !strings.Contains(raw.Body.String(), `"overlays":[`) {
		t.Errorf("body has no overlays array: %s", raw.Body)
	}
}

func TestAddressSpaceRouteAViewerMayReadAndAUserWithoutTheEnvironmentGets404(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "outsider", false)
	awaitOnline(t, f.mon, e.ID)

	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/address-space", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("a Viewer: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	cookie, csrf = f.signIn(t, "outsider", false)
	rec = f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/address-space", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a user without the environment: status = %d, want 404: %s", rec.Code, rec.Body)
	}
	unknown := f.req(t, http.MethodGet, "/api/env/nope/address-space", "")
	if unknown.code != http.StatusNotFound || unknown.ptyp != "environment_not_found" {
		t.Fatalf("an unknown environment: %d %q, want 404 environment_not_found", unknown.code, unknown.ptyp)
	}
}

func TestAddressSpaceRouteAStandaloneHostIsEstimatedWithItsSource(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 26}},
		Networks:            []dockertest.FakeNetwork{{ID: "app", Name: "app", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.64/26"}}},
	})
	awaitOnline(t, f.mon, e.ID)
	raw := f.get(t, e, "address-space")
	if raw.code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.code, raw.body)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw.body, &generic); err != nil {
		t.Fatal(err)
	}
	if _, has := generic["notMeasured"]; has {
		t.Errorf("a standalone host is estimated, not unmeasured: %s", raw.body)
	}
	got := decode[addressSpace](t, raw)
	want := addressPool{
		CIDR: "10.50.0.0/16", Mask: 26, Used: 1, Capacity: 1024, Percent: 0, State: "normal",
		LargestFree: "10.50.128.0/17", LargestFreeSubnets: 512, Source: "daemon", Estimated: true,
	}
	if got.EnvType != "standalone" || got.Measured || len(got.Pools) != 1 || got.Pools[0] != want {
		t.Fatalf("answer = %+v\nwant one daemon pool %+v, estimated, not measured", got, want)
	}

	// Without a setting of its own the daemon's pools are Docker's built-in ones, still estimated.
	f2, e2 := dashEnv(t, dockertest.FakeConfig{Hostname: "node-2", Version: "29.1.0"})
	awaitOnline(t, f2.mon, e2.ID)
	defaults := decode[addressSpace](t, f2.get(t, e2, "address-space"))
	if len(defaults.Pools) != 7 || defaults.Measured {
		t.Fatalf("defaults = %+v, want Docker's seven built-in pools", defaults)
	}
	for _, p := range defaults.Pools {
		if p.Source != "defaults" || !p.Estimated {
			t.Errorf("pool %+v, want source defaults and estimated", p)
		}
	}
}

func TestAddressSpaceRouteASwarmWithoutAManagerSaysNoManager(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if strings.HasPrefix(host, "mgr-") {
			cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		}
	})
	s.srv.Hub.Kick(s.m1.id, tunnel.DisconnectClosed)
	s.srv.Hub.Kick(s.m2.id, tunnel.DisconnectClosed)
	for _, m := range []*agentConn{s.m1, s.m2} {
		if _, err := s.srv.Hub.AwaitSession(testsupport.Context(t), m.id, func(sess *tunnel.Session) bool { return sess == nil }); err != nil {
			t.Fatalf("the session of %s never went: %v", m.id, err)
		}
	}

	rec := s.get(t, s.env.ID, "address-space")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[addressSpace](t, rec)
	if got.EnvType != "swarm" || got.Measured || got.NotMeasured != "no_manager" || got.Pools == nil || len(got.Pools) != 0 {
		t.Fatalf("answer = %+v, want a swarm that says no_manager with no pool ever read", got)
	}
	if !slices.Equal(got.NotReporting, []string{"mgr-1", "mgr-2"}) {
		t.Errorf("notReporting = %v, want the two managers that stopped answering", got.NotReporting)
	}
}

// mapRowNamed is the row of the map for a network name on a node ("" for a swarm-wide network).
func mapRowNamed(t *testing.T, got addressSpace, name, node string) mapRow {
	t.Helper()
	for _, r := range got.Networks {
		if r.Name == name && (node == "" || r.Node == node) {
			return r
		}
	}
	t.Fatalf("no row %q on %q in %+v", name, node, got.Networks)
	return mapRow{}
}

// mapSwarm is a swarm whose managers share these networks: an ingress, a stack overlay
// (alpha_default, linked to a TierPlan stack), a forged one that only claims the stack's name, a
// service-only overlay, and per node a host network; mgr-2 also has a compose project's bridge.
// tune runs on the networks before the agents connect. The returned function gives the stack's
// networks their label once the stack exists (its ID is only known then) and refreshes the reads.
func mapSwarm(t *testing.T, extra func(host string, nets []dockertest.FakeNetwork) []dockertest.FakeNetwork) (*swarmFix, func() string) {
	t.Helper()
	cfgs := map[string]*dockertest.FakeConfig{}
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfgs[host] = cfg
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.ServiceRows = []dockertest.FakeService{
			{ID: "svc-z", Name: "zeta", Mode: "replicated", Running: 1, Desired: 1, Networks: []string{"svc-only"}},
			{ID: "svc-a", Name: "alpha-svc", Mode: "replicated", Running: 1, Desired: 1, Networks: []string{"svc-only"}},
		}
		nets := []dockertest.FakeNetwork{
			{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
			{ID: "alpha-net", Name: "alpha_default", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.0/24"}},
			{ID: "forged-net", Name: "forged", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.3.0/24"},
				Labels: map[string]string{"com.docker.stack.namespace": "alpha"}},
			{ID: "svc-only", Name: "svc_only", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.2.0/24"}},
			{ID: "host-" + host, Name: "host", Driver: "host", Scope: "local"},
		}
		if host == "mgr-2" {
			nets = append(nets, dockertest.FakeNetwork{ID: "blog-net", Name: "blog_default", Driver: "bridge", Scope: "local",
				Subnets: []string{"172.20.0.0/16"}, Labels: map[string]string{"com.docker.compose.project": "blog"}})
		}
		if extra != nil {
			nets = extra(host, nets)
		}
		cfg.Networks = nets
	})
	for _, m := range []*agentConn{s.m1, s.m2} {
		m.fake.SetNetworkUsage("alpha_default", dockertest.FakeSubnetUsage{Subnet: "10.0.1.0/24", InUse: 8, Available: 248})
	}
	link := func() string {
		id := s.seedStack(t, s.env.ID, "alpha", stacks.TargetSwarm)
		for host, m := range map[string]*agentConn{"mgr-1": s.m1, "mgr-2": s.m2} {
			cfg := *cfgs[host]
			cfg.Networks = slices.Clone(cfg.Networks)
			for i, n := range cfg.Networks {
				if n.Name == "alpha_default" {
					cfg.Networks[i].Labels = map[string]string{"com.docker.stack.namespace": "alpha", "tierplan.stack.id": id}
				}
			}
			m.fake.Reconfigure(cfg)
		}
		if _, _, err := s.agg.FreshSnapshot(testsupport.Context(t), s.env.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.clusters.Refresh(testsupport.Context(t), s.env.ID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	return s, link
}

func TestAddressSpaceMapRows(t *testing.T) {
	t.Parallel()
	s, link := mapSwarm(t, nil)
	stackID := link()

	raw := s.get(t, s.env.ID, "address-space")
	if raw.code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.code, raw.body)
	}
	got := decode[addressSpace](t, raw)

	// A swarm network once; a host network on every node (two managers and a worker); the bridge of the
	// worker (the fixture's own) and mgr-2's compose bridge as local rows.
	counts := map[string]int{}
	for _, r := range got.Networks {
		counts[r.Name]++
	}
	if counts["alpha_default"] != 1 || counts["ingress"] != 1 || counts["host"] != 2 || counts["blog_default"] != 1 {
		t.Errorf("rows per name = %v, want swarm networks once and local ones per node", counts)
	}
	if got.NetworksTotal != len(got.Networks) || got.NetworksTruncated {
		t.Errorf("total/truncated = %d/%v with %d rows", got.NetworksTotal, got.NetworksTruncated, len(got.Networks))
	}

	alpha := mapRowNamed(t, got, "alpha_default", "")
	if alpha.Scope != "swarm" || alpha.Node != "Cluster" || alpha.Owner != "alpha" || alpha.OwnerKind != "stack" || alpha.OwnerStackID != stackID {
		t.Errorf("alpha_default = %+v, want a swarm network owned by the TierPlan stack %s", alpha, stackID)
	}
	if alpha.Usage == nil || alpha.Usage.Used != 8 || alpha.Usage.Capacity != 256 || alpha.Usage.State != "normal" {
		t.Errorf("alpha_default usage = %+v, want 8 of 256", alpha.Usage)
	}
	// Only a stack ID the environment's stack states is a link: the forged label names the stack and has no ID.
	forged := mapRowNamed(t, got, "forged", "")
	if forged.OwnerKind != "stack" || forged.Owner != "alpha" || forged.OwnerStackID != "" {
		t.Errorf("forged = %+v, want the stack named and no link", forged)
	}

	blog := mapRowNamed(t, got, "blog_default", "")
	if blog.Scope != "local" || blog.Node != "mgr-2" || blog.OwnerKind != "project" || blog.Owner != "blog" || blog.OwnerStackID != "" || blog.Usage != nil {
		t.Errorf("blog_default = %+v, want a local bridge of mgr-2 owned by project blog", blog)
	}

	svc := mapRowNamed(t, got, "svc_only", "")
	if svc.OwnerKind != "service" || svc.Owner != "alpha-svc" || svc.AttachedServices != 2 {
		t.Errorf("svc_only = %+v, want the first service by name as owner and 2 services", svc)
	}

	host := mapRowNamed(t, got, "host", "mgr-1")
	if host.OwnerKind != "none" || host.Owner != "" || !host.BuiltIn || len(host.Subnets) != 0 || host.Subnets == nil {
		t.Errorf("host = %+v, want a built-in row with an empty (not null) subnets list", host)
	}
	if ing := mapRowNamed(t, got, "ingress", ""); !ing.Ingress || !ing.BuiltIn {
		t.Errorf("ingress = %+v, want ingress and built-in", ing)
	}
	for _, r := range got.Networks {
		if r.Overlaps == nil {
			t.Errorf("%s: overlaps is null, want a list", r.Name)
		}
	}
	// Nothing but the owner values leaves: the label sets stay on the server.
	if strings.Contains(string(raw.body), "tierplan.stack.id") || strings.Contains(string(raw.body), "com.docker.compose.project") {
		t.Errorf("a label key leaked into the answer: %s", raw.body)
	}
}

func TestAddressSpaceMapOverlaps(t *testing.T) {
	t.Parallel()
	s, link := mapSwarm(t, func(host string, nets []dockertest.FakeNetwork) []dockertest.FakeNetwork {
		nets = append(nets,
			// Swarm against swarm: inside alpha_default. Both name each other.
			dockertest.FakeNetwork{ID: "clash-net", Name: "clash", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.0.1.128/25"}},
			// The same local subnet on two nodes is two hosts, not an overlap.
			dockertest.FakeNetwork{ID: "dup-" + host, Name: "dup", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/16"}},
		)
		if host == "mgr-2" {
			// A local network inside a swarm one overlaps it, and names its node.
			nets = append(nets, dockertest.FakeNetwork{ID: "inner-net", Name: "inner", Driver: "bridge", Scope: "local", Subnets: []string{"10.0.2.0/25"}})
		}
		return nets
	})
	link()
	got := decode[addressSpace](t, s.get(t, s.env.ID, "address-space"))

	overlaps := func(name, node string) []mapOverlap { return mapRowNamed(t, got, name, node).Overlaps }
	if want := []mapOverlap{{Name: "clash", Subnet: "10.0.1.128/25"}}; !slices.Equal(overlaps("alpha_default", ""), want) {
		t.Errorf("alpha_default overlaps %+v, want %+v", overlaps("alpha_default", ""), want)
	}
	if want := []mapOverlap{{Name: "alpha_default", Subnet: "10.0.1.0/24"}}; !slices.Equal(overlaps("clash", ""), want) {
		t.Errorf("clash overlaps %+v, want %+v", overlaps("clash", ""), want)
	}
	if want := []mapOverlap{{Name: "inner", Subnet: "10.0.2.0/25", Node: "mgr-2"}}; !slices.Equal(overlaps("svc_only", ""), want) {
		t.Errorf("svc_only overlaps %+v, want %+v", overlaps("svc_only", ""), want)
	}
	if want := []mapOverlap{{Name: "svc_only", Subnet: "10.0.2.0/24"}}; !slices.Equal(overlaps("inner", ""), want) {
		t.Errorf("inner overlaps %+v, want %+v", overlaps("inner", ""), want)
	}
	// Adjacent (ingress 10.0.0.0/24 next to alpha_default 10.0.1.0/24) and same-subnet-on-other-host rows are clean.
	for _, c := range [][2]string{{"ingress", ""}, {"dup", "mgr-1"}, {"dup", "mgr-2"}, {"blog_default", ""}, {"forged", ""}} {
		if o := overlaps(c[0], c[1]); len(o) != 0 {
			t.Errorf("%s on %q overlaps %+v, want none", c[0], c[1], o)
		}
	}

	// The order: numeric start address, the shorter mask first, rows with no subnet last, and every
	// row's order is its index.
	var withSubnet, bare []string
	for i, r := range got.Networks {
		if r.Order != i {
			t.Errorf("row %d (%s) has order %d", i, r.Name, r.Order)
		}
		if len(r.Subnets) == 0 {
			bare = append(bare, r.Name)
			continue
		}
		if len(bare) > 0 {
			t.Errorf("%s has a subnet and follows %v, which have none", r.Name, bare)
		}
		withSubnet = append(withSubnet, r.Name)
	}
	want := []string{"ingress", "alpha_default", "clash", "svc_only", "inner", "forged", "blog_default", "dup", "dup"}
	if !slices.Equal(withSubnet, want) {
		t.Errorf("rows with a subnet = %v, want %v", withSubnet, want)
	}
	if len(bare) == 0 {
		t.Error("host and bridge rows with no subnet are missing")
	}
}

func TestAddressSpaceMapCap(t *testing.T) {
	t.Parallel()
	const total = 5001
	nets := make([]dockertest.FakeNetwork, 0, total)
	for i := range total - 2 {
		nets = append(nets, dockertest.FakeNetwork{
			ID: fmt.Sprintf("n%05d", i), Name: fmt.Sprintf("net-%05d", i), Driver: "bridge", Scope: "local",
			Subnets: []string{fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)},
		})
	}
	// The two highest addresses overlap each other. In address order they are the last two rows, so the
	// cap cuts the second and the first (row 5000) must still name it.
	nets = append(nets,
		dockertest.FakeNetwork{ID: "tail-a", Name: "tail-a", Driver: "bridge", Scope: "local", Subnets: []string{"250.0.0.0/24"}},
		dockertest.FakeNetwork{ID: "tail-b", Name: "tail-b", Driver: "bridge", Scope: "local", Subnets: []string{"250.0.0.128/25"}},
	)
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", Networks: nets})
	awaitOnline(t, f.mon, e.ID)

	raw := f.get(t, e, "address-space")
	if raw.code != http.StatusOK {
		t.Fatalf("status = %d: %.300s", raw.code, raw.body)
	}
	got := decode[addressSpace](t, raw)
	if len(got.Networks) != 5000 || got.NetworksTotal != total || !got.NetworksTruncated {
		t.Fatalf("rows/total/truncated = %d/%d/%v, want 5000/%d/true", len(got.Networks), got.NetworksTotal, got.NetworksTruncated, total)
	}
	last := got.Networks[4999]
	if last.Name != "tail-a" || len(last.Overlaps) != 1 || last.Overlaps[0].Name != "tail-b" || last.Overlaps[0].Node != "node-1" {
		t.Errorf("last row = %s overlaps %+v, want tail-a naming tail-b although tail-b is cut", last.Name, last.Overlaps)
	}
	if got.Networks[0].Order != 0 || last.Order != 4999 {
		t.Errorf("orders = %d..%d, want 0..4999", got.Networks[0].Order, last.Order)
	}

	// Under the cap nothing is truncated.
	f2, e2 := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", Networks: nets[:10]})
	awaitOnline(t, f2.mon, e2.ID)
	small := decode[addressSpace](t, f2.get(t, e2, "address-space"))
	if len(small.Networks) != 10 || small.NetworksTotal != 10 || small.NetworksTruncated {
		t.Errorf("small = %d/%d/%v, want 10/10/false", len(small.Networks), small.NetworksTotal, small.NetworksTruncated)
	}
}

func TestAddressSpaceMapNotReporting(t *testing.T) {
	t.Parallel()
	s, link := mapSwarm(t, nil)
	link()
	before := decode[addressSpace](t, s.get(t, s.env.ID, "address-space"))
	if len(before.NotReporting) != 0 || mapRowNamed(t, before, "bridge", "wrk-1").Node != "wrk-1" {
		t.Fatalf("before: notReporting %v, want every node answering and the worker's bridge listed", before.NotReporting)
	}

	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	if _, err := s.srv.Hub.AwaitSession(testsupport.Context(t), s.w.id, func(sess *tunnel.Session) bool { return sess == nil }); err != nil {
		t.Fatalf("the worker's session never went: %v", err)
	}
	after := decode[addressSpace](t, s.get(t, s.env.ID, "address-space"))
	if !slices.Equal(after.NotReporting, []string{"wrk-1"}) {
		t.Fatalf("notReporting = %v, want the worker", after.NotReporting)
	}
	// Only the worker's rows are gone: every other row is as it was, order included, since its
	// position among the rows that remain does not move for a worker network with no subnet.
	var want []mapRow
	for _, r := range before.Networks {
		if r.Node != "wrk-1" {
			want = append(want, r)
		}
	}
	for i := range want {
		want[i].Order = i
	}
	if after.NetworksTotal != len(want) || len(after.Networks) != len(want) {
		t.Fatalf("rows = %d (total %d), want %d", len(after.Networks), after.NetworksTotal, len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(after.Networks[i], want[i]) {
			t.Errorf("row %d = %+v\nwant     %+v", i, after.Networks[i], want[i])
		}
	}
}

func TestAddressSpaceOrphansRoute(t *testing.T) {
	t.Parallel()
	long := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	f, e := dashEnv(t, dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Created: long},
			{ID: "idle", Name: "idle", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}, Created: long},
			{ID: "gone", Name: "shop_default", Driver: "bridge", Scope: "local", Created: long,
				Labels: map[string]string{"com.docker.compose.project": "shop"}},
			{ID: "fresh", Name: "fresh", Driver: "bridge", Scope: "local", Created: time.Now()},
		},
	})
	awaitOnline(t, f.mon, e.ID)
	raw := f.get(t, e, "address-space")
	if raw.code != http.StatusOK {
		t.Fatalf("status = %d: %s", raw.code, raw.body)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw.body, &generic); err != nil {
		t.Fatal(err)
	}
	if _, has := generic["orphansWithheld"]; has {
		t.Errorf("nothing was withheld, so orphansWithheld is absent: %s", raw.body)
	}
	got := decode[addressSpace](t, raw)
	if got.Orphans == nil || len(got.Orphans) != 2 {
		t.Fatalf("orphans = %+v, want the project-labelled network and the unlabelled one, in that order; not bridge, not the one made just now", got.Orphans)
	}
	o, u := got.Orphans[0], got.Orphans[1]
	if o.Name != "shop_default" || o.Class != "orphaned" || o.FormerOwner != "shop" || o.FormerOwnerKind != "project" || o.IdleSinceSource != "created" || o.Scope != "local" || o.Node != "node-1" {
		t.Errorf("orphaned row = %+v", o)
	}
	if u.Name != "idle" || u.Class != "unused" || u.FormerOwner != "" || u.FormerOwnerKind != "" || u.IdleSince != "2020-01-01T00:00:00Z" || !slices.Equal(u.Subnets, []string{"172.30.0.0/24"}) {
		t.Errorf("unused row = %+v", u)
	}
}
