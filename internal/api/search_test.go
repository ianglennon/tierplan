// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Search across the environments (plan 12-32, ENV-11, D-18). The fixtures are those of fleet_test.go.

type searchHit struct {
	Type     string `json:"type"`
	EnvID    string `json:"envId"`
	EnvName  string `json:"envName"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Managed  bool   `json:"managed"`
	Stack    string `json:"stack"`
	Image    string `json:"image"`
	Node     string `json:"node"`
	Driver   string `json:"driver"`
	Subnet   string `json:"subnet"`
	Size     int64  `json:"size"`
	Services int    `json:"services"`
}

type searchResp struct {
	Results []searchHit `json:"results"`
	Counts  struct {
		Stack, Service, Container, Image, Network, All int
	} `json:"counts"`
	Truncated   bool               `json:"truncated"`
	NotSearched []fleetNotIncluded `json:"notSearched"`
	SearchedAt  string             `json:"searchedAt"`
}

// search asks GET /api/search as a user ("" is the instance admin) and decodes the answer.
func (f *fleetFix) search(t *testing.T, as, query string) searchResp {
	t.Helper()
	rec := f.fleetGet(t, as, "/api/search"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("GET /api/search%s as %q = %d %s", query, as, rec.code, rec.body)
	}
	var r searchResp
	if err := json.Unmarshal(rec.body, &r); err != nil {
		t.Fatalf("decode %s: %v", rec.body, err)
	}
	return r
}

// hits is the results as "type env name", in order, for a comparison that reads.
func (r searchResp) hits() []string {
	out := make([]string, 0, len(r.Results))
	for _, h := range r.Results {
		out = append(out, h.Type+" "+h.EnvName+" "+h.Name)
	}
	return out
}

// containerIn makes a standalone host's container row.
func containerIn(id, name, image, project string) envs.ContainerRow {
	labels := map[string]string{}
	if project != "" {
		labels["com.docker.compose.project"] = project
		labels["com.docker.compose.service"] = "app"
	}
	return envs.ContainerRow{
		ContainerRow: dockerx.ContainerRow{ID: id, Name: name, Image: image, State: "running", Labels: labels},
		Node:         "host-1", NodeID: "n1",
	}
}

// standalone makes id a reporting standalone host holding these containers.
func (f *fleetFake) standalone(id string, containers ...envs.ContainerRow) {
	f.online(id)
	s := f.snap[id]
	s.agg.Containers = containers
	f.snap[id] = s
}

// managedStack stores a stack TierPlan manages in an environment.
func (f *fleetFix) managedStack(t *testing.T, envID, name string, target stacks.Target) stackops.Stack {
	t.Helper()
	st, _, err := stackops.Repo{Store: f.st, Vault: f.v}.CreateStack(testsupport.Context(t), envID, name, target, "ada",
		stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSearchFindsAcrossEnvironments(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	f.fake.swarmWith(alpha, []dockerx.ServiceRow{
		{ID: "s1", Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1", Stack: "webshop"},
		{ID: "s2", Name: "cache", Mode: "replicated", Running: 1, Desired: 1, Image: "redis:7", Stack: "other"},
	})
	managed := f.managedStack(t, alpha, "mywebstack", stacks.TargetSwarm)
	f.fake.standalone(beta,
		containerIn("aaaa1111", "web-1", "nginx:1", "site"),
		containerIn("bbbb2222", "db", "web:2", "site"),
		containerIn("cccc3333", "worker", "busybox", ""))
	f.fleet.Store = f.st
	f.rebuild(t)

	got := f.search(t, "", "?q=web")
	// Exact name first, then names that start with it, then names that hold it, then an image that does.
	// Equal matches go by environment name, then name.
	want := []string{
		"service Alpha web",
		"stack Alpha webshop",
		"container Beta web-1",
		"stack Alpha mywebstack",
		"container Beta db",
	}
	if !slices.Equal(got.hits(), want) {
		t.Fatalf("results = %v, want %v", got.hits(), want)
	}
	if c := got.Counts; c.Stack != 2 || c.Service != 1 || c.Container != 2 || c.Image != 0 || c.Network != 0 || c.All != 5 {
		t.Errorf("counts = %+v", c)
	}
	if got.Truncated || len(got.NotSearched) != 0 || got.SearchedAt != fleetNow.Format("2006-01-02T15:04:05Z") {
		t.Errorf("truncated %v, notSearched %+v, searchedAt %q", got.Truncated, got.NotSearched, got.SearchedAt)
	}

	byName := map[string]searchHit{}
	for _, h := range got.Results {
		byName[h.Type+" "+h.Name] = h
	}
	if h := byName["service web"]; h.EnvID != alpha || h.ID != "s1" || h.Image != "nginx:1" || h.Stack != "webshop" {
		t.Errorf("service = %+v", h)
	}
	if h := byName["container web-1"]; h.EnvID != beta || h.ID != "aaaa1111" || h.Image != "nginx:1" || h.Node != "host-1" || h.Stack != "site" {
		t.Errorf("container = %+v", h)
	}
	if h := byName["stack mywebstack"]; h.ID != managed.ID || !h.Managed {
		t.Errorf("managed stack = %+v", h)
	}
	if h := byName["stack webshop"]; h.ID != "" || h.Managed || h.Services != 1 {
		t.Errorf("discovered stack = %+v", h)
	}
}

// status asks GET /api/search and gives the status code.
func (f *fleetFix) searchStatus(t *testing.T, query string) int {
	t.Helper()
	return f.fleetGet(t, "", "/api/search"+query).code
}

func TestSearchValidation(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.standalone(alpha, containerIn("aaaa1111", "ab", "busybox", ""), containerIn("bbbb2222", "a", "busybox", ""))
	f.rebuild(t)

	for _, tc := range []struct{ name, query string }{
		{"missing", ""},
		{"empty", "?q="},
		{"one character", "?q=a"},
		{"one character between spaces", "?q=%20%20a%20%20"},
		{"only spaces", "?q=%20%20%20%20"},
		{"101 characters", "?q=" + strings.Repeat("a", 101)},
		{"unknown type", "?q=ab&type=volume"},
		{"unknown limit", "?q=ab&limit=all"},
	} {
		if code := f.searchStatus(t, tc.query); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: GET /api/search%s = %d, want 422", tc.name, tc.query, code)
		}
	}
	if code := f.searchStatus(t, "?q="+strings.Repeat("a", 100)); code != http.StatusOK {
		t.Errorf("100 characters = %d, want 200", code)
	}
	query := "?q=ab"
	for range 101 {
		query += "&env=x"
	}
	if code := f.searchStatus(t, query); code != http.StatusUnprocessableEntity {
		t.Errorf("101 environment IDs = %d, want 422", code)
	}

	// The query is trimmed and then searched: "  ab  " finds "ab", not the "a" that holds no "ab".
	got := f.search(t, "", "?q=%20%20AB%20%20")
	if want := []string{"container Alpha ab"}; !slices.Equal(got.hits(), want) {
		t.Errorf("a trimmed query finds %v, want %v", got.hits(), want)
	}
	if got.Counts.All != 1 {
		t.Errorf("counts = %+v", got.Counts)
	}
}

func TestSearchIDPrefix(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	alpha := f.id("Alpha")
	f.fake.standalone(alpha, containerIn("3f2a9c01deadbeef", "frontend", "busybox", ""), containerIn("77aa00bb11cc", "backend", "busybox", ""))
	s := f.fake.snap[alpha]
	s.agg.Images = []envs.ImageRow{
		{ImageRow: dockerx.ImageRow{ID: "sha256:3f2ab4cd0000", RepoTags: []string{"registry.example/app:2"}, Size: 1234}, Node: "host-1", NodeID: "n1"},
		{ImageRow: dockerx.ImageRow{ID: "sha256:9999aaaa0000", RepoTags: []string{"other:1"}}, Node: "host-1", NodeID: "n1"},
	}
	f.fake.snap[alpha] = s
	f.rebuild(t)

	got := f.search(t, "", "?q=3f2a")
	want := []string{"container Alpha frontend", "image Alpha registry.example/app:2"}
	if !slices.Equal(got.hits(), want) {
		t.Errorf("3f2a finds %v, want %v", got.hits(), want)
	}
	if got.Results[1].Size != 1234 || got.Results[1].ID != "sha256:3f2ab4cd0000" {
		t.Errorf("image = %+v", got.Results[1])
	}
	if upper := f.search(t, "", "?q=3F2A"); !slices.Equal(upper.hits(), want) {
		t.Errorf("3F2A finds %v, want %v", upper.hits(), want)
	}
	for _, q := range []string{"3f2", "zzzz", "ffff"} {
		if got := f.search(t, "", "?q="+q); len(got.Results) != 0 || got.Counts.All != 0 {
			t.Errorf("%q finds %v, want nothing: it is not four hexadecimal characters of an ID", q, got.hits())
		}
	}
}

func TestSearchCaps(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	var many []envs.ContainerRow
	for i := range 250 {
		many = append(many, containerIn(fmt.Sprintf("%064x", i), fmt.Sprintf("web-%03d", i), "busybox", ""))
	}
	f.fake.standalone(alpha, many...)
	f.fake.standalone(beta)
	s := f.fake.snap[alpha]
	for i := range 7 {
		s.agg.Images = append(s.agg.Images, envs.ImageRow{ImageRow: dockerx.ImageRow{ID: fmt.Sprintf("sha256:%064x", i), RepoTags: []string{fmt.Sprintf("web:%d", i)}}})
	}
	f.fake.snap[alpha] = s
	f.rebuild(t)

	got := f.search(t, "", "?q=web")
	if len(got.Results) != 200 || !got.Truncated || got.Counts.Container != 250 || got.Counts.Image != 7 || got.Counts.All != 257 {
		t.Errorf("page: %d results, truncated %v, counts %+v, want 200 results, truncated, 250 containers, 7 images", len(got.Results), got.Truncated, got.Counts)
	}
	// The exact matches (web:N images, whose repository is web) come first, so the cap cuts the weakest.
	if first := got.Results[0]; first.Type != "image" || first.Name != "web:0" {
		t.Errorf("first result = %+v, want the image web:0", first)
	}

	got = f.search(t, "", "?q=web&limit=palette")
	perType := map[string]int{}
	for _, h := range got.Results {
		perType[h.Type]++
	}
	if perType["container"] != 5 || perType["image"] != 5 || len(got.Results) != 10 || !got.Truncated || got.Counts.Container != 250 || got.Counts.Image != 7 || got.Counts.All != 257 {
		t.Errorf("palette: per type %v, truncated %v, counts %+v, want 5 of each and the full counts", perType, got.Truncated, got.Counts)
	}

	got = f.search(t, "", "?q=web-24")
	if len(got.Results) != 10 || got.Truncated {
		t.Errorf("a query under the cap: %d results, truncated %v, want 10 and not truncated", len(got.Results), got.Truncated)
	}

	// A type filter searches that type alone.
	got = f.search(t, "", "?q=web&type=image")
	if got.Counts.Image != 7 || got.Counts.Container != 0 || got.Counts.All != 7 || len(got.Results) != 7 || got.Truncated {
		t.Errorf("type=image: %d results, counts %+v", len(got.Results), got.Counts)
	}
}

func TestSearchPartialResults(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma", "Delta", "Epsilon")
	alpha, beta, gamma, delta, epsilon := f.id("Alpha"), f.id("Beta"), f.id("Gamma"), f.id("Delta"), f.id("Epsilon")
	f.fake.standalone(alpha, containerIn("aaaa1111", "web-a", "busybox", ""))
	f.fake.down(beta, envs.StateOffline)
	f.fake.standalone(gamma, containerIn("cccc3333", "web-g", "busybox", ""))
	// Delta answers, but one of its nodes does not, so what that node runs is missing.
	f.fake.standalone(delta, containerIn("dddd4444", "web-d", "busybox", ""))
	ds := f.fake.snap[delta]
	ds.agg.NotReporting = []string{"node-2", "node-3"}
	f.fake.snap[delta] = ds
	// Epsilon is a swarm that has lost its quorum, so its services and stacks cannot be searched.
	f.fake.online(epsilon)
	f.fake.swarm[epsilon] = true
	f.fake.state[epsilon] = envs.ClusterState{NoQuorum: true, FetchedAt: fleetNow}
	// Gamma never answers: the search gives up on it. Long enough for the others to answer on a loaded runner.
	f.fleet.SearchTimeout = 2 * time.Second
	f.fleet.Aggregator = &blockOn{id: gamma, fleetFake: f.fake, hang: testsupport.Context(t)}
	f.rebuild(t)

	got := f.search(t, "", "?q=web")
	if want := []string{"container Alpha web-a", "container Delta web-d"}; !slices.Equal(got.hits(), want) {
		t.Errorf("results = %v, want %v", got.hits(), want)
	}
	want := []fleetNotIncluded{
		{EnvID: beta, EnvName: "Beta", Reason: "offline"},
		{EnvID: delta, EnvName: "Delta", Reason: "nodes_not_reporting", Nodes: 2},
		{EnvID: epsilon, EnvName: "Epsilon", Reason: "not_reporting"},
		{EnvID: gamma, EnvName: "Gamma", Reason: "not_reporting"},
	}
	if !slices.Equal(got.NotSearched, want) {
		t.Errorf("notSearched = %+v, want %+v", got.NotSearched, want)
	}

	// Looking for containers alone does not need the cluster, so Epsilon's lost quorum does not stop it.
	if got := f.search(t, "", "?q=web&type=container&env="+epsilon); len(got.NotSearched) != 0 {
		t.Errorf("a container search of a swarm without quorum names %+v, want nothing", got.NotSearched)
	}
}

// grantsApart is a checker that holds the permissions apart: a Viewer may read everything but images, and
// the shell-less roles of the default registry cannot do that (all five are Viewer permissions).
func (f *fleetFix) grantsApart(hidden ...rbac.Perm) {
	reg := rbac.DefaultRegistry()
	for i := range reg {
		if slices.Contains(hidden, reg[i].Name) {
			reg[i].MinRole = rbac.RoleOperator
		}
	}
	f.fleet.Checker = rbac.NewChecker(f.st, rbac.WithRegistry(reg))
}

func TestSearchPermissionFilter(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	for _, id := range []string{alpha, beta} {
		f.fake.standalone(id, containerIn("aaaa1111", "web-1", "busybox", "webshop"))
		s := f.fake.snap[id]
		s.agg.Images = []envs.ImageRow{{ImageRow: dockerx.ImageRow{ID: "sha256:abcd0000", RepoTags: []string{"web:1"}}}}
		s.agg.Networks = []envs.NetworkRow{{NetworkRow: dockerx.NetworkRow{ID: "n1", Name: "webnet", Driver: "bridge", Subnets: []string{"172.20.0.0/16"}}}}
		f.fake.snap[id] = s
	}
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.addEnvUser(t, "operator", alpha, "operator")
	f.fleet.Store = f.st
	f.managedStack(t, alpha, "webstack", stacks.TargetStandalone)
	f.managedStack(t, beta, "webstack", stacks.TargetStandalone)
	f.grantsApart(rbac.PermImagesRead)
	f.rebuild(t)

	// An Operator of Alpha reads images there. Nothing of Beta, which they may not read.
	op := f.search(t, "operator", "?q=web")
	if want := []string{"image Alpha web:1", "network Alpha webnet", "container Alpha web-1", "stack Alpha webstack", "stack Alpha webshop"}; !sameSet(op.hits(), want) {
		t.Errorf("an Operator of Alpha finds %v, want %v", op.hits(), want)
	}
	if op.Counts.Image != 1 || op.Counts.All != 5 || len(op.NotSearched) != 0 {
		t.Errorf("an Operator's counts = %+v, notSearched %v", op.Counts, op.NotSearched)
	}

	// A Viewer of Alpha reads containers there but not images: no image result, and no image in the count.
	v := f.search(t, "viewer", "?q=web")
	if want := []string{"network Alpha webnet", "container Alpha web-1", "stack Alpha webstack", "stack Alpha webshop"}; !sameSet(v.hits(), want) {
		t.Errorf("a Viewer of Alpha finds %v, want %v", v.hits(), want)
	}
	if v.Counts.Image != 0 || v.Counts.Container != 1 || v.Counts.All != 4 {
		t.Errorf("a Viewer's counts = %+v, want no image", v.Counts)
	}
	if got := f.search(t, "viewer", "?q=web&type=image"); len(got.Results) != 0 || got.Counts.All != 0 {
		t.Errorf("a Viewer asking for images alone finds %v %+v", got.hits(), got.Counts)
	}

	// The instance admin reads everything, everywhere: both environments, images included.
	admin := f.search(t, "", "?q=web")
	if admin.Counts.Image != 2 || admin.Counts.Container != 2 || admin.Counts.All != 10 {
		t.Errorf("the admin's counts = %+v", admin.Counts)
	}
}

func TestSearchOnlyReadsWhereATypeMayBeSearched(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta")
	alpha, beta := f.id("Alpha"), f.id("Beta")
	for _, id := range []string{alpha, beta} {
		f.fake.standalone(id, containerIn("aaaa1111", "web-1", "busybox", ""))
	}
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.grantsApart(rbac.PermImagesRead)
	f.rebuild(t)

	// A Viewer may search containers but not images: asking for images alone reads nothing.
	if got := f.search(t, "viewer", "?q=web&type=image"); len(got.Results) != 0 || len(got.NotSearched) != 0 {
		t.Errorf("results %v, notSearched %v", got.hits(), got.NotSearched)
	}
	if asked := f.fake.asked(); len(asked) != 0 {
		t.Errorf("environments read = %v, want none: nothing the Viewer asked for may be searched", asked)
	}
}

func TestSearchUnreadableEnvironmentNeverNamed(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha", "Beta", "Gamma")
	alpha, beta, gamma := f.id("Alpha"), f.id("Beta"), f.id("Gamma")
	f.fake.standalone(alpha, containerIn("aaaa1111", "web-a", "busybox", ""))
	f.fake.standalone(beta, containerIn("bbbb2222", "web-secret", "busybox", ""))
	f.fake.down(gamma, envs.StateOffline)
	f.addEnvUser(t, "viewer", alpha, "viewer")
	f.addUser(t, "nobody", false)
	f.rebuild(t)

	got := f.search(t, "viewer", "?q=web")
	if want := []string{"container Alpha web-a"}; !slices.Equal(got.hits(), want) || got.Counts.All != 1 || len(got.NotSearched) != 0 {
		t.Errorf("a Viewer of Alpha: results %v, counts %+v, notSearched %+v", got.hits(), got.Counts, got.NotSearched)
	}
	// Naming the environments in env changes nothing: an ID the caller may not read is ignored.
	got = f.search(t, "viewer", "?q=web&env="+beta+"&env="+gamma)
	if len(got.Results) != 0 || got.Counts.All != 0 || len(got.NotSearched) != 0 {
		t.Errorf("a Viewer of Alpha asking for Beta and Gamma: results %v, counts %+v, notSearched %+v", got.hits(), got.Counts, got.NotSearched)
	}
	got = f.search(t, "nobody", "?q=web")
	if len(got.Results) != 0 || got.Counts.All != 0 || len(got.NotSearched) != 0 {
		t.Errorf("a user with no grant: results %v, counts %+v, notSearched %+v", got.hits(), got.Counts, got.NotSearched)
	}
	if asked := f.fake.asked(); !slices.Equal(asked, []string{alpha}) {
		t.Errorf("environments read = %v, want only Alpha: nothing of the others may be read", asked)
	}
}

func TestSearchReturnsNamesAsText(t *testing.T) {
	t.Parallel()
	f := newFleetFix(t, "Alpha")
	const name = "<img src=x onerror=alert(1)>web"
	f.fake.standalone(f.id("Alpha"), containerIn("aaaa1111", name, "busybox", ""))
	f.rebuild(t)

	rec := f.fleetGet(t, "", "/api/search?q=web")
	var raw struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.body, &raw); err != nil || len(raw.Results) != 1 || raw.Results[0].Name != name {
		t.Errorf("results = %s (%v), want the name unchanged", rec.body, err)
	}
}

// sameSet says whether a and b hold the same strings, whatever their order.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
