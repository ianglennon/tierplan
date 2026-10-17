// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The container list's server-side search, status filter and counts (plan 04-06,
// D-03, T-04-29, T-04-31). The engine is a FakeDaemon behind the real monitor.

// filterItem is a container row as the list page reads it, including the fields the
// filter adds.
type filterItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Project  string `json:"project"`
	Stack    string `json:"stack"`
	Service  string `json:"service"`
	Health   string `json:"health"`
	SelfKind string `json:"selfKind"`
	ReadOnly bool   `json:"readOnly"`
	IsSelf   bool   `json:"isSelf"`
}

type statusCounts struct {
	Running    int `json:"running"`
	Exited     int `json:"exited"`
	Paused     int `json:"paused"`
	Restarting int `json:"restarting"`
	Unhealthy  int `json:"unhealthy"`
}

type filterList struct {
	Items           []filterItem `json:"items"`
	Total           int          `json:"total"`
	Truncated       bool         `json:"truncated"`
	UnfilteredTotal int          `json:"unfilteredTotal"`
	StatusCounts    statusCounts `json:"statusCounts"`
}

func listContainersFor(t *testing.T, f *envFix, e envBody, query string) filterList {
	t.Helper()
	rec := f.get(t, e, "containers"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("containers%s = %d %s", query, rec.code, rec.body)
	}
	return decode[filterList](t, rec)
}

// manyContainers is n numbered containers, three of them in the compose project shop.
func manyContainers(n int) dockertest.FakeConfig {
	cs := make([]dockertest.FakeContainer, 0, n)
	for i := range n {
		c := dockertest.FakeContainer{
			ID:    fmt.Sprintf("%064x", i+1),
			Names: []string{fmt.Sprintf("/c%03d", i)}, Image: "busybox:1", State: "running", Status: "Up 1 hour",
		}
		if i < 3 {
			c.Labels = map[string]string{"com.docker.compose.project": "shop"}
		}
		cs = append(cs, c)
	}
	return dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", Containers: cs}
}

func TestContainerFilterSearchesTheComposeProjectBeforeTheCap(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, manyContainers(600))
	got := listContainersFor(t, f, e, "?q=shop")
	if len(got.Items) != 3 || got.Total != 3 || got.Truncated || got.UnfilteredTotal != 600 {
		t.Fatalf("q=shop: items %d total %d truncated %v unfiltered %d", len(got.Items), got.Total, got.Truncated, got.UnfilteredTotal)
	}
	for _, it := range got.Items {
		if it.Project != "shop" {
			t.Errorf("project of %s = %q, want shop", it.Name, it.Project)
		}
	}
	if up := listContainersFor(t, f, e, "?q=SHOP"); up.Total != 3 {
		t.Errorf("q=SHOP total = %d, want 3 (case-insensitive)", up.Total)
	}
	if empty := listContainersFor(t, f, e, "?q="); empty.Total != 600 {
		t.Errorf("q= total = %d, want 600 (no search)", empty.Total)
	}
}

func TestContainerFilterWithNoSearchKeepsTheFirst500ByName(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, manyContainers(600))
	got := listContainersFor(t, f, e, "")
	if len(got.Items) != 500 || got.Total != 600 || !got.Truncated || got.UnfilteredTotal != 600 {
		t.Fatalf("items %d total %d truncated %v unfiltered %d", len(got.Items), got.Total, got.Truncated, got.UnfilteredTotal)
	}
	if got.Items[0].Name != "c000" || got.Items[499].Name != "c499" {
		t.Errorf("order = %s ... %s, want name order", got.Items[0].Name, got.Items[499].Name)
	}
}

func TestContainerFilterRefusesAQueryOver128Characters(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, manyContainers(3))
	rec := f.get(t, e, "containers?q="+url.QueryEscape(strings.Repeat("a", 129)))
	if rec.code != http.StatusUnprocessableEntity {
		t.Errorf("129-character q = %d %s, want 422", rec.code, rec.body)
	}
	if rec := f.get(t, e, "containers?q="+strings.Repeat("a", 128)); rec.code != http.StatusOK {
		t.Errorf("128-character q = %d %s, want 200", rec.code, rec.body)
	}
}

func TestContainerFilterFindsALabelValueButNeverSendsItBack(t *testing.T) {
	t.Parallel()
	cfg := manyContainers(2)
	cfg.Containers[1].Labels = map[string]string{"secret.token": "abc-9f3", "com.docker.stack.namespace": "blog", "com.docker.swarm.service.name": "blog_web"}
	cfg.Containers[1].Mounts = []string{"/srv/mount-9f3"}
	f, e := dashEnv(t, cfg)
	rec := f.get(t, e, "containers?q=abc-9f3")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	got := decode[filterList](t, rec)
	if got.Total != 1 || got.Items[0].Name != "c001" || got.Items[0].Stack != "blog" || got.Items[0].Service != "blog_web" {
		t.Fatalf("q=abc-9f3 = %+v", got)
	}
	for _, leaked := range []string{"abc-9f3", "secret.token", "mount-9f3"} {
		if strings.Contains(string(rec.body), leaked) {
			t.Errorf("the list carries %q (T-04-29): %s", leaked, rec.body)
		}
	}
}

// statusEngine is one container in each state, plus an unhealthy one and a container of
// Bayplan's own agent.
func statusEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", Containers: []dockertest.FakeContainer{
		{ID: selfID + "ffffffffffffffffffffffffffffffffffffffffffffffffffff", Names: []string{"/bayplan"}, Image: "bayplan:edge", State: "running", Status: "Up 1 hour", Health: "healthy"},
		{ID: "a1", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours (unhealthy)",
			Labels: map[string]string{"com.docker.compose.project": "shop"}},
		{ID: "a2", Names: []string{"/cache"}, Image: "redis:8", State: "running", Status: "Up 2 hours", Health: "unhealthy"},
		{ID: "a3", Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago"},
		{ID: "a4", Names: []string{"/paused"}, Image: "busybox", State: "paused", Status: "Up 1 hour (Paused)"},
		{ID: "a5", Names: []string{"/loop"}, Image: "busybox", State: "restarting", Status: "Restarting (1) 5 seconds ago"},
		{ID: "a6", Names: []string{"/starting"}, Image: "busybox", State: "running", Status: "Up 3 seconds (health: starting)"},
		{ID: "a7", Names: []string{"/agent"}, Image: "ghcr.io/ianglennon/bayplan-agent:0.1.0", State: "running", Status: "Up 1 hour",
			Labels: map[string]string{"bayplan.agent": "true"}},
		{ID: "a8", Names: []string{"/api_1"}, Image: "api:1", State: "running", Status: "Up 1 hour",
			Labels: map[string]string{"com.docker.stack.namespace": "blog", "com.docker.swarm.service.name": "blog_api"}},
	}}
}

func names(items []filterItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestContainerFilterStatusKeepsTheRowsOfAnyListedStatus(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	for query, want := range map[string]string{
		"?status=exited":               "db",
		"?status=paused":               "paused",
		"?status=restarting":           "loop",
		"?status=unhealthy":            "cache web",
		"?status=exited,paused":        "db paused",
		"?status=exited,unhealthy":     "cache db web",
		"?status=restarting,%20exited": "db loop",
	} {
		got := listContainersFor(t, f, e, query)
		if strings.Join(names(got.Items), " ") != want || got.Total != len(got.Items) || got.UnfilteredTotal != 9 {
			t.Errorf("%s = %v total %d unfiltered %d, want %q", query, names(got.Items), got.Total, got.UnfilteredTotal, want)
		}
	}
	if got := listContainersFor(t, f, e, "?status=running"); got.Total != 6 {
		t.Errorf("status=running total = %d, want 6", got.Total)
	}
	if got := listContainersFor(t, f, e, "?status=exited&q=nomatch"); got.Total != 0 || len(got.Items) != 0 {
		t.Errorf("status plus search with no match = %+v", got)
	}
}

func TestContainerFilterStatusCountsIgnoreTheSearchAndTheStatus(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	want := statusCounts{Running: 6, Exited: 1, Paused: 1, Restarting: 1, Unhealthy: 2}
	for _, query := range []string{"", "?q=shop", "?status=exited", "?q=nomatch&status=paused,restarting"} {
		if got := listContainersFor(t, f, e, query).StatusCounts; got != want {
			t.Errorf("%q: statusCounts = %+v, want %+v", query, got, want)
		}
	}
}

func TestContainerFilterRefusesAnUnknownStatus(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	for _, query := range []string{"?status=bogus", "?status=running,bogus", "?status=created", "?status=" + strings.Repeat("a", 65)} {
		if rec := f.get(t, e, "containers"+query); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s, want 422", query, rec.code, rec.body)
		}
	}
	if rec := f.get(t, e, "containers?status="); rec.code != http.StatusOK {
		t.Errorf("an empty status = %d, want 200 (no filter)", rec.code)
	}
}

func TestContainerFilterHealthShowsUnhealthyOnEveryEngine(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	health := map[string]string{}
	for _, it := range listContainersFor(t, f, e, "").Items {
		health[it.Name] = it.Health
	}
	for name, want := range map[string]string{
		"web": "unhealthy", "cache": "unhealthy", "starting": "starting", "bayplan": "healthy", "db": "none",
	} {
		if health[name] != want {
			t.Errorf("health of %s = %q, want %q", name, health[name], want)
		}
	}
}

func TestContainerFilterSelfKindNamesTheServerAndTheAgent(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	kinds := map[string]filterItem{}
	for _, it := range listContainersFor(t, f, e, "").Items {
		kinds[it.Name] = it
	}
	if k := kinds["bayplan"]; k.SelfKind != "server" || !k.IsSelf {
		t.Errorf("bayplan = %+v, want selfKind server and isSelf", k)
	}
	if k := kinds["agent"]; k.SelfKind != "agent" || k.IsSelf {
		t.Errorf("agent = %+v, want selfKind agent and not isSelf", k)
	}
	for _, name := range []string{"web", "db"} {
		if k := kinds[name]; k.SelfKind != "" || k.IsSelf {
			t.Errorf("%s = %+v, want no selfKind", name, k)
		}
	}
}

func TestContainerFilterStackAndServiceComeFromTheirLabels(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, statusEngine())
	got := listContainersFor(t, f, e, "?q=blog_api")
	if got.Total != 1 || got.Items[0].Stack != "blog" || got.Items[0].Service != "blog_api" || got.Items[0].Project != "" {
		t.Errorf("q=blog_api = %+v", got)
	}
}

func TestContainerFilterReadOnlyIsTrueOnlyOnTheRowsOfAReadOnlyHost(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Locked")
	f.connect(t, env, dockertest.FakeConfig{
		Hostname: "node-1", Containers: []dockertest.FakeContainer{{ID: "a1", Names: []string{"/web"}, State: "running", Status: "Up 1 hour"}},
	}, &wire.Facts{EngineID: "ENG:ro", Hostname: "node-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true}})
	f.whenState(t, env.ID, "online")
	rec := f.req(t, http.MethodGet, "/api/env/"+env.ID+"/containers", "")
	if rec.code != http.StatusOK {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if got := decode[filterList](t, rec); len(got.Items) != 1 || !got.Items[0].ReadOnly {
		t.Errorf("rows = %+v, want one read-only row", got.Items)
	}

	f2, e2 := dashEnv(t, statusEngine())
	for _, it := range listContainersFor(t, f2, e2, "").Items {
		if it.ReadOnly {
			t.Errorf("%s is read-only on the local socket", it.Name)
		}
	}
}
