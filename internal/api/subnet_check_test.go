// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
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
	IPv6NotChecked bool `json:"ipv6NotChecked"`
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
