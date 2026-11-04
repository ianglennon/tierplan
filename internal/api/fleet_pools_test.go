// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/netspace"
)

// The pools near exhaustion across the fleet (plan 12-28 task 2, ENV-10): every meter at 70% or
// more, by the P10 rule on the whole percent rounded down.

type fleetPoolsResp struct {
	Items []struct {
		EnvID     string `json:"envId"`
		EnvName   string `json:"envName"`
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Subnet    string `json:"subnet"`
		Used      uint64 `json:"used"`
		Capacity  uint64 `json:"capacity"`
		Percent   int    `json:"percent"`
		State     string `json:"state"`
		Unit      string `json:"unit"`
		Estimated bool   `json:"estimated"`
	} `json:"items"`
	NotIncluded []fleetNotIncluded `json:"notIncluded"`
}

func (f *fleetFix) pools(t *testing.T, as, query string) fleetPoolsResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/fleet/pools"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/fleet/pools%s as %q = %d %s", query, as, rec.code, rec.body)
	}
	var r fleetPoolsResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

// overlays is an address space of overlay networks named net{percent}, one at each percent given.
func overlays(percents ...int) envs.AddressSpace {
	v := envs.AddressSpace{Swarm: true, Measured: true}
	for _, p := range percents {
		v.Overlays = append(v.Overlays, envs.OverlayUsage{
			Name: fmt.Sprintf("net%03d", p), Subnets: []string{fmt.Sprintf("10.0.%d.0/24", p)}, Measured: true,
			Used: uint64(p), Capacity: 100, Percent: p, State: netspace.StateOf(uint64(p), 100),
		})
	}
	return v
}

func TestFleetPoolsThresholds(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.online(alpha)
	f.fake.online(beta)
	// 69 is not listed; 70 and 89 are getting full; 90 is almost full.
	f.fake.space[alpha] = fleetSpace{v: overlays(69, 70, 89, 90)}
	// A standalone host's pool is a guess, and says so.
	pool := netspace.PoolStat{Pool: netspace.Pool{Base: netip.MustParsePrefix("172.17.0.0/16"), Size: 16}, Used: 3, Capacity: 4, Percent: 75, State: netspace.StateGettingFull}
	f.fake.space[beta] = fleetSpace{v: envs.AddressSpace{Pools: []envs.PoolUsage{{PoolStat: pool, Estimated: true}}}}
	f.rebuild(t)

	got := f.pools(t, "", "")
	type row struct {
		env, name, state string
		percent          int
		estimated        bool
	}
	var have []row
	for _, it := range got.Items {
		have = append(have, row{it.EnvName, it.Name, it.State, it.Percent, it.Estimated})
	}
	// Fullest first.
	want := []row{
		{"Alpha", "net090", "almost_full", 90, false},
		{"Alpha", "net089", "getting_full", 89, false},
		{"Beta", "172.17.0.0/16", "getting_full", 75, true},
		{"Alpha", "net070", "getting_full", 70, false},
	}
	if !slices.Equal(have, want) {
		t.Fatalf("items = %+v, want %+v", have, want)
	}
	for _, it := range got.Items {
		switch it.Kind {
		case "overlay":
			if it.Unit != "addresses" || it.Capacity != 100 || it.Used != uint64(it.Percent) || it.Subnet == "" {
				t.Errorf("overlay %+v", it)
			}
		case "pool":
			if it.Unit != "subnets" || it.Subnet != "172.17.0.0/16" {
				t.Errorf("pool %+v", it)
			}
		default:
			t.Errorf("kind %q", it.Kind)
		}
	}
}

func TestFleetPoolsOrder(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "a", "b")
	a, b := f.id("a"), f.id("b")
	f.fake.online(a)
	f.fake.online(b)
	two := overlays(75)
	two.Overlays = append(two.Overlays, envs.OverlayUsage{Name: "alpha", Measured: true, Used: 75, Capacity: 100, Percent: 75, State: netspace.StateGettingFull})
	f.fake.space[a] = fleetSpace{v: two}
	f.fake.space[b] = fleetSpace{v: overlays(75)}
	f.rebuild(t)

	order := func() []string {
		var out []string
		for _, it := range f.pools(t, "", "").Items {
			out = append(out, it.EnvName+"/"+it.Name)
		}
		return out
	}
	want := []string{"a/alpha", "a/net075", "b/net075"}
	for range 3 {
		if got := order(); !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v: equal percent goes by environment name and then network name", got, want)
		}
	}
}

func TestFleetPoolsNotRead(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma")
	alpha, beta, gamma := f.id("Alpha"), f.id("Beta"), f.id("Gamma")
	f.fake.online(alpha)
	f.fake.space[alpha] = fleetSpace{v: overlays(80)}
	// Beta has had no address read yet; Gamma's swarm could not be measured.
	f.fake.space[beta] = fleetSpace{err: errNoRead}
	f.fake.space[gamma] = fleetSpace{v: envs.AddressSpace{Swarm: true, NotMeasured: "no manager answered"}}
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.rebuild(t)

	got := f.pools(t, "", "")
	if len(got.Items) != 1 || got.Items[0].EnvID != alpha {
		t.Errorf("items = %+v, want Alpha's one", got.Items)
	}
	want := []fleetNotIncluded{{EnvID: beta, EnvName: "Beta", Reason: "not_reporting"}, {EnvID: gamma, EnvName: "Gamma", Reason: "not_reporting"}}
	if !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}

	// An environment the caller may not read is absent: not listed, not named, not read.
	f.fake.mu.Lock()
	f.fake.read = map[string]int{}
	f.fake.mu.Unlock()
	v := f.pools(t, "viewer", "")
	if len(v.Items) != 1 || len(v.NotIncluded) != 0 {
		t.Errorf("viewer of Alpha sees %+v", v)
	}
	if asked := f.fake.asked(); !slices.Equal(asked, []string{alpha}) {
		t.Errorf("environments read for the viewer = %v, want only Alpha", asked)
	}
}
