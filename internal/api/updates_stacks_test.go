// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The update state of stacks (plan 12-07, IMG-05): a stack's workloads rolled up, and the state of each of
// its services. Docker is a fake daemon behind the real monitor, snapshot cache, aggregator and cluster
// reader; the registry is a scripted Heads; the update service, its SQLite store, the router, policy and
// permission checks are the real ones.

// stackUpdateAnswer is the roll-up a stack row carries.
type stackUpdateAnswer struct {
	State        string   `json:"state"`
	Count        int      `json:"count"`
	NotCheckable []string `json:"notCheckable"`
}

// stackRowUpdate is a stack row as far as its update goes.
type stackRowUpdate struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Update *stackUpdateAnswer `json:"update"`
}

type stackRowsUpdate struct {
	Items []stackRowUpdate `json:"items"`
	Total int              `json:"total"`
}

// of is the roll-up of the stack named name, nil when it has none.
func (l stackRowsUpdate) of(t *testing.T, name string) *stackUpdateAnswer {
	t.Helper()
	for _, it := range l.Items {
		if it.Name == name {
			return it.Update
		}
	}
	t.Fatalf("no stack %q in %+v", name, l)
	return nil
}

func (l stackRowsUpdate) has(name string) bool {
	for _, it := range l.Items {
		if it.Name == name {
			return true
		}
	}
	return false
}

// stackServicesUpdate is the services card of a stack as far as its update goes.
type stackServicesUpdate struct {
	Items []struct {
		ID     string      `json:"id"`
		Name   string      `json:"name"`
		Short  string      `json:"short"`
		Update *updateInfo `json:"update"`
	} `json:"items"`
}

func (s stackServicesUpdate) of(t *testing.T, short string) *updateInfo {
	t.Helper()
	for _, it := range s.Items {
		if it.Short == short {
			return it.Update
		}
	}
	t.Fatalf("no service %q in %+v", short, s)
	return nil
}

// stackImageService is a service of stack st running digest running of team/<repo>:1, which the stack's file wrote
// as that tag.
func stackImageService(st stackAnswer, short, repo string, running byte) dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-" + st.Name + "-" + short, Name: st.Name + "_" + short, Mode: "replicated", Running: 1, Desired: 1, Version: 1,
		Image: fmt.Sprintf("registry.test/team/%s:1@%s", repo, updDigest(running)),
		Labels: map[string]string{
			"com.docker.stack.namespace": st.Name, "tierplan.stack.id": st.ID,
			"tierplan.stack.image": "registry.test/team/" + repo + ":1",
		},
	}
}

// deployedSwarmStack is a stack whose record says it deployed once. The services it runs are the test's to set
// with reconfigureSwarm: a real deploy would add the starter file's service, which no registry here knows.
func deployedSwarmStack(t *testing.T, f *envFix, e envBody, admin *roleCaller, name string) stackAnswer {
	t.Helper()
	st := newStack(t, admin, name)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	ctx := testsupport.Context(t)
	row, err := repo.StartDeploy(ctx, st.ID, e.ID, "deploy", 0, "job-"+name, "ada")
	if err != nil {
		t.Fatalf("record a deploy: %v", err)
	}
	if err := repo.FinishDeploy(ctx, row.ID, "done", "", ""); err != nil {
		t.Fatalf("finish a deploy: %v", err)
	}
	return st
}

func TestStackListAggregatesUpdates(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/a": {digest: updDigest('b')},
		"team/b": {digest: updDigest('b')},
		"team/c": {digest: updDigest('c')},
	})
	f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads = heads })
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := deployedSwarmStack(t, f, e, admin, "shop")
	reconfigureSwarm(t, f, e,
		stackImageService(st, "web", "a", 'a'),
		stackImageService(st, "api", "b", 'a'),
		stackImageService(st, "db", "c", 'c'),
	)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	list := answerOf[stackRowsUpdate](t, admin.do(http.MethodGet, "stacks", ""))
	got := list.of(t, "shop")
	if got == nil || got.State != "update_available" || got.Count != 2 {
		t.Errorf("shop update = %+v, want update_available with 2: two of three services have moved tags", got)
	}

	// The stack's own page carries the same roll-up.
	detail := answerOf[stackRowUpdate](t, admin.do(http.MethodGet, "stacks/"+st.ID, ""))
	if detail.Update == nil || detail.Update.State != "update_available" || detail.Update.Count != 2 {
		t.Errorf("detail update = %+v, want update_available with 2", detail.Update)
	}

	// The services card says which two.
	rec := admin.do(http.MethodGet, "stacks/"+st.ID+"/services", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("services: %d %s", rec.Code, rec.Body)
	}
	services := answerOf[stackServicesUpdate](t, rec)
	for short, want := range map[string]string{"web": "update_available", "api": "update_available", "db": "up_to_date"} {
		if u := services.of(t, short); u == nil || u.State != want {
			t.Errorf("%s = %+v, want %s", short, u, want)
		}
	}
}

// unmanagedUpdateAnswer is an unmanaged stack as far as its update goes.
type unmanagedUpdateAnswer struct {
	Update   *stackUpdateAnswer `json:"update"`
	Services []struct {
		Name   string      `json:"name"`
		Short  string      `json:"short"`
		Update *updateInfo `json:"update"`
	} `json:"services"`
}

func (u unmanagedUpdateAnswer) of(t *testing.T, name string) *updateInfo {
	t.Helper()
	for _, s := range u.Services {
		if s.Name == name {
			return s.Update
		}
	}
	t.Fatalf("no service %q in %+v", name, u)
	return nil
}

func TestEveryStackSurfaceCarriesUpdates(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/a": {digest: updDigest('b')},
		"team/c": {digest: updDigest('c')},
	})

	t.Run("a swarm: the all-environments list, the detail of a stack, its services, an unmanaged stack and one service", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads = heads })
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := deployedSwarmStack(t, f, e, admin, "shop")
		old := stackImageService(st, "web", "a", 'a')
		legacyWeb := dockertest.FakeService{
			ID: "svc-legacy-web", Name: "legacy_web", Mode: "replicated", Running: 1, Desired: 1, Version: 1,
			Image:  fmt.Sprintf("registry.test/team/a:1@%s", updDigest('a')),
			Labels: map[string]string{"com.docker.stack.namespace": "legacy"},
		}
		legacyDB := dockertest.FakeService{
			ID: "svc-legacy-db", Name: "legacy_db", Mode: "replicated", Running: 1, Desired: 1, Version: 1,
			Image:  fmt.Sprintf("registry.test/team/c:1@%s", updDigest('c')),
			Labels: map[string]string{"com.docker.stack.namespace": "legacy"},
		}
		reconfigureSwarm(t, f, e, old, legacyWeb, legacyDB)
		if rec := f.checkNow(t, e); rec.code != http.StatusOK {
			t.Fatalf("check now: %d %s", rec.code, rec.body)
		}

		// GET /api/stacks: each group's stacks carry the roll-up, managed or not.
		all := answerOf[struct {
			Groups []struct {
				Stacks []stackRowUpdate `json:"stacks"`
			} `json:"groups"`
		}](t, admin.f.call(t, authReq{method: http.MethodGet, path: "/api/stacks", cookie: admin.cookie, csrf: admin.csrf}))
		seen := map[string]*stackUpdateAnswer{}
		for _, g := range all.Groups {
			for _, s := range g.Stacks {
				seen[s.Name] = s.Update
			}
		}
		if u := seen["shop"]; u == nil || u.State != "update_available" || u.Count != 1 {
			t.Errorf("all-stacks shop = %+v, want update_available with 1", u)
		}
		if u := seen["legacy"]; u == nil || u.State != "update_available" || u.Count != 1 {
			t.Errorf("all-stacks legacy (unmanaged) = %+v, want update_available with 1", u)
		}

		// The unmanaged stack's page: the roll-up and each service.
		rec := admin.do(http.MethodGet, unmanagedPath("legacy", ""), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("unmanaged: %d %s", rec.Code, rec.Body)
		}
		um := answerOf[unmanagedUpdateAnswer](t, rec)
		if um.Update == nil || um.Update.State != "update_available" || um.Update.Count != 1 {
			t.Errorf("unmanaged update = %+v, want update_available with 1", um.Update)
		}
		if u := um.of(t, "legacy_web"); u == nil || u.State != "update_available" || u.Repository != "team/a" {
			t.Errorf("legacy_web = %+v, want update_available for team/a", u)
		}
		if u := um.of(t, "legacy_db"); u == nil || u.State != "up_to_date" {
			t.Errorf("legacy_db = %+v, want up_to_date", u)
		}

		// One service's detail carries its update object.
		detail := decode[struct {
			Update *updateInfo `json:"update"`
		}](t, f.get(t, e, "services/svc-legacy-web"))
		if detail.Update == nil || detail.Update.State != "update_available" || detail.Update.RemoteDigest != updDigest('b') {
			t.Errorf("service detail update = %+v, want update_available", detail.Update)
		}
	})

	t.Run("a stack none of whose services can be checked reads none, never up to date", func(t *testing.T) {
		t.Parallel()
		f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads = heads })
		admin := f.caller(t, e, "envadmin-1", "admin")
		st := deployedSwarmStack(t, f, e, admin, "pinned")
		web := stackImageService(st, "web", "a", 'a')
		// The file names a digest and no tag: it can never update.
		web.Labels["tierplan.stack.image"] = "registry.test/team/a@" + updDigest('a')
		reconfigureSwarm(t, f, e, web)
		if rec := f.checkNow(t, e); rec.code != http.StatusOK {
			t.Fatalf("check now: %d %s", rec.code, rec.body)
		}

		got := answerOf[stackRowsUpdate](t, admin.do(http.MethodGet, "stacks", "")).of(t, "pinned")
		if got == nil || got.State != "none" || len(got.NotCheckable) != 1 || got.NotCheckable[0] != "pinned_in_file" {
			t.Errorf("pinned update = %+v, want none with pinned_in_file", got)
		}
		services := answerOf[stackServicesUpdate](t, admin.do(http.MethodGet, "stacks/"+st.ID+"/services", ""))
		if u := services.of(t, "web"); u == nil || u.State != "pinned_in_file" {
			t.Errorf("web = %+v, want pinned_in_file", u)
		}
	})

	t.Run("a standalone host: a service's containers run one image and count once", func(t *testing.T) {
		t.Parallel()
		compose := func(service string) map[string]string {
			return map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": service}
		}
		f, e := hostWithContainers(t, heads, nil,
			[]dockertest.FakeContainer{
				appContainer("shop-web-1", "registry.test/team/a:1", updImage('1'), compose("web")),
				appContainer("shop-web-2", "registry.test/team/a:1", updImage('1'), compose("web")),
				appContainer("shop-db-1", "registry.test/team/c:1", updImage('2'), compose("db")),
			},
			[]dockertest.FakeImage{
				{ID: updImage('1'), RepoTags: []string{"registry.test/team/a:1"}, RepoDigests: []string{"registry.test/team/a@" + updDigest('a')}},
				{ID: updImage('2'), RepoTags: []string{"registry.test/team/c:1"}, RepoDigests: []string{"registry.test/team/c@" + updDigest('c')}},
			})
		if rec := f.checkNow(t, e); rec.code != http.StatusOK {
			t.Fatalf("check now: %d %s", rec.code, rec.body)
		}

		rec := f.get(t, e, "unmanaged-stacks/shop")
		if rec.code != http.StatusOK {
			t.Fatalf("unmanaged: %d %s", rec.code, rec.body)
		}
		um := decode[unmanagedUpdateAnswer](t, rec)
		if um.Update == nil || um.Update.State != "update_available" || um.Update.Count != 1 {
			t.Errorf("unmanaged update = %+v, want update_available with 1: two containers of one service are one update", um.Update)
		}
		if u := um.of(t, "web"); u == nil || u.State != "update_available" {
			t.Errorf("web = %+v, want update_available", u)
		}
		if u := um.of(t, "db"); u == nil || u.State != "up_to_date" {
			t.Errorf("db = %+v, want up_to_date", u)
		}
	})
}

type filteredServices struct {
	Items []struct {
		Name   string      `json:"name"`
		Update *updateInfo `json:"update"`
	} `json:"items"`
	Total           int `json:"total"`
	UnfilteredTotal int `json:"unfilteredTotal"`
}

func (l filteredServices) names() []string {
	out := make([]string, 0, len(l.Items))
	for _, it := range l.Items {
		out = append(out, it.Name)
	}
	return out
}

func TestUpdatesAvailableFilters(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/a": {digest: updDigest('b')},
		"team/c": {digest: updDigest('c')},
	})
	f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads = heads })
	admin := f.caller(t, e, "envadmin-1", "admin")
	shop := deployedSwarmStack(t, f, e, admin, "shop")
	calm := deployedSwarmStack(t, f, e, admin, "calm")
	alone := dockertest.FakeService{
		ID: "svc-alone", Name: "alone", Mode: "replicated", Running: 1, Desired: 1, Version: 1,
		Image: fmt.Sprintf("registry.test/team/a:1@%s", updDigest('a')),
	}
	reconfigureSwarm(t, f, e,
		stackImageService(shop, "web", "a", 'a'),
		stackImageService(shop, "db", "c", 'c'),
		stackImageService(calm, "db", "c", 'c'),
		alone,
	)
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	// Services: only those with an update, counted as such.
	all := decode[filteredServices](t, f.get(t, e, "services"))
	if all.Total != 4 {
		t.Fatalf("unfiltered services = %v, want 4", all.names())
	}
	got := decode[filteredServices](t, f.get(t, e, "services?updates=available"))
	if !equalNames(got.names(), []string{"alone", "shop_web"}) || got.Total != 2 || got.UnfilteredTotal != 4 {
		t.Errorf("services?updates=available = %v total %d of %d, want alone and shop_web, 2 of 4", got.names(), got.Total, got.UnfilteredTotal)
	}
	for _, it := range got.Items {
		if it.Update == nil || it.Update.State != "update_available" {
			t.Errorf("%s = %+v, want an update object that says update_available", it.Name, it.Update)
		}
	}
	// The filter meets the others: a search that matches nothing with an update is an empty list.
	if got := decode[filteredServices](t, f.get(t, e, "services?updates=available&q=calm")); got.Total != 0 {
		t.Errorf("services?updates=available&q=calm = %v, want none", got.names())
	}
	if rec := f.get(t, e, "services?updates=bogus"); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("updates=bogus = %d, want 422", rec.code)
	}

	// Stacks: only those whose roll-up is update_available.
	var names []string
	list := answerOf[stackRowsUpdate](t, admin.do(http.MethodGet, "stacks?updates=available", ""))
	for _, it := range list.Items {
		names = append(names, it.Name)
	}
	if !equalNames(names, []string{"shop"}) || list.Total != 1 {
		t.Errorf("stacks?updates=available = %v total %d, want shop alone", names, list.Total)
	}
	if u := list.of(t, "shop"); u == nil || u.State != "update_available" || u.Count != 1 {
		t.Errorf("shop = %+v, want update_available with 1", u)
	}
	if everything := answerOf[stackRowsUpdate](t, admin.do(http.MethodGet, "stacks", "")); !everything.has("calm") {
		t.Errorf("stacks = %+v, want calm listed without the filter", everything)
	} else if u := everything.of(t, "calm"); u == nil || u.State != "up_to_date" {
		t.Errorf("calm = %+v, want up_to_date: its one checkable service is", u)
	}
}

// A list filtered by update names an environment whose checks cannot be read as failed, where it would say that
// the environment has no update available.
func TestStacksUpdatesFilterNamesAnEnvironmentWithUnreadVerdictsAsFailed(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{})
	f, e := stackEnvTuned(t, func(f *envFix) { f.updateHeads, f.updateStore = heads, failingChecks{} })
	admin := f.caller(t, e, "envadmin-1", "admin")
	newStack(t, admin, "web")

	got := answerOf[allStacksAnswer](t, f.allStacks(t, f.cookie, "?updates=available"))
	if len(got.Groups) != 0 || len(got.FailedEnvironments) != 1 || got.FailedEnvironments[0].EnvID != e.ID {
		t.Errorf("updates=available = %+v, want the environment failed and no group", got)
	}
	got = answerOf[allStacksAnswer](t, f.allStacks(t, f.cookie, ""))
	if len(got.Groups) != 1 || len(got.FailedEnvironments) != 0 {
		t.Errorf("unfiltered = %+v, want the stack listed though the verdicts could not be read", got)
	}
}

func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
