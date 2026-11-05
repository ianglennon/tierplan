// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync/atomic"
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

// fleetWork is what the fake answers Workloads with.
type fleetWork struct {
	list []updates.Workload
	err  error
}

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
			Kind            string `json:"kind"`
			ID              string `json:"id"`
			Name            string `json:"name"`
			Stack           string `json:"stack"`
			TierPlanStackID string `json:"tierplanStackId"`
			Image           string `json:"image"`
			FromDigest      string `json:"fromDigest"`
			ToDigest        string `json:"toDigest"`
			Update          struct {
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

// An item of a stack TierPlan manages carries the stack's ID, so the SPA can open the stack's update console and say the update
// is a new revision of the stack (WR-18). A label that names a stack the environment does not hold, or holds under another name,
// links nothing.
func TestFleetUpdatesItemsCarryTheirManagedStack(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.swarmWith(alpha, nil)
	owned := func(name, stack, id string) updates.WorkloadVerdict {
		v := verdictOf(t, "service", name, stack, updates.StateUpdateAvailable)
		v.Workload.Labels = map[string]string{"com.docker.stack.namespace": stack}
		if id != "" {
			v.Workload.Labels["tierplan.stack.id"] = id
		}
		return v
	}
	f.fake.verdicts[alpha] = fleetVerdicts{list: []updates.WorkloadVerdict{
		owned("own", "shop", "stk-shop"),
		owned("ghost", "gone", "stk-gone"),
		owned("forged", "other", "stk-shop"),
		owned("foreign", "legacy", ""),
	}}
	f.insertStackRow(t, alpha, "stk-shop", "shop")
	f.fleet.Store = f.st
	f.rebuild(t)

	got := map[string]string{}
	for _, g := range f.updatesList(t, "", "").Groups {
		for _, it := range g.Items {
			got[it.Name] = it.TierPlanStackID
		}
	}
	want := map[string]string{"own": "stk-shop", "ghost": "", "forged": "", "foreign": ""}
	if !maps.Equal(got, want) {
		t.Errorf("tierplanStackId by item = %v, want %v", got, want)
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

// fleetCheckSpacing is how long the server makes one person wait between two fleet-wide checks.
const fleetCheckSpacing = 30 * time.Second

// fleetClock is a clock a test moves by hand, for the fleet routes and the update routes.
type fleetClock struct{ ns atomic.Int64 }

func newFleetClock() *fleetClock {
	c := &fleetClock{}
	c.ns.Store(fleetNow.UnixNano())
	return c
}

func (c *fleetClock) now() time.Time { return time.Unix(0, c.ns.Load()).UTC() }

func (c *fleetClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

type fleetCheckResp struct {
	Checked int `json:"checked"`
	Updates int `json:"updates"`
	Failed  int `json:"failed"`
	Skipped []struct {
		EnvID   string `json:"envId"`
		EnvName string `json:"envName"`
		Reason  string `json:"reason"`
	} `json:"skipped"`
}

// checkFleet posts the fleet-wide Check now as the admin (as empty) or the named user.
func (f *fleetFix) checkFleet(t *testing.T, as, body string) (*envRec, fleetCheckResp) {
	t.Helper()
	var rec *envRec
	if as == "" {
		rec = f.req(t, http.MethodPost, "/api/fleet/updates/check", body)
	} else {
		rec = f.asUser(t, as, http.MethodPost, "/api/fleet/updates/check", body)
	}
	var r fleetCheckResp
	if rec.code == http.StatusOK {
		if err := json.Unmarshal(rec.body, &r); err != nil {
			t.Fatalf("decode %s: %v", rec.body, err)
		}
	}
	return rec, r
}

func TestFleetCheckNowSkipsUnpermitted(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma", "Delta")
	alpha, beta, gamma, delta := f.id("Alpha"), f.id("Beta"), f.id("Gamma"), f.id("Delta")
	clk := newFleetClock()
	f.fleet.Now = clk.now
	for _, id := range []string{alpha, beta} {
		f.fake.online(id)
	}
	f.fake.down(gamma, envs.StateOffline)
	f.fake.work[alpha] = fleetWork{list: []updates.Workload{{Kind: "container", ID: "c1", Name: "cache"}}}
	f.fake.work[gamma] = fleetWork{err: errNoRead}
	f.fake.checkRes[alpha] = updates.CheckResult{Checked: 2, Failed: 1}
	f.fake.tally[alpha] = fleetTally{t: updates.Tally{Available: 3}}
	// Olive may check Alpha and Gamma, only look at Beta, and knows nothing of Delta.
	f.addEnvUser(t, "olive", alpha, "operator")
	f.addEnvRole(t, "olive", gamma, "operator")
	f.addEnvRole(t, "olive", beta, "viewer")
	f.rebuild(t)

	skipped := func(r fleetCheckResp) []string {
		var out []string
		for _, s := range r.Skipped {
			out = append(out, s.EnvName+":"+s.Reason)
		}
		return out
	}
	rec, got := f.checkFleet(t, "olive", `{"envIds":["`+alpha+`","`+beta+`","`+gamma+`","`+delta+`"]}`)
	if rec.code != http.StatusOK {
		t.Fatalf("fleet check as an Operator = %d %s", rec.code, rec.body)
	}
	if got.Checked != 2 || got.Failed != 1 || got.Updates != 3 {
		t.Errorf("answer = %+v, want 2 checked, 1 failed, 3 updates", got)
	}
	if want := []string{"Beta:no_permission", "Gamma:offline"}; !slices.Equal(skipped(got), want) {
		t.Errorf("skipped = %v, want %v: Delta is not hers to know of", skipped(got), want)
	}
	if !slices.Equal(f.fake.checked[0], []string{alpha}) || len(f.fake.checked) != 1 {
		t.Errorf("environments checked = %v, want one pass over Alpha only", f.fake.checked)
	}
	for _, id := range f.fake.asked() {
		if id == delta {
			t.Errorf("Delta was read for a person who may not read it")
		}
	}

	// With no environments named, every readable one she may check is checked.
	clk.advance(fleetCheckSpacing)
	rec, got = f.checkFleet(t, "olive", "")
	if rec.code != http.StatusOK || got.Checked != 2 || len(got.Skipped) != 2 {
		t.Errorf("fleet check with no body = %d %s, want Alpha checked and Beta and Gamma skipped", rec.code, rec.body)
	}
	if !slices.Equal(f.fake.checked[1], []string{alpha}) {
		t.Errorf("second pass = %v, want Alpha only", f.fake.checked[1])
	}

	// An administrator checks what is reachable: Alpha and Beta; Gamma is offline and Delta has nothing to read.
	clk.advance(fleetCheckSpacing)
	f.fake.online(delta)
	rec, got = f.checkFleet(t, "", "")
	if rec.code != http.StatusOK || len(got.Skipped) != 1 || got.Skipped[0].EnvName != "Gamma" || got.Skipped[0].Reason != "offline" {
		t.Errorf("admin fleet check = %d %s, want only Gamma skipped, offline", rec.code, rec.body)
	}
	if !slices.Equal(f.fake.checked[2], []string{alpha, beta, delta}) {
		t.Errorf("admin pass = %v, want Alpha, Beta and Delta", f.fake.checked[2])
	}
	if rec.body != nil && !bytes.Contains(rec.body, []byte(`"skipped"`)) {
		t.Errorf("answer %s has no skipped list", rec.body)
	}
}

func TestFleetCheckNowRateLimited(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	clk := newFleetClock()
	f.fleet.Now = clk.now
	f.fake.online(alpha)
	f.addEnvUser(t, "olive", alpha, "operator")
	f.rebuild(t)

	if rec, _ := f.checkFleet(t, "olive", ""); rec.code != http.StatusOK {
		t.Fatalf("first check = %d %s", rec.code, rec.body)
	}
	clk.advance(fleetCheckSpacing - time.Second)
	if rec, _ := f.checkFleet(t, "olive", ""); rec.code != http.StatusTooManyRequests || rec.ptyp != "too_many_requests" {
		t.Errorf("second check within 30 s = %d %s (%q), want 429 too_many_requests", rec.code, rec.body, rec.ptyp)
	}
	// Someone else is not held back by her check.
	if rec, _ := f.checkFleet(t, "", ""); rec.code != http.StatusOK {
		t.Errorf("another person's check = %d %s, want 200", rec.code, rec.body)
	}
	clk.advance(time.Second)
	if rec, _ := f.checkFleet(t, "olive", ""); rec.code != http.StatusOK {
		t.Errorf("check after 30 s = %d %s, want 200", rec.code, rec.body)
	}
	if n := len(f.fake.checked); n != 3 {
		t.Errorf("%d passes over the registries, want 3: the refused one asked nobody", n)
	}
}

func TestFleetCoverageStates(t *testing.T) {
	t.Parallel()
	at := func(h time.Duration) time.Time { return fleetNow.Add(-h * time.Hour) }
	run := func(t *testing.T, cov updates.Coverage, list ...updates.WorkloadVerdict) fleetUpdatesResp {
		t.Helper()
		f := newFleetFix(t, "Alpha")
		f.fake.online(f.id("Alpha"))
		f.fake.verdicts[f.id("Alpha")] = fleetVerdicts{list: list, cov: cov}
		f.rebuild(t)
		return f.updatesList(t, "", "")
	}

	t.Run("everything checked and nothing newer", func(t *testing.T) {
		t.Parallel()
		got := run(t, updates.Coverage{Checkable: 2, Checked: 2, LastCheckedAt: at(1)},
			verdictOf(t, "container", "a", "", updates.StateUpToDate), verdictOf(t, "container", "b", "", updates.StateUpToDate))
		c := got.Coverage
		if c.Checked != c.Checkable || c.Checkable != 2 || c.Failed != 0 || c.NeverChecked != 0 || c.Skipped != 0 || got.Counts.All != 0 || len(got.Groups) != 0 {
			t.Errorf("coverage %+v counts %+v groups %d, want 2 of 2 checked, nothing to list", c, got.Counts, len(got.Groups))
		}
		if c.LastCheckedAt == "" || c.FirstCheckAt != "" || c.Environments != 1 {
			t.Errorf("coverage = %+v, want a last check, no first check pending, one environment", c)
		}
	})
	t.Run("some checks failed", func(t *testing.T) {
		t.Parallel()
		got := run(t, updates.Coverage{Checkable: 2, Checked: 2, Failed: 1, LastCheckedAt: at(1)},
			verdictOf(t, "container", "a", "", updates.StateUpToDate), failedVerdict(t, "container", "b"))
		if got.Coverage.Failed != 1 || got.Counts.Updates != 0 || got.Counts.Problems != 1 || len(got.Groups) != 0 {
			t.Errorf("coverage %+v counts %+v groups %d, want 1 failed, no updates, the failure under Problems and not listed under Updates", got.Coverage, got.Counts, len(got.Groups))
		}
	})
	t.Run("some never checked", func(t *testing.T) {
		t.Parallel()
		next := fleetNow.Add(3 * time.Hour)
		got := run(t, updates.Coverage{Checkable: 3, Checked: 1, NeverChecked: 2, LastCheckedAt: at(5), NextCheckAt: &next},
			verdictOf(t, "container", "a", "", updates.StateUpToDate),
			verdictOf(t, "container", "b", "", updates.StateNotChecked), verdictOf(t, "container", "c", "", updates.StateNotChecked))
		if got.Coverage.NeverChecked != 2 || got.Coverage.FirstCheckAt != next.Format(time.RFC3339) {
			t.Errorf("coverage = %+v, want 2 never checked and the first check at %s", got.Coverage, next.Format(time.RFC3339))
		}
	})
	t.Run("only skipped and ignored remain", func(t *testing.T) {
		t.Parallel()
		got := run(t, updates.Coverage{Checkable: 3, Checked: 3, LastCheckedAt: at(1)},
			verdictOf(t, "container", "a", "", updates.StateSkipped), verdictOf(t, "container", "b", "", updates.StateIgnored),
			verdictOf(t, "container", "c", "", updates.StateUpToDate))
		if got.Coverage.Skipped != 2 || got.Counts.Updates != 0 || got.Counts.Ignored != 2 || len(got.Groups) != 0 {
			t.Errorf("coverage %+v counts %+v, want 2 skipped or ignored and nothing under Updates", got.Coverage, got.Counts)
		}
	})
	t.Run("no environment is read", func(t *testing.T) {
		t.Parallel()
		f := newFleetFix(t, "Alpha")
		f.fake.down(f.id("Alpha"), envs.StateOffline)
		f.rebuild(t)
		got := f.updatesList(t, "", "")
		if got.Coverage.Environments != 0 || len(got.NotIncluded) != 1 {
			t.Errorf("coverage %+v notIncluded %+v, want no environment included and Alpha named", got.Coverage, got.NotIncluded)
		}
	})
}

func TestFleetUpdatesOrderStable(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Zulu", "Alpha", "Mike", "Broken")
	for _, n := range []string{"Zulu", "Alpha", "Mike", "Broken"} {
		f.fake.online(f.id(n))
	}
	// The same names in every environment, a service and a container of one name in each.
	same := func() []updates.WorkloadVerdict {
		return []updates.WorkloadVerdict{
			verdictOf(t, "service", "web", "", updates.StateUpdateAvailable),
			verdictOf(t, "container", "web", "", updates.StateUpdateAvailable),
			verdictOf(t, "container", "api", "", updates.StateUpdateAvailable),
		}
	}
	for _, n := range []string{"Zulu", "Alpha", "Mike"} {
		f.fake.verdicts[f.id(n)] = fleetVerdicts{list: same()}
	}
	f.fake.verdicts[f.id("Broken")] = fleetVerdicts{list: append(same(), failedVerdict(t, "container", "zz"))}
	f.rebuild(t)

	first := f.fleetGet(t, "", "/api/fleet/updates")
	second := f.fleetGet(t, "", "/api/fleet/updates")
	if first.code != http.StatusOK || !bytes.Equal(first.body, second.body) {
		t.Fatalf("two calls with the same input differ:\n%s\n%s", first.body, second.body)
	}
	got := f.updatesList(t, "", "")
	var groups []string
	for _, g := range got.Groups {
		groups = append(groups, g.EnvName)
	}
	if want := []string{"Broken", "Alpha", "Mike", "Zulu"}; !slices.Equal(groups, want) {
		t.Errorf("groups = %v, want %v: the failed check first, then by name", groups, want)
	}
	var items []string
	for _, it := range got.Groups[1].Items {
		items = append(items, it.Name+"/"+it.Kind)
	}
	if want := []string{"api/container", "web/container", "web/service"}; !slices.Equal(items, want) {
		t.Errorf("items = %v, want %v: by name, then kind", items, want)
	}
}

func TestEnvCheckNowRateLimited(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('a')}})
	f, e := updatesEnv(t, heads, updatedService("web", 'a'))

	if rec := f.checkNowSoon(t, e); rec.code != http.StatusOK {
		t.Fatalf("first check = %d %s", rec.code, rec.body)
	}
	f.clk.t = f.clk.t.Add(envCheckSpacing - time.Second)
	if rec := f.checkNowSoon(t, e); rec.code != http.StatusTooManyRequests || rec.ptyp != "too_many_requests" {
		t.Errorf("second check within 10 s = %d %s (%q), want 429 too_many_requests", rec.code, rec.body, rec.ptyp)
	}
	if got := heads.requests(); got != 1 {
		t.Errorf("the refused check asked the registry: %d requests, want 1", got)
	}
	// Another person is not held back by it.
	if rec := f.checkNowAs(t, e, "olive", "operator"); rec.Code != http.StatusOK {
		t.Errorf("another person's check = %d %s, want 200", rec.Code, rec.Body)
	}
	f.clk.t = f.clk.t.Add(time.Second)
	if rec := f.checkNowSoon(t, e); rec.code != http.StatusOK {
		t.Errorf("check after 10 s = %d %s, want 200", rec.code, rec.body)
	}
}
