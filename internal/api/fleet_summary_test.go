// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The Overview's tiles, the permission filtering of the fleet views, empty fleets and a stable
// order (plan 12-26 task 2, ENV-10, D-17). The fixtures are those of fleet_test.go.

func meter(percent int) envs.AddressSpace {
	return envs.AddressSpace{Swarm: true, Measured: true, Overlays: []envs.OverlayUsage{
		{Name: fmt.Sprintf("net%d", percent), Measured: true, Used: uint64(percent), Capacity: 100, Percent: percent, State: netspace.StateNormal},
	}}
}

type fleetSummaryResp struct {
	Environments struct {
		Total         int `json:"total"`
		NeedAttention int `json:"needAttention"`
	} `json:"environments"`
	Failing struct {
		Count        *int `json:"count"`
		Environments *int `json:"environments"`
	} `json:"failing"`
	Updates struct {
		Count         *int   `json:"count"`
		ChecksFailed  *int   `json:"checksFailed"`
		LastCheckedAt string `json:"lastCheckedAt"`
		AllChecked    bool   `json:"allChecked"`
	} `json:"updates"`
	Pools struct {
		NearFull   *int `json:"nearFull"`
		AlmostFull *int `json:"almostFull"`
	} `json:"pools"`
	NotIncluded []fleetNotIncluded `json:"notIncluded"`
}

// fleetFix is an agent fixture with named environments and the fake behind the fleet routes.
func (f *fleetFix) summary(t *testing.T, as string) fleetSummaryResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/fleet/summary")
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/fleet/summary as %q = %d %s", as, rec.code, rec.body)
	}
	var r fleetSummaryResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

// fleetGet calls a route as the fixture's admin when as is empty, else as the named user.
func TestFleetSummaryCounts(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma")
	alpha, beta, gamma := f.id("Alpha"), f.id("Beta"), f.id("Gamma")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{degraded("s1", "web", 1, 2), degraded("s2", "db", 0, 1), degraded("s3", "ok", 1, 1)})
	f.fake.online(beta)
	f.fake.state[beta] = envs.ClusterState{FetchedAt: fleetNow, AttemptedAt: fleetNow}
	f.fake.down(gamma, envs.StateOffline)
	checked := fleetNow.Add(-time.Hour)
	f.fake.tally[alpha] = fleetTally{t: updates.Tally{Available: 2, Failed: 1, CheckedAt: checked}}
	f.fake.tally[beta] = fleetTally{t: updates.Tally{Available: 1, CheckedAt: checked.Add(-time.Hour)}}
	f.fake.space[alpha] = fleetSpace{v: meter(75)}
	f.fake.space[beta] = fleetSpace{v: meter(92)}
	f.rebuild(t)

	got := f.summary(t, "")
	if got.Environments.Total != 3 || got.Environments.NeedAttention != 1 {
		t.Errorf("environments = %+v, want 3 with 1 needing attention", got.Environments)
	}
	if got.Failing.Count == nil || *got.Failing.Count != 2 || got.Failing.Environments == nil || *got.Failing.Environments != 1 {
		t.Errorf("failing = %+v, want 2 in 1 environment", got.Failing)
	}
	u := got.Updates
	if u.Count == nil || *u.Count != 3 || u.ChecksFailed == nil || *u.ChecksFailed != 1 || u.AllChecked || u.LastCheckedAt != checked.Format(time.RFC3339) {
		t.Errorf("updates = %+v, want 3 available, 1 failed, not all checked, last checked at %s", u, checked)
	}
	if got.Pools.NearFull == nil || *got.Pools.NearFull != 2 || got.Pools.AlmostFull == nil || *got.Pools.AlmostFull != 1 {
		t.Errorf("pools = %+v, want 2 near full of which 1 almost full", got.Pools)
	}
	if want := []fleetNotIncluded{{EnvID: gamma, EnvName: "Gamma", Reason: "offline"}}; !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
}

func TestFleetSummaryAllCheckedNeedsEveryEnvironmentChecked(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	f.fake.online(f.id("Alpha"))
	f.fake.online(f.id("Beta"))
	for _, n := range []string{"Alpha", "Beta"} {
		f.fake.state[f.id(n)] = envs.ClusterState{FetchedAt: fleetNow, AttemptedAt: fleetNow}
		f.fake.tally[f.id(n)] = fleetTally{t: updates.Tally{CheckedAt: fleetNow}}
		f.fake.space[f.id(n)] = fleetSpace{v: meter(10)}
	}
	f.rebuild(t)
	if got := f.summary(t, ""); !got.Updates.AllChecked || got.Updates.Count == nil || *got.Updates.Count != 0 {
		t.Fatalf("updates = %+v, want all checked and a known 0", got.Updates)
	}

	f.fake.tally[f.id("Beta")] = fleetTally{t: updates.Tally{NotChecked: 1}}
	if got := f.summary(t, ""); got.Updates.AllChecked {
		t.Errorf("updates = %+v, want not all checked while a workload waits for its first check", got.Updates)
	}
}

func TestFleetNeverShowsUnreadableEnvironments(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma")
	alpha, beta, gamma := f.id("Alpha"), f.id("Beta"), f.id("Gamma")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{degraded("s1", "web", 1, 2)})
	f.fake.swarmWith(beta, []dockerx.ServiceRow{degraded("s2", "secret", 0, 3), degraded("s3", "other", 0, 1)})
	f.fake.down(gamma, envs.StateOffline)
	f.fake.tally[alpha] = fleetTally{t: updates.Tally{Available: 1, CheckedAt: fleetNow}}
	f.fake.tally[beta] = fleetTally{t: updates.Tally{Available: 5, Failed: 4, CheckedAt: fleetNow}}
	f.fake.space[alpha] = fleetSpace{v: meter(10)}
	f.fake.space[beta] = fleetSpace{v: meter(99)}
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.rebuild(t)

	got := f.failing(t, "viewer", "")
	if len(got.Items) != 1 || got.Items[0].EnvID != alpha || got.Items[0].Name != "web" {
		t.Errorf("a Viewer of Alpha sees items %+v, want Alpha's web only", got.Items)
	}
	if len(got.NotIncluded) != 0 {
		t.Errorf("a Viewer of Alpha is told %+v is not included: Gamma is offline but theirs to know nothing of", got.NotIncluded)
	}
	sum := f.summary(t, "viewer")
	if sum.Environments.Total != 1 || sum.Environments.NeedAttention != 0 ||
		sum.Failing.Count == nil || *sum.Failing.Count != 1 ||
		sum.Updates.Count == nil || *sum.Updates.Count != 1 || sum.Updates.ChecksFailed == nil || *sum.Updates.ChecksFailed != 0 ||
		sum.Pools.NearFull == nil || *sum.Pools.NearFull != 0 || len(sum.NotIncluded) != 0 {
		t.Errorf("summary for a Viewer of Alpha = %+v", sum)
	}
	if asked := f.fake.asked(); !slices.Equal(asked, []string{alpha}) {
		t.Errorf("environments read for a Viewer of Alpha = %v, want only Alpha: nothing of the others may be read, let alone shown", asked)
	}
}

func TestFleetEmpty(t *testing.T) {
	t.Parallel()
	t.Run("no readable environment", func(t *testing.T) {
		t.Parallel()
		f := newFleetFix(t, "Alpha")
		f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 0, 1)})
		f.addUser(t, "nobody", false)
		f.rebuild(t)

		rec := f.fleetGet(t, "nobody", "/api/fleet/failing")
		if rec.code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.code, rec.body)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rec.body, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["items"]) != "[]" || string(raw["notIncluded"]) != "[]" {
			t.Errorf("body = %s, want empty arrays and not null", rec.body)
		}
		sum := f.summary(t, "nobody")
		zero := func(p *int) bool { return p != nil && *p == 0 }
		if sum.Environments.Total != 0 || sum.Environments.NeedAttention != 0 || !zero(sum.Failing.Count) || !zero(sum.Failing.Environments) ||
			!zero(sum.Updates.Count) || !zero(sum.Updates.ChecksFailed) || !zero(sum.Pools.NearFull) || !zero(sum.Pools.AlmostFull) || len(sum.NotIncluded) != 0 {
			t.Errorf("summary = %+v, want zeros for what is known and nothing else", sum)
		}
		if asked := f.fake.asked(); len(asked) != 0 {
			t.Errorf("environments read = %v, want none", asked)
		}
	})

	t.Run("everything reports and nothing fails", func(t *testing.T) {
		t.Parallel()
		f := newFleetFix(t, "Alpha", "Beta")
		f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 2, 2)})
		f.fake.swarmWith(f.id("Beta"), nil)
		for _, n := range []string{"Alpha", "Beta"} {
			f.fake.tally[f.id(n)] = fleetTally{}
			f.fake.space[f.id(n)] = fleetSpace{v: meter(1)}
		}
		f.rebuild(t)

		if got := f.failing(t, "", ""); len(got.Items) != 0 || len(got.NotIncluded) != 0 {
			t.Errorf("failing = %+v, want nothing", got)
		}
		sum := f.summary(t, "")
		if sum.Failing.Count == nil || *sum.Failing.Count != 0 || sum.Failing.Environments == nil || *sum.Failing.Environments != 0 {
			t.Errorf("failing tile = %+v, want a known 0 when every environment reported", sum.Failing)
		}
	})

	t.Run("one environment offline and nothing failing in the rest", func(t *testing.T) {
		t.Parallel()
		f := newFleetFix(t, "Alpha", "Beta")
		f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 2, 2)})
		f.fake.down(f.id("Beta"), envs.StateOffline)
		f.fake.tally[f.id("Alpha")] = fleetTally{}
		f.fake.space[f.id("Alpha")] = fleetSpace{v: meter(1)}
		f.rebuild(t)

		sum := f.summary(t, "")
		if sum.Failing.Count != nil || sum.Failing.Environments != nil || sum.Updates.Count != nil || sum.Pools.NearFull != nil || sum.Pools.AlmostFull != nil {
			t.Errorf("summary = %+v, want null where 0 would claim what an unread environment may contradict", sum)
		}
		if len(sum.NotIncluded) != 1 || sum.Environments.NeedAttention != 1 || sum.Updates.AllChecked {
			t.Errorf("summary = %+v, want Beta named and needing attention", sum)
		}
	})

	t.Run("one environment offline and a failing service in the rest", func(t *testing.T) {
		t.Parallel()
		f := newFleetFix(t, "Alpha", "Beta")
		f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 0, 2)})
		f.fake.down(f.id("Beta"), envs.StateOffline)
		f.rebuild(t)

		sum := f.summary(t, "")
		if sum.Failing.Count == nil || *sum.Failing.Count != 1 || len(sum.NotIncluded) != 1 {
			t.Errorf("summary = %+v, want the count of the environments that reported, with Beta named", sum)
		}
	})
}

func TestFleetFailingOrderStable(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Beta", "Alpha", "Gamma")
	old, recent := fleetNow.Add(-time.Hour), fleetNow.Add(-time.Minute)
	task := func(svc string, at time.Time) dockerx.FailedTaskRow {
		return dockerx.FailedTaskRow{ID: "t-" + svc, ServiceID: svc, At: at}
	}
	// Equal times in two environments, and within one, must give one order.
	f.fake.swarmWith(f.id("Beta"), []dockerx.ServiceRow{degraded("b2", "zeta", 0, 1), degraded("b1", "alpha", 0, 1), degraded("b3", "old", 0, 1)},
		task("b1", recent), task("b2", recent), task("b3", old))
	f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("a1", "mid", 0, 1)}, task("a1", recent))
	f.fake.swarmWith(f.id("Gamma"), []dockerx.ServiceRow{degraded("g1", "undated", 0, 1)})
	f.rebuild(t)

	names := func(r fleetFailingResp) []string {
		var out []string
		for _, it := range r.Items {
			out = append(out, it.EnvName+"/"+it.Name)
		}
		return out
	}
	first := names(f.failing(t, "", ""))
	// Newest first; equal times by environment name, then name; a row with no time last.
	want := []string{"Alpha/mid", "Beta/alpha", "Beta/zeta", "Beta/old", "Gamma/undated"}
	if !slices.Equal(first, want) {
		t.Errorf("order = %v, want %v", first, want)
	}
	for range 5 {
		if again := names(f.failing(t, "", "")); !slices.Equal(again, first) {
			t.Fatalf("a second read gave %v, the first %v", again, first)
		}
	}
}

func TestFleetSummaryNullWhenUnread(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	f.fake.swarmWith(f.id("Alpha"), []dockerx.ServiceRow{degraded("s1", "web", 1, 2)})
	f.fake.swarmWith(f.id("Beta"), nil)
	f.fake.tally[f.id("Alpha")] = fleetTally{t: updates.Tally{Available: 4, CheckedAt: fleetNow}}
	f.fake.tally[f.id("Beta")] = fleetTally{err: errors.New("the checks table is locked")}
	f.fake.space[f.id("Alpha")] = fleetSpace{v: meter(80)}
	f.fake.space[f.id("Beta")] = fleetSpace{err: errors.New("no address space")}
	f.rebuild(t)

	sum := f.summary(t, "")
	if sum.Updates.Count != nil || sum.Updates.ChecksFailed != nil || sum.Updates.AllChecked {
		t.Errorf("updates = %+v, want null where an environment's checks could not be read: a partial sum is not the count", sum.Updates)
	}
	if sum.Pools.NearFull != nil || sum.Pools.AlmostFull != nil {
		t.Errorf("pools = %+v, want null while an environment's pools could not be read", sum.Pools)
	}
	if sum.Failing.Count == nil || *sum.Failing.Count != 1 {
		t.Errorf("failing = %+v, want the tile that could be read to stand", sum.Failing)
	}
	if len(sum.NotIncluded) != 0 {
		t.Errorf("notIncluded = %+v, want none: both environments reported", sum.NotIncluded)
	}
}

func TestFleetSummaryNullWhenNothingToReadThemWith(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	f.fake.swarmWith(f.id("Alpha"), nil)
	f.fleet.Updates, f.fleet.AddressSpaces = nil, nil
	f.rebuild(t)

	sum := f.summary(t, "")
	if sum.Updates.Count != nil || sum.Pools.NearFull != nil {
		t.Errorf("summary = %+v, want null for tiles with no source", sum)
	}
}

func TestFleetEnvFilterIgnoresUnreadable(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{degraded("s1", "web", 0, 1)})
	f.fake.down(beta, envs.StateOffline)
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.rebuild(t)

	if got := f.failing(t, "viewer", "?env="+beta+"&env="+alpha); len(got.Items) != 1 || len(got.NotIncluded) != 0 {
		t.Errorf("filter naming an unreadable environment = %+v, want Alpha's rows and no mention of Beta", got)
	}
	if got := f.failing(t, "viewer", "?env="+beta); len(got.Items) != 0 || len(got.NotIncluded) != 0 {
		t.Errorf("filter naming only an unreadable environment = %+v, want nothing at all, and not everything", got)
	}
	if asked := f.fake.asked(); !slices.Equal(asked, []string{alpha}) {
		t.Errorf("environments read = %v, want only Alpha", asked)
	}

	// An administrator may read both, and the filter narrows to the one named.
	if got := f.failing(t, "", "?env="+alpha); len(got.Items) != 1 || len(got.NotIncluded) != 0 {
		t.Errorf("admin filtered to Alpha = %+v", got)
	}
	if got := f.failing(t, "", "?env="+beta); len(got.Items) != 0 || len(got.NotIncluded) != 1 {
		t.Errorf("admin filtered to Beta = %+v, want Beta named offline", got)
	}
}

func TestFleetRoutesNeedASignedInCaller(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	f.rebuild(t)
	for _, path := range []string{"/api/fleet/failing", "/api/fleet/summary"} {
		rec := f.call(t, authReq{method: http.MethodGet, path: path})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous GET %s = %d, want 401", path, rec.Code)
		}
	}
}
