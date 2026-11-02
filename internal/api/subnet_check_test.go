// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// The subnet check (plan 10-13, NET-05, D-14, D-15, T-10-34, T-10-35). Docker is a fake daemon
// behind the real monitor, snapshot cache and aggregator; the router, policy and address reader
// are the real ones.

type subnetCheckAnswer struct {
	Suggestion *struct {
		Subnet string `json:"subnet"`
		Pool   string `json:"pool"`
	} `json:"suggestion"`
	NoFreeBlock *struct {
		Mask int    `json:"mask"`
		Pool string `json:"pool"`
	} `json:"noFreeBlock"`
	Needed            *int `json:"needed"`
	RecommendedMask   *int `json:"recommendedMask"`
	RecommendedUsable *int `json:"recommendedUsable"`
	TooManyTasks      bool `json:"tooManyTasks"`
	SubnetError       *struct {
		Code      string `json:"code"`
		Corrected string `json:"corrected"`
	} `json:"subnetError"`
	IPv6NotChecked bool            `json:"ipv6NotChecked"`
	Findings       []subnetFinding `json:"findings"`
	RouteChecks    []routeCheck    `json:"routeChecks"`
}

type routeCheck struct {
	Node   string `json:"node"`
	NodeID string `json:"nodeId"`
	State  string `json:"state"`
	Local  bool   `json:"local"`
	Fix    string `json:"fix"`
}

type subnetFinding struct {
	Kind        string `json:"kind"`
	Severity    string `json:"severity"`
	Network     string `json:"network"`
	Subnet      string `json:"subnet"`
	Node        string `json:"node"`
	Environment string `json:"environment"`
	Route       string `json:"route"`
	Iface       string `json:"iface"`
	Pool        string `json:"pool"`
	NoOverride  bool   `json:"noOverride"`
}

// ofKind are the findings of one kind, in the order given.
func (a subnetCheckAnswer) ofKind(kind string) []subnetFinding {
	var out []subnetFinding
	for _, f := range a.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func (f *agentFix) subnetCheck(t *testing.T, envID, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+envID+"/address-space/subnet-check", body)
}

// subnetSwarm is a swarm with the built-in pool (10.0.0.0/8 at /24) and the given overlays on
// its managers.
func subnetSwarm(t *testing.T, overlays ...dockertest.FakeNetwork) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.Networks = append([]dockertest.FakeNetwork{}, overlays...)
	})
}

// subnetSwarmWithBridge is subnetSwarm with a bridge network on one manager (host, "mgr-1" or
// "mgr-2") only: a node-local network the swarm allocator does not see.
func subnetSwarmWithBridge(t *testing.T, host, name, subnet string, overlays ...dockertest.FakeNetwork) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(h string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(h, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.Networks = append([]dockertest.FakeNetwork{}, overlays...)
		if h == host {
			cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{ID: name, Name: name, Driver: "bridge", Scope: "local", Subnets: []string{subnet}})
		}
	})
}

func overlayNet(id string, subnet string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{ID: id, Name: id, Driver: "overlay", Scope: "swarm", Subnets: []string{subnet}}
}

func TestSubnetCheckSuggest(t *testing.T) {
	t.Parallel()
	s := subnetSwarm(t,
		dockertest.FakeNetwork{ID: "ing", Name: "ingress", Driver: "overlay", Scope: "swarm", Ingress: true, Subnets: []string{"10.0.0.0/24"}},
		overlayNet("one", "10.0.1.0/24"),
		overlayNet("two", "10.0.2.0/26"),
	)
	for _, tc := range []struct{ body, subnet string }{
		{`{"suggest":true,"mask":26}`, "10.0.2.64/26"},
		{`{"suggest":true,"mask":24}`, "10.0.3.0/24"},
		{`{"suggest":true,"mask":24,"driver":"overlay"}`, "10.0.3.0/24"},
	} {
		rec := s.subnetCheck(t, s.env.ID, tc.body)
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", tc.body, rec.code, rec.body)
		}
		got := decode[subnetCheckAnswer](t, rec)
		if got.Suggestion == nil || got.Suggestion.Subnet != tc.subnet || got.Suggestion.Pool != "10.0.0.0/8" || got.NoFreeBlock != nil {
			t.Errorf("%s: answer = %s, want %s from 10.0.0.0/8", tc.body, rec.body, tc.subnet)
		}
	}
}

func (f *envFix) subnetCheck(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/address-space/subnet-check", body)
}

func checkOK(t *testing.T, rec *envRec) subnetCheckAnswer {
	t.Helper()
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	return decode[subnetCheckAnswer](t, rec)
}

func intIs(p *int, want int) bool { return p != nil && *p == want }

func TestSubnetCheckRecommend(t *testing.T) {
	t.Parallel()
	s := subnetSwarm(t)
	// The swarm has 3 nodes: (2*20 + 1 + 3) x 1.25 = 55, plus 3.
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"tasks":20}`))
	if !intIs(got.Needed, 58) || !intIs(got.RecommendedMask, 26) || !intIs(got.RecommendedUsable, 61) || got.TooManyTasks {
		t.Errorf("20 tasks on 3 nodes = %+v, want needed 58, /26 with 61 usable", got)
	}
	if got.Suggestion != nil || got.NoFreeBlock != nil || got.SubnetError != nil {
		t.Errorf("a size question alone answered more: %+v", got)
	}
	// Services and nodes given are used as given: (2*20 + 4 + 0) x 1.25 = 55, plus 3.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"tasks":20,"services":4,"nodes":0}`))
	if !intIs(got.Needed, 58) || !intIs(got.RecommendedMask, 26) {
		t.Errorf("20 tasks, 4 services, 0 nodes = %+v, want needed 58", got)
	}
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"tasks":1,"services":0,"nodes":0}`))
	if !intIs(got.Needed, 6) || !intIs(got.RecommendedMask, 28) || !intIs(got.RecommendedUsable, 13) {
		t.Errorf("1 task = %+v, want needed 6, /28", got)
	}
	// The size recommendation picks the block to suggest when no mask is given.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"tasks":20,"suggest":true}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "10.0.0.0/26" {
		t.Errorf("suggest with tasks = %+v, want 10.0.0.0/26", got)
	}
}

func TestSubnetCheckTooManyTasks(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	got := checkOK(t, f.subnetCheck(t, e, `{"tasks":120}`))
	// A standalone host spans no nodes: (2*120 + 1) x 1.25 = 301.25, ceiled to 302, plus 3.
	if !intIs(got.Needed, 305) || !got.TooManyTasks || got.RecommendedMask != nil || got.RecommendedUsable != nil {
		t.Errorf("120 tasks = %+v, want needed 305 and tooManyTasks with no size", got)
	}
	if !strings.Contains(string(f.subnetCheck(t, e, `{"tasks":1}`).body), `"tooManyTasks":false`) {
		t.Error("tooManyTasks is not always present")
	}
	// Out-of-range numbers are refused by the schema, not run.
	for _, body := range []string{`{"tasks":0}`, `{"tasks":10001}`, `{"mask":15}`, `{"mask":30}`, `{"services":1001}`, `{"driver":"host"}`} {
		if rec := f.subnetCheck(t, e, body); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", body, rec.code)
		}
	}
}

func TestSubnetCheckSubnetErrors(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	for _, tc := range []struct {
		subnet, code, corrected string
		ipv6                    bool
	}{
		{"nonsense", "subnet_invalid", "", false},
		{"10.0.0.0", "subnet_invalid", "", false},
		{"10.0.0.0/33", "subnet_invalid", "", false},
		{"10.0.0.5/24", "host_bits", "10.0.0.0/24", false},
		{"10.0.0.0/30", "too_small", "", false},
		{"10.0.0.0/29", "", "", false},
		{"10.0.0.0/24", "", "", false},
		{"fd00:1::/64", "", "", true},
		{"fd00:1::5/64", "", "", true},
	} {
		body := `{"subnet":"` + tc.subnet + `"}`
		got := checkOK(t, f.subnetCheck(t, e, body))
		switch {
		case tc.code == "" && got.SubnetError != nil:
			t.Errorf("%s: error %+v, want none", tc.subnet, got.SubnetError)
		case tc.code != "" && (got.SubnetError == nil || got.SubnetError.Code != tc.code || got.SubnetError.Corrected != tc.corrected):
			t.Errorf("%s: error %+v, want %s corrected %q", tc.subnet, got.SubnetError, tc.code, tc.corrected)
		}
		if got.IPv6NotChecked != tc.ipv6 {
			t.Errorf("%s: ipv6NotChecked = %v, want %v", tc.subnet, got.IPv6NotChecked, tc.ipv6)
		}
	}
	if rec := f.subnetCheck(t, e, `{"subnet":"`+strings.Repeat("1", 65)+`"}`); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a subnet of 65 characters: status = %d, want 422", rec.code)
	}
}

func TestSubnetCheckLocalDriverOnSwarm(t *testing.T) {
	t.Parallel()
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if !strings.HasPrefix(host, "mgr-") {
			return
		}
		cfg.SwarmClusterID, cfg.SwarmSubnetSize = "cluster-1", 24
		cfg.Networks = []dockertest.FakeNetwork{overlayNet("ovl", "10.0.0.0/24")}
		if host == "mgr-2" {
			cfg.DefaultAddressPools = []dockertest.FakeAddressPool{{Base: "172.30.0.0/16", Size: 24}}
			cfg.Networks = append(cfg.Networks, dockertest.FakeNetwork{ID: "br2", Name: "br2", Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}})
		}
	})
	// A bridge on mgr-2 comes from mgr-2's own daemon pool, clear of its network.
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"suggest":true,"mask":24,"driver":"bridge","nodeId":"`+s.m2.id+`"}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "172.30.1.0/24" || got.Suggestion.Pool != "172.30.0.0/16" {
		t.Errorf("a bridge on mgr-2 = %+v, want 172.30.1.0/24 from 172.30.0.0/16", got)
	}
	// mgr-1 sets no pools, so Docker's defaults apply.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"suggest":true,"mask":24,"driver":"macvlan","nodeId":"`+s.m1.id+`"}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "172.17.0.0/24" || got.Suggestion.Pool != "172.17.0.0/16" {
		t.Errorf("a macvlan on mgr-1 = %+v, want 172.17.0.0/24 from Docker's first default pool", got)
	}
	// An overlay is the swarm's, whatever node is named.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"suggest":true,"mask":24,"driver":"overlay","nodeId":"`+s.m2.id+`"}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "10.0.1.0/24" || got.Suggestion.Pool != "10.0.0.0/8" {
		t.Errorf("an overlay = %+v, want 10.0.1.0/24 from the swarm pool", got)
	}
	// A local driver with no node has no pool to search: no answer, and no error.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"suggest":true,"mask":24,"driver":"bridge"}`))
	if got.Suggestion != nil || got.NoFreeBlock != nil {
		t.Errorf("a bridge with no node = %+v, want no suggestion", got)
	}
	// A node that is not in the environment is refused, not a 500.
	if rec := s.subnetCheck(t, s.env.ID, `{"suggest":true,"mask":24,"driver":"bridge","nodeId":"nobody"}`); rec.code >= 500 && rec.code != http.StatusServiceUnavailable {
		t.Errorf("an unknown node: status = %d: %s", rec.code, rec.body)
	}
}

func TestSubnetCheckStandalone(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/16", Size: 26}},
		Networks: []dockertest.FakeNetwork{
			{ID: "a", Name: "a", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.0/26"}},
			{ID: "b", Name: "b", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.64/26"}},
		},
	})
	for _, body := range []string{`{"suggest":true,"mask":26}`, `{"suggest":true,"mask":26,"driver":"bridge"}`} {
		got := checkOK(t, f.subnetCheck(t, e, body))
		if got.Suggestion == nil || got.Suggestion.Subnet != "10.50.0.128/26" || got.Suggestion.Pool != "10.50.0.0/16" {
			t.Errorf("%s = %+v, want 10.50.0.128/26 from the host's pool", body, got)
		}
	}
	// The mask is the pool's own when none is asked for.
	if got := checkOK(t, f.subnetCheck(t, e, `{"suggest":true}`)); got.Suggestion == nil || got.Suggestion.Subnet != "10.50.0.128/26" {
		t.Errorf("no mask = %+v, want the pool's /26", got)
	}
	// Nothing is asked for, nothing is answered.
	if got := checkOK(t, f.subnetCheck(t, e, `{}`)); got.Suggestion != nil || got.NoFreeBlock != nil || got.Needed != nil || got.SubnetError != nil {
		t.Errorf("an empty question = %+v, want an empty answer", got)
	}

	// Docker's built-in pools are searched when the daemon sets none: the first is 172.17.0.0/16.
	f2, e2 := dashEnv(t, dockertest.FakeConfig{Hostname: "node-2", Version: "29.1.0"})
	got := checkOK(t, f2.subnetCheck(t, e2, `{"suggest":true,"mask":24,"tasks":3}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "172.17.0.0/24" || !intIs(got.Needed, 12) {
		t.Errorf("a host with no pools = %+v, want 172.17.0.0/24 and needed 12", got)
	}
}

func TestSubnetCheckNoFreeBlock(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "10.50.0.0/24", Size: 26}},
		Networks:            []dockertest.FakeNetwork{{ID: "a", Name: "a", Driver: "bridge", Scope: "local", Subnets: []string{"10.50.0.0/25", "10.50.0.128/26"}}},
	})
	// One /26 is left: asking for it works, for a /25 it does not.
	if got := checkOK(t, f.subnetCheck(t, e, `{"suggest":true,"mask":26}`)); got.Suggestion == nil || got.Suggestion.Subnet != "10.50.0.192/26" {
		t.Errorf("the last /26 = %+v", got)
	}
	rec := f.subnetCheck(t, e, `{"suggest":true,"mask":25}`)
	got := checkOK(t, rec)
	if got.Suggestion != nil || got.NoFreeBlock == nil || got.NoFreeBlock.Mask != 25 || got.NoFreeBlock.Pool != "10.50.0.0/24" {
		t.Errorf("a /25 = %s, want noFreeBlock 25 in 10.50.0.0/24 and 200", rec.body)
	}
}

func TestSubnetCheckRateLimited(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	awaitOnline(t, f.mon, e.ID)
	// The test clock does not move, so nothing refills: the burst of 20 is all there is.
	for i := range 20 {
		if rec := f.subnetCheck(t, e, `{"tasks":1}`); rec.code != http.StatusOK {
			t.Fatalf("check %d: status = %d: %s", i+1, rec.code, rec.body)
		}
	}
	rec := f.subnetCheck(t, e, `{"tasks":1}`)
	if rec.code != http.StatusTooManyRequests || rec.ptyp != "rate_limited" {
		t.Fatalf("the 21st check: %d %q, want 429 rate_limited", rec.code, rec.ptyp)
	}
}

func TestSubnetCheckViewerAllowed(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "outsider", false)
	awaitOnline(t, f.mon, e.ID)

	path := "/api/env/" + e.ID + "/address-space/subnet-check"
	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: path, body: `{"suggest":true,"mask":24}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("a Viewer: status = %d, want 200: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	cookie, csrf = f.signIn(t, "outsider", false)
	rec = f.call(t, authReq{method: http.MethodPost, path: path, body: `{}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a user without the environment: status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

// The overlap findings (plan 10-16, NET-05, D-16, D-18).

func TestSubnetCheckSameEnvBlocking(t *testing.T) {
	t.Parallel()
	s := subnetSwarm(t, overlayNet("ovl-a", "10.0.1.0/24"))
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.1.0/25","driver":"overlay"}`))
	same := got.ofKind("same_env")
	if len(same) != 1 || same[0].Severity != "blocking" || same[0].Network != "ovl-a" || same[0].Subnet != "10.0.1.0/24" || same[0].Node != "" {
		t.Fatalf("findings = %+v, want one blocking same_env naming ovl-a", got.Findings)
	}
	// Findings are never null, and a subnet that only touches the network does not collide.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.2.0/24"}`))
	if got.Findings == nil || len(got.ofKind("same_env")) != 0 {
		t.Errorf("an adjacent subnet = %+v, want no same_env finding", got.Findings)
	}
	if rec := s.subnetCheck(t, s.env.ID, `{"tasks":3}`); !strings.Contains(string(rec.body), `"findings":[]`) {
		t.Errorf("findings is not always present: %s", rec.body)
	}
}

// Only an overlap between swarm networks on a swarm cannot be created anyway (G-10-8): the
// managers' allocator refuses it after the create answers.
func TestSubnetCheckNoOverride(t *testing.T) {
	t.Parallel()
	s := subnetSwarmWithBridge(t, "mgr-2", "lan", "10.0.5.0/24", overlayNet("ovl-a", "10.0.1.0/24"))

	// An overlay against another swarm network.
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.1.0/25","driver":"overlay"}`))
	same := got.ofKind("same_env")
	if len(same) != 1 || same[0].Severity != "blocking" || !same[0].NoOverride {
		t.Errorf("overlay over a swarm network = %+v, want blocking with noOverride", got.Findings)
	}
	// No driver is an overlay too.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.1.0/25"}`))
	if same := got.ofKind("same_env"); len(same) != 1 || !same[0].NoOverride {
		t.Errorf("no driver over a swarm network = %+v, want noOverride", got.Findings)
	}
	// A bridge on a node against a swarm network can be created anyway.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.1.0/25","driver":"bridge","nodeId":"`+s.m2.id+`"}`))
	if same := got.ofKind("same_env"); len(same) != 1 || same[0].Severity != "blocking" || same[0].NoOverride {
		t.Errorf("bridge over a swarm network = %+v, want blocking without noOverride", got.Findings)
	}
	// An overlay against only a node's bridge can be created anyway.
	got = checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.5.0/25","driver":"overlay"}`))
	if same := got.ofKind("same_env"); len(same) != 1 || same[0].Network != "lan" || same[0].NoOverride {
		t.Errorf("overlay over a node's bridge = %+v, want blocking without noOverride", got.Findings)
	}
	// The field is left out of the answer unless it is true.
	if rec := s.subnetCheck(t, s.env.ID, `{"subnet":"10.0.5.0/25","driver":"overlay"}`); strings.Contains(string(rec.body), "noOverride") {
		t.Errorf("noOverride is in the answer for an overridable finding: %s", rec.body)
	}
}

func TestSubnetCheckOutsidePool(t *testing.T) {
	t.Parallel()
	s := subnetSwarm(t)
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"172.20.0.0/24"}`))
	out := got.ofKind("outside_pool")
	if len(out) != 1 || out[0].Severity != "warning" || out[0].Pool != "10.0.0.0/8" {
		t.Errorf("a subnet outside the swarm pool = %+v, want one outside_pool warning naming 10.0.0.0/8", got.Findings)
	}
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.9.0.0/24"}`)); len(got.ofKind("outside_pool")) != 0 {
		t.Errorf("a subnet inside the pool = %+v, want no outside_pool", got.Findings)
	}
}

// Private (RFC 1918) ranges are not flagged: a real LAN or VPN clash shows as a host route.
func TestSubnetCheckPrivateRangesNotFlagged(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	for _, subnet := range []string{"192.168.5.0/24", "172.17.3.0/24", "172.31.0.0/20", "192.168.0.0/15", "10.9.0.0/24"} {
		rec := f.subnetCheck(t, e, `{"subnet":"`+subnet+`"}`)
		got := checkOK(t, rec)
		for _, fd := range got.Findings {
			switch fd.Kind {
			case "same_env", "other_env", "host_route", "outside_pool":
			default:
				t.Errorf("%s: finding %+v has a kind outside the four", subnet, fd)
			}
		}
		if strings.Contains(string(rec.body), "\"range\"") {
			t.Errorf("%s: the response carries a range key: %s", subnet, rec.body)
		}
	}
	// The subnet a default swarm suggests comes back clean.
	s := subnetSwarm(t)
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.9.0.0/24"}`)); len(got.Findings) != 0 {
		t.Errorf("10.9.0.0/24 on a default swarm = %+v, want no findings", got.Findings)
	}
}

// The networks of another environment are named only to a caller who may read them, and only from
// their last read (T-10-39).
func TestSubnetCheckOtherEnvFiltered(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "Site A")
	b := f.srv.NewEnvironment(t, "Site B")
	f.connect(t, a, dockertest.FakeConfig{Hostname: "a-node", Version: "29.1.0"}, nil)
	f.connect(t, b, dockertest.FakeConfig{Hostname: "b-node", Version: "29.1.0", Networks: []dockertest.FakeNetwork{
		{ID: "bl", Name: "b-lan", Driver: "bridge", Scope: "local", Subnets: []string{"10.77.0.0/24"}},
	}}, nil)
	f.whenState(t, a.ID, "online")
	f.whenState(t, b.ID, "online")
	f.addUser(t, "a-only", false)
	f.addEnvRole(t, "a-only", a.ID, "viewer")
	f.addUser(t, "both", false)
	f.addEnvRole(t, "both", a.ID, "viewer")
	f.addEnvRole(t, "both", b.ID, "viewer")

	const body = `{"subnet":"10.77.0.0/25"}`
	check := func(cookie, csrf string) subnetCheckAnswer {
		t.Helper()
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + a.ID + "/address-space/subnet-check", body: body, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		return decode[subnetCheckAnswer](t, &envRec{rec.Code, rec.Body.Bytes(), ""})
	}
	admin := func() subnetCheckAnswer { return check(f.cookie, f.csrf) }

	// Site B has never been read: it contributes nothing, even to an admin.
	if got := admin(); len(got.ofKind("other_env")) != 0 {
		t.Fatalf("an environment never read: %+v, want nothing", got.Findings)
	}
	if rec := f.get(t, b.ID, "address-space"); rec.code != http.StatusOK {
		t.Fatalf("reading site B: %d %s", rec.code, rec.body)
	}
	got := admin()
	other := got.ofKind("other_env")
	if len(other) != 1 || other[0].Environment != "Site B" || other[0].Network != "b-lan" || other[0].Subnet != "10.77.0.0/24" || other[0].Node != "b-node" || other[0].Severity != "warning" {
		t.Fatalf("an admin: %+v, want a warning naming Site B and b-lan", got.Findings)
	}
	// A caller with no grant on Site B learns nothing of it.
	cookie, csrf := f.signIn(t, "a-only", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + a.ID + "/address-space/subnet-check", body: body, cookie: cookie, csrf: csrf})
	if strings.Contains(rec.Body.String(), "Site B") || strings.Contains(rec.Body.String(), "b-lan") || strings.Contains(rec.Body.String(), "other_env") {
		t.Errorf("a caller without access to Site B: %s", rec.Body)
	}
	cookie, csrf = f.signIn(t, "both", false)
	if got := check(cookie, csrf); len(got.ofKind("other_env")) != 1 {
		t.Errorf("a caller with both: %+v, want the Site B warning", got.Findings)
	}
}

// The host routes (plan 10-16, NET-05, D-16, D-17 as amended, T-10-41).

func TestSubnetCheckHostRoute(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Edge")
	f.connectAs(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "29.1.0", Networks: []dockertest.FakeNetwork{
		{ID: "b19", Name: "mynet", Driver: "bridge", Scope: "local", Subnets: []string{"172.19.0.0/16"}},
	}}, nil, tunneltest.Agent{HostNet: wire.HostNetOK, HostRoutes: []wire.HostRoute{
		{Iface: "eth0", Dst: "192.168.2.0/24"},
		{Iface: "docker0", Dst: "172.17.0.0/16"},
		{Iface: "docker_gwbridge", Dst: "172.18.0.0/16"},
		{Iface: "br-0123456789ab", Dst: "172.20.0.0/16"},
		// A Docker network's own subnet, whatever interface carries it.
		{Iface: "eth1", Dst: "172.19.0.0/16"},
	}})
	f.whenState(t, env.ID, "online")

	got := checkOK(t, f.subnetCheck(t, env.ID, `{"subnet":"192.168.2.0/25"}`))
	routes := got.ofKind("host_route")
	if len(routes) != 1 || routes[0].Severity != "warning" || routes[0].Iface != "eth0" || routes[0].Route != "192.168.2.0/24" || routes[0].Node != "edge-1" {
		t.Fatalf("findings = %+v, want one host_route warning on eth0", got.Findings)
	}
	if len(got.RouteChecks) != 1 || got.RouteChecks[0].State != "ok" || got.RouteChecks[0].Node != "edge-1" || got.RouteChecks[0].Fix != "" {
		t.Errorf("routeChecks = %+v, want edge-1 ok", got.RouteChecks)
	}
	// Docker's own bridges and the networks Docker made are not the host's LAN or VPN.
	for _, subnet := range []string{"172.17.0.0/24", "172.18.0.0/24", "172.20.0.0/24", "172.19.0.0/24"} {
		got := checkOK(t, f.subnetCheck(t, env.ID, `{"subnet":"`+subnet+`"}`))
		if r := got.ofKind("host_route"); len(r) != 0 {
			t.Errorf("%s: %+v, want no host_route", subnet, r)
		}
	}
	// A suggestion keeps clear of a route of the host.
	got = checkOK(t, f.subnetCheck(t, env.ID, `{"suggest":true,"mask":24}`))
	if got.Suggestion == nil {
		t.Fatalf("no suggestion: %+v", got)
	}
	if strings.HasPrefix(got.Suggestion.Subnet, "192.168.2.") {
		t.Errorf("the suggestion %s lies inside a route of the host", got.Suggestion.Subnet)
	}
}

func TestSubnetCheckHostRouteIsAvoidedBySuggestion(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Edge")
	f.connectAs(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "29.1.0", DefaultAddressPools: []dockertest.FakeAddressPool{{Base: "192.168.0.0/16", Size: 24}}},
		nil, tunneltest.Agent{HostNet: wire.HostNetOK, HostRoutes: []wire.HostRoute{{Iface: "eth0", Dst: "192.168.0.0/23"}}})
	f.whenState(t, env.ID, "online")
	got := checkOK(t, f.subnetCheck(t, env.ID, `{"suggest":true,"mask":24}`))
	if got.Suggestion == nil || got.Suggestion.Subnet != "192.168.2.0/24" {
		t.Errorf("suggestion = %+v, want 192.168.2.0/24, the first block clear of the host's /23", got.Suggestion)
	}
}

func TestSubnetCheckRouteStates(t *testing.T) {
	t.Parallel()
	s := newSwarmFixHostNet(t, nil, func(host string) (string, []wire.HostRoute) {
		switch host {
		case "mgr-1":
			return wire.HostNetOK, []wire.HostRoute{{Iface: "eth0", Dst: "192.168.9.0/24"}}
		case "wrk-1":
			return wire.HostNetUnmounted, nil
		}
		return "", nil // mgr-2 is an agent older than this check
	})
	got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"10.9.0.0/24"}`))
	want := []routeCheck{
		{Node: "mgr-1", State: "ok"},
		{Node: "mgr-2", State: "old_agent"},
		{Node: "wrk-1", State: "unmounted", Fix: agents.HostRouteMountCommand("swarm", agents.InstallParams{})},
	}
	if len(got.RouteChecks) != len(want) {
		t.Fatalf("routeChecks = %+v, want %+v", got.RouteChecks, want)
	}
	for i, w := range want {
		g := got.RouteChecks[i]
		if g.Node != w.Node || g.State != w.State || g.Fix != w.Fix || g.Local {
			t.Errorf("routeChecks[%d] = %+v, want %+v", i, g, w)
		}
	}
	if !strings.Contains(want[2].Fix, "--mount-add type=bind,src=/proc/1/net/route,dst=/host/route,ro") {
		t.Errorf("the fix = %q", want[2].Fix)
	}
	// Only the agent that said it can was asked for its routes.
	if n := s.m1.conn.HostNetStreams.Load(); n != 1 {
		t.Errorf("mgr-1 was asked %d times, want once", n)
	}
	if n := s.m2.conn.HostNetStreams.Load() + s.w.conn.HostNetStreams.Load(); n != 0 {
		t.Errorf("an agent that cannot report routes was asked %d times, want 0", n)
	}
	// The routes of mgr-1 concern an overlay, and a bridge on mgr-1 only.
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"subnet":"192.168.9.0/25","driver":"overlay"}`)); len(got.ofKind("host_route")) != 1 {
		t.Errorf("an overlay: %+v, want the route of mgr-1", got.Findings)
	}
	body := `{"subnet":"192.168.9.0/25","driver":"bridge","nodeId":"` + s.m1.id + `"}`
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, body)); len(got.ofKind("host_route")) != 1 {
		t.Errorf("a bridge on mgr-1: %+v, want the route of mgr-1", got.Findings)
	}
	body = `{"subnet":"192.168.9.0/25","driver":"bridge","nodeId":"` + s.m2.id + `"}`
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, body)); len(got.ofKind("host_route")) != 0 {
		t.Errorf("a bridge on mgr-2: %+v, want no route of another node", got.Findings)
	}
	// A question that is not about a subnet reads no routes at all.
	if got := checkOK(t, s.subnetCheck(t, s.env.ID, `{"tasks":4}`)); len(got.RouteChecks) != 0 {
		t.Errorf("a size question: routeChecks = %+v, want none", got.RouteChecks)
	}
	if n := s.m1.conn.HostNetStreams.Load(); n != 1 {
		t.Errorf("mgr-1 was asked %d times in all, want once (its answer is kept)", n)
	}
}

func TestSubnetCheckRouteStatesOfTheLocalHost(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	got := checkOK(t, f.subnetCheck(t, e, `{"subnet":"10.9.0.0/24"}`))
	if len(got.RouteChecks) != 1 || got.RouteChecks[0].State != "unmounted" || !got.RouteChecks[0].Local || got.RouteChecks[0].Fix != "" {
		t.Fatalf("routeChecks = %+v, want the local host unmounted, local, with no fix of the server's", got.RouteChecks)
	}

	// With the route file mounted the host's routes are read from it.
	file := filepath.Join(t.TempDir(), "route")
	content := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"eth0\t0007A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.hostRouteFile = file
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	f.cookie, f.csrf = cookie, csrf
	got = checkOK(t, f.subnetCheck(t, e, `{"subnet":"192.168.7.0/25"}`))
	if len(got.RouteChecks) != 1 || got.RouteChecks[0].State != "ok" || !got.RouteChecks[0].Local {
		t.Fatalf("routeChecks = %+v, want the local host ok", got.RouteChecks)
	}
	if r := got.ofKind("host_route"); len(r) != 1 || r[0].Iface != "eth0" || r[0].Route != "192.168.7.0/24" {
		t.Errorf("findings = %+v, want the route of eth0", got.Findings)
	}
}
