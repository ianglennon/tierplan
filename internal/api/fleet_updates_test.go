// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The fleet's pending updates, the pools near exhaustion and the fleet-wide Check now (plan 12-28,
// ENV-10, IMG-04, IMG-05, D-17). The fixtures are those of fleet_test.go; what each environment's
// verdicts are is set on the fake.

var errNoRead = errors.New("the environment could not be read")

// fleetVerdicts is what the fake answers EnvVerdicts with.
type fleetVerdicts struct {
	list []updates.WorkloadVerdict
	cov  updates.Coverage
	err  error
}

// verdictOf is the verdict of a workload of redis:7 in state, running digest old and the tag pointing at fresh.
func verdictOf(t *testing.T, kind, name, stack string, state updates.State) updates.WorkloadVerdict {
	t.Helper()
	ref, err := registry.ParseRef("redis:7")
	if err != nil {
		t.Fatal(err)
	}
	return updates.WorkloadVerdict{
		Workload: updates.Workload{Kind: kind, ID: kind + "-" + name, Name: name, Stack: stack, Written: "redis:7@sha256:old", Ref: ref},
		Verdict: updates.Verdict{
			State: state, RunningDigest: "sha256:old", RemoteDigest: "sha256:fresh",
			CheckedAt: fleetNow.Add(-time.Hour), SuccessAt: fleetNow.Add(-time.Hour),
		},
	}
}

func failedVerdict(t *testing.T, kind, name string) updates.WorkloadVerdict {
	t.Helper()
	v := verdictOf(t, kind, name, "", updates.StateCheckFailed)
	v.Verdict.Reason = updates.ReasonAuth
	v.Verdict.RemoteDigest = ""
	return v
}

type fleetUpdatesResp struct {
	Groups []struct {
		EnvID         string  `json:"envId"`
		EnvName       string  `json:"envName"`
		Type          string  `json:"type"`
		LastCheckedAt string  `json:"lastCheckedAt"`
		CheckFailed   *string `json:"checkFailed"`
		Items         []struct {
			Kind       string `json:"kind"`
			ID         string `json:"id"`
			Name       string `json:"name"`
			Stack      string `json:"stack"`
			Image      string `json:"image"`
			FromDigest string `json:"fromDigest"`
			ToDigest   string `json:"toDigest"`
			Update     struct {
				State string `json:"state"`
			} `json:"update"`
			Selectable bool `json:"selectable"`
		} `json:"items"`
	} `json:"groups"`
	Counts struct {
		Updates  int `json:"updates"`
		Problems int `json:"problems"`
		Ignored  int `json:"ignored"`
		All      int `json:"all"`
	} `json:"counts"`
	Coverage struct {
		Checkable     int    `json:"checkable"`
		Checked       int    `json:"checked"`
		Failed        int    `json:"failed"`
		NeverChecked  int    `json:"neverChecked"`
		Skipped       int    `json:"skipped"`
		Environments  int    `json:"environments"`
		LastCheckedAt string `json:"lastCheckedAt"`
		FirstCheckAt  string `json:"firstCheckAt"`
	} `json:"coverage"`
	NotIncluded []fleetNotIncluded `json:"notIncluded"`
}

func (f *fleetFix) updatesList(t *testing.T, as, query string) fleetUpdatesResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/fleet/updates"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/fleet/updates%s as %q = %d %s", query, as, rec.code, rec.body)
	}
	var r fleetUpdatesResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

func TestFleetUpdatesGroupsAndCounts(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.swarmWith(alpha, nil)
	f.fake.online(beta)
	f.fake.verdicts[alpha] = fleetVerdicts{
		list: []updates.WorkloadVerdict{
			verdictOf(t, "service", "web", "shop", updates.StateUpdateAvailable),
			verdictOf(t, "service", "api", "shop", updates.StateUpdateAvailable),
			verdictOf(t, "service", "old", "shop", updates.StateIgnored),
			verdictOf(t, "service", "fine", "shop", updates.StateUpToDate),
		},
		cov: updates.Coverage{Checkable: 4, Checked: 4, LastCheckedAt: fleetNow.Add(-time.Hour)},
	}
	f.fake.verdicts[beta] = fleetVerdicts{
		list: []updates.WorkloadVerdict{
			verdictOf(t, "container", "cache", "", updates.StateUpdateAvailable),
			failedVerdict(t, "container", "broken"),
		},
		cov: updates.Coverage{Checkable: 2, Checked: 2, Failed: 1, LastCheckedAt: fleetNow.Add(-2 * time.Hour)},
	}
	f.rebuild(t)

	got := f.updatesList(t, "", "")
	if got.Counts.Updates != 3 || got.Counts.Problems != 1 || got.Counts.Ignored != 1 || got.Counts.All != 5 {
		t.Errorf("counts = %+v, want {3, 1, 1, 5}", got.Counts)
	}
	if len(got.Groups) != 2 || got.Groups[0].EnvName != "Beta" || got.Groups[1].EnvName != "Alpha" {
		t.Fatalf("groups = %+v, want Beta (it has a failed check) before Alpha", got.Groups)
	}
	b, a := got.Groups[0], got.Groups[1]
	if b.CheckFailed == nil || *b.CheckFailed != "auth" || a.CheckFailed != nil {
		t.Errorf("checkFailed: Beta %v, Alpha %v, want auth and null", b.CheckFailed, a.CheckFailed)
	}
	if b.Type != "standalone" || a.Type != "swarm" {
		t.Errorf("types: Beta %q, Alpha %q", b.Type, a.Type)
	}
	if len(b.Items) != 1 || b.Items[0].Name != "cache" || !b.Items[0].Selectable {
		t.Errorf("Beta items = %+v, want the one update, selectable", b.Items)
	}
	if len(a.Items) != 2 || a.Items[0].Name != "api" || a.Items[1].Name != "web" {
		t.Errorf("Alpha items = %+v, want the two updates, api before web", a.Items)
	}
	web := a.Items[1]
	if web.Kind != "service" || web.Stack != "shop" || web.Image != "redis:7" || web.FromDigest != "sha256:old" || web.ToDigest != "sha256:fresh" ||
		web.Update.State != "update_available" || !web.Selectable {
		t.Errorf("web = %+v", web)
	}
	if a.LastCheckedAt == "" {
		t.Errorf("Alpha lastCheckedAt is empty")
	}
	if got.Coverage.Environments != 2 || got.Coverage.Checkable != 6 || got.Coverage.Checked != 6 || got.Coverage.Failed != 1 || got.Coverage.Skipped != 1 {
		t.Errorf("coverage = %+v", got.Coverage)
	}

	// The other filters list what their counts say.
	for show, want := range map[string]int{"updates": 3, "problems": 1, "ignored": 1, "all": 5} {
		g := f.updatesList(t, "", "?show="+show)
		n := 0
		for _, grp := range g.Groups {
			n += len(grp.Items)
		}
		if n != want {
			t.Errorf("show=%s lists %d items, want %d", show, n, want)
		}
		if g.Counts != got.Counts {
			t.Errorf("show=%s counts = %+v, want the same as without it", show, g.Counts)
		}
	}
	// A skipped or ignored item, and a failed one, can never be selected.
	for _, grp := range f.updatesList(t, "", "?show=all").Groups {
		for _, it := range grp.Items {
			if it.Selectable != (it.Update.State == "update_available") {
				t.Errorf("%s in %s: selectable %v in state %s", it.Name, grp.EnvName, it.Selectable, it.Update.State)
			}
		}
	}
	if rec := f.fleetGet(t, "", "/api/fleet/updates?show=nonsense"); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("show=nonsense = %d, want 422", rec.code)
	}
}

func TestFleetUpdatesNamesWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma")
	alpha, beta, gamma := f.id("Alpha"), f.id("Beta"), f.id("Gamma")
	f.fake.online(alpha)
	f.fake.verdicts[alpha] = fleetVerdicts{list: []updates.WorkloadVerdict{verdictOf(t, "container", "cache", "", updates.StateUpdateAvailable)}}
	f.fake.down(beta, envs.StateOffline)
	f.fake.online(gamma)
	f.fake.verdicts[gamma] = fleetVerdicts{err: errNoRead}
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.rebuild(t)

	got := f.updatesList(t, "", "")
	want := []fleetNotIncluded{{EnvID: beta, EnvName: "Beta", Reason: "offline"}, {EnvID: gamma, EnvName: "Gamma", Reason: "not_reporting"}}
	if !slices.Equal(got.NotIncluded, want) {
		t.Errorf("notIncluded = %+v, want %+v", got.NotIncluded, want)
	}
	if got.Coverage.Environments != 1 || got.Counts.Updates != 1 {
		t.Errorf("coverage %+v counts %+v, want only Alpha counted", got.Coverage, got.Counts)
	}

	// A person who can read Alpha only sees Alpha, and nothing else is read for them.
	f.fake.mu.Lock()
	f.fake.read = map[string]int{}
	f.fake.mu.Unlock()
	v := f.updatesList(t, "viewer", "")
	if len(v.NotIncluded) != 0 || len(v.Groups) != 1 || v.Groups[0].EnvID != alpha {
		t.Errorf("viewer sees %+v", v)
	}
	if asked := f.fake.asked(); !slices.Equal(asked, []string{alpha}) {
		t.Errorf("environments read for the viewer = %v, want only Alpha", asked)
	}
}
