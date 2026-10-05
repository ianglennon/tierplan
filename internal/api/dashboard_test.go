// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The environment summary and the four read-only lists (plan 18, ENV-07, PLAT-06,
// D-24, D-25, D-27, D-48, D-55, D-56, D-58, Pitfall 22). Docker is a FakeDaemon
// on a real unix socket behind the real monitor and snapshot cache.

const selfID = "0123456789ab" // the self container ID newEnvFix gives the service

type summaryBody struct {
	Environment struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	} `json:"environment"`
	Engine struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		APIVersion    string `json:"apiVersion"`
		OS            string `json:"os"`
		Arch          string `json:"arch"`
		StorageDriver string `json:"storageDriver"`
		NCPU          int    `json:"ncpu"`
		MemTotal      int64  `json:"memTotal"`
	} `json:"engine"`
	Counts struct {
		ContainersRunning int  `json:"containersRunning"`
		ContainersStopped int  `json:"containersStopped"`
		Containers        int  `json:"containers"`
		Images            int  `json:"images"`
		Volumes           int  `json:"volumes"`
		Networks          int  `json:"networks"`
		Nodes             *int `json:"nodes"`
		Services          *int `json:"services"`
	} `json:"counts"`
	IsWorker        bool   `json:"isWorker"`
	SelfContainerID string `json:"selfContainerId"`
	FetchedAt       string `json:"fetchedAt"`
}

type listBody[T any] struct {
	Items     []T  `json:"items"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

type containerItem struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	State   string   `json:"state"`
	Status  string   `json:"status"`
	Image   string   `json:"image"`
	Created string   `json:"created"`
	Ports   []string `json:"ports"`
	IsSelf  bool     `json:"isSelf"`
}

type imageItem struct {
	ID       string   `json:"id"`
	ShortID  string   `json:"shortId"`
	RepoTags []string `json:"repoTags"`
	Size     int64    `json:"size"`
	Created  string   `json:"created"`
}

type volumeItem struct {
	Name    string `json:"name"`
	Driver  string `json:"driver"`
	Created string `json:"created"`
}

type networkItem struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Driver  string   `json:"driver"`
	Scope   string   `json:"scope"`
	Subnets []string `json:"subnets"`
	Created string   `json:"created"`
}

// envProblem is the environment problem: the stable type plus what the
// full-page states of plan 19 show.
type envProblem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Since  string `json:"since"`
	Reason string `json:"reason"`
	Engine struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		APIVersion string `json:"apiVersion"`
	} `json:"engine"`
}

var dataPaths = []string{"summary", "containers", "images", "volumes", "networks"}

func dashEnv(t *testing.T, cfg dockertest.FakeConfig) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, cfg)
	return f, f.add(t)
}

func (f *envFix) get(t *testing.T, e envBody, what string) *envRec {
	t.Helper()
	return f.req(t, http.MethodGet, "/api/env/"+e.ID+"/"+what, "")
}

func decode[T any](t *testing.T, r *envRec) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return v
}

func richEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: selfID + "ffffffffffffffffffffffffffffffffffffffffffffffffffff", Names: []string{"/bayplan"}, Image: "bayplan:edge", State: "running", Status: "Up 1 hour", Created: 1700000000,
				Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 8080, Public: 8080, Type: "tcp"}}},
			{ID: "aaaaaaaaaaaa11111111111111111111111111111111111111111111111111", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours", Created: 1700000100},
			{ID: "bbbbbbbbbbbb22222222222222222222222222222222222222222222222222", Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago", Created: 1700000200},
		},
		Images: []dockertest.FakeImage{
			{ID: "sha256:0123456789abcdef0123456789abcdef", RepoTags: []string{"nginx:1.27"}, Size: 1234, Created: 1700000000},
			{ID: "sha256:fedcba9876543210fedcba9876543210", Size: 99, Created: 1700000500},
		},
		Volumes: []dockertest.FakeVolume{{Name: "data", Driver: "local", CreatedAt: "2026-03-04T05:06:07Z"}},
		Networks: []dockertest.FakeNetwork{
			{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}, Created: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
			{ID: "n2", Name: "host", Driver: "host", Scope: "local"},
		},
	}
}

func TestSummaryReturnsTheEngineCardAndCountsForAnOnlineEnvironment(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	rec := f.get(t, e, "summary")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s\n%s", rec.code, rec.body, f.logs)
	}
	s := decode[summaryBody](t, rec)
	if s.Environment.ID != e.ID || s.Environment.Name != "node-1" || s.Environment.Type != "standalone" || s.Environment.Status.State != "online" {
		t.Errorf("environment = %+v", s.Environment)
	}
	eng := s.Engine
	if eng.Name != "Docker Engine" || eng.Version != "29.1.0" || eng.APIVersion != "1.52" || eng.OS != "linux" || eng.Arch != "amd64" ||
		eng.StorageDriver != "overlayfs" || eng.NCPU != 4 || eng.MemTotal != 8<<30 {
		t.Errorf("engine = %+v", eng)
	}
	c := s.Counts
	if c.ContainersRunning != 2 || c.ContainersStopped != 1 || c.Containers != 3 || c.Images != 2 || c.Volumes != 1 || c.Networks != 2 {
		t.Errorf("counts = %+v", c)
	}
	if c.Nodes != nil || c.Services != nil || s.IsWorker || s.SelfContainerID != selfID {
		t.Errorf("nodes %v services %v worker %v self %q", c.Nodes, c.Services, s.IsWorker, s.SelfContainerID)
	}
	if p, err := time.Parse(time.RFC3339, s.FetchedAt); err != nil || p.Location() != time.UTC {
		t.Errorf("fetchedAt %q is not RFC 3339 UTC (D-66): %v", s.FetchedAt, err)
	}
	if strings.Contains(string(rec.body), "null") {
		t.Errorf("the summary carries a null: %s", rec.body)
	}
}

func TestSummaryCountsEqualTheLengthOfTheListsTheirTilesLinkTo(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	s := decode[summaryBody](t, f.get(t, e, "summary"))
	if got := decode[listBody[containerItem]](t, f.get(t, e, "containers")).Total; got != s.Counts.Containers {
		t.Errorf("containers tile %d, list %d (Pitfall 22)", s.Counts.Containers, got)
	}
	if got := decode[listBody[imageItem]](t, f.get(t, e, "images")).Total; got != s.Counts.Images {
		t.Errorf("images tile %d, list %d", s.Counts.Images, got)
	}
	if got := decode[listBody[volumeItem]](t, f.get(t, e, "volumes")).Total; got != s.Counts.Volumes {
		t.Errorf("volumes tile %d, list %d", s.Counts.Volumes, got)
	}
	if got := decode[listBody[networkItem]](t, f.get(t, e, "networks")).Total; got != s.Counts.Networks {
		t.Errorf("networks tile %d, list %d", s.Counts.Networks, got)
	}
	// One snapshot served all five requests: the engine saw one container list.
	n := 0
	for _, p := range f.fake.Requests() {
		if strings.HasSuffix(p, "/containers/json") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the engine listed containers %d times for five requests, want 1 (short-TTL snapshot)", n)
	}
}

func TestSummaryOnASwarmManagerIncludesNodesAndServices(t *testing.T) {
	cfg := richEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.Nodes, cfg.Services = "active", true, 3, 7
	f, e := dashEnv(t, cfg)
	s := decode[summaryBody](t, f.get(t, e, "summary"))
	if s.Environment.Type != "swarm-manager" || s.IsWorker || s.Counts.Nodes == nil || *s.Counts.Nodes != 3 || s.Counts.Services == nil || *s.Counts.Services != 7 {
		t.Errorf("type %q worker %v nodes %v services %v, want a manager with 3 and 7", s.Environment.Type, s.IsWorker, s.Counts.Nodes, s.Counts.Services)
	}
}

func TestSummaryOnASwarmWorkerIsWorkerAndOmitsNodeAndServiceCounts(t *testing.T) {
	cfg := richEngine()
	cfg.SwarmState, cfg.SwarmControl = "active", false
	f, e := dashEnv(t, cfg)
	rec := f.get(t, e, "summary")
	s := decode[summaryBody](t, rec)
	if s.Environment.Type != "swarm-worker" || !s.IsWorker {
		t.Errorf("type %q isWorker %v, want swarm-worker and true (D-24)", s.Environment.Type, s.IsWorker)
	}
	if strings.Contains(string(rec.body), `"nodes"`) || strings.Contains(string(rec.body), `"services"`) {
		t.Errorf("a worker's summary mentions nodes or services: %s", rec.body)
	}
	for _, p := range f.fake.Requests() {
		if strings.HasSuffix(p, "/nodes") || strings.HasSuffix(p, "/services") {
			t.Errorf("a worker was asked for %s", p)
		}
	}
}

func TestListContainersCapsAtFiveHundredAndReportsTheTotal(t *testing.T) {
	var cs []dockertest.FakeContainer
	for i := range 600 {
		cs = append(cs, dockertest.FakeContainer{ID: fmt.Sprintf("c%04d", i), Names: []string{fmt.Sprintf("/c%04d", i)}, State: "running"})
	}
	f, e := dashEnv(t, dockertest.FakeConfig{Containers: cs})
	l := decode[listBody[containerItem]](t, f.get(t, e, "containers"))
	if len(l.Items) != 500 || l.Total != 600 || !l.Truncated {
		t.Errorf("items %d total %d truncated %v, want 500, 600, true (D-56)", len(l.Items), l.Total, l.Truncated)
	}
	if l.Items[0].Name != "c0000" || l.Items[499].Name != "c0499" {
		t.Errorf("the cap kept %s..%s, want the first 500 in order", l.Items[0].Name, l.Items[499].Name)
	}
	if s := decode[summaryBody](t, f.get(t, e, "summary")); s.Counts.Containers != 600 {
		t.Errorf("the tile counts %d, want all 600, not the cap", s.Counts.Containers)
	}
}

func TestListContainersWithinTheCapIsNotTruncated(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	l := decode[listBody[containerItem]](t, f.get(t, e, "containers"))
	if len(l.Items) != 3 || l.Total != 3 || l.Truncated {
		t.Errorf("items %d total %d truncated %v", len(l.Items), l.Total, l.Truncated)
	}
}

func TestListContainersBadgesOnlyBayplansOwnContainer(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	l := decode[listBody[containerItem]](t, f.get(t, e, "containers"))
	var selfNames []string
	for _, c := range l.Items {
		if c.IsSelf {
			selfNames = append(selfNames, c.Name)
		}
	}
	if !slices.Equal(selfNames, []string{"bayplan"}) {
		t.Errorf("isSelf is true for %v, want only bayplan (D-27)", selfNames)
	}
}

func TestListsCarryTheDocumentedColumns(t *testing.T) {
	f, e := dashEnv(t, richEngine())

	cs := decode[listBody[containerItem]](t, f.get(t, e, "containers")).Items
	web := cs[slices.IndexFunc(cs, func(c containerItem) bool { return c.Name == "web" })]
	if web.State != "running" || web.Status != "Up 2 hours" || web.Image != "nginx:1.27" || web.Created != "2023-11-14T22:15:00Z" || web.ID == "" {
		t.Errorf("web = %+v", web)
	}
	if bay := cs[slices.IndexFunc(cs, func(c containerItem) bool { return c.Name == "bayplan" })]; !slices.Equal(bay.Ports, []string{"0.0.0.0:8080->8080/tcp"}) {
		t.Errorf("bayplan ports = %q", bay.Ports)
	}

	is := decode[listBody[imageItem]](t, f.get(t, e, "images")).Items
	if len(is) != 2 || !slices.Equal(is[0].RepoTags, []string{"<none>"}) || is[0].ShortID != "fedcba987654" || is[0].Size != 99 ||
		!slices.Equal(is[1].RepoTags, []string{"nginx:1.27"}) || is[1].ShortID != "0123456789ab" || is[1].ID != "sha256:0123456789abcdef0123456789abcdef" {
		t.Errorf("images = %+v", is)
	}

	vs := decode[listBody[volumeItem]](t, f.get(t, e, "volumes")).Items
	if len(vs) != 1 || vs[0] != (volumeItem{Name: "data", Driver: "local", Created: "2026-03-04T05:06:07Z"}) {
		t.Errorf("volumes = %+v", vs)
	}

	ns := decode[listBody[networkItem]](t, f.get(t, e, "networks")).Items
	if len(ns) != 2 || ns[0].Name != "bridge" || !slices.Equal(ns[0].Subnets, []string{"172.17.0.0/16"}) || ns[0].Scope != "local" || ns[0].Created != "2026-01-02T03:04:05Z" {
		t.Errorf("networks = %+v", ns)
	}
	if ns[1].Name != "host" || ns[1].Subnets == nil || len(ns[1].Subnets) != 0 {
		t.Errorf("a network with no subnet has %#v, want an empty array", ns[1].Subnets)
	}
}

func TestListsNeverCarryLabelsMountsOrVariables(t *testing.T) {
	cfg := richEngine()
	cfg.Containers[1].Labels = map[string]string{"app.note": "label-9f3"}
	cfg.Containers[1].Mounts = []string{"/srv/mount-9f3"}
	cfg.Images[0].Labels = map[string]string{"app.build": "image-9f3"}
	cfg.Volumes[0].Labels = map[string]string{"app.owner": "volume-9f3"}
	f, e := dashEnv(t, cfg)
	for _, what := range dataPaths {
		rec := f.get(t, e, what)
		if rec.code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, rec.code, rec.body)
		}
		if strings.Contains(string(rec.body), "9f3") {
			t.Errorf("%s leaked a label, mount or variable (T-01-77): %s", what, rec.body)
		}
		for _, banned := range []string{`"labels"`, `"mounts"`, `"env"`, `"Labels"`, `"Mounts"`, `"Env"`} {
			if strings.Contains(string(rec.body), banned) {
				t.Errorf("%s has a %s field: %s", what, banned, rec.body)
			}
		}
	}
}

func TestListsOfAnEmptyEngineAreEmptyArraysAndZeroCountsNeverNull(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{})
	for _, what := range []string{"containers", "images", "volumes", "networks"} {
		rec := f.get(t, e, what)
		if rec.code != http.StatusOK || !strings.Contains(string(rec.body), `"items":[]`) || !strings.Contains(string(rec.body), `"total":0`) || strings.Contains(string(rec.body), "null") {
			t.Errorf("%s of an empty engine = %d %s", what, rec.code, rec.body)
		}
	}
	rec := f.get(t, e, "summary")
	s := decode[summaryBody](t, rec)
	if c := s.Counts; c.Containers+c.ContainersRunning+c.ContainersStopped+c.Images+c.Volumes+c.Networks != 0 || strings.Contains(string(rec.body), "null") {
		t.Errorf("summary of an empty engine = %s", rec.body)
	}
}

func TestListsReturnEqualSortKeysInAStableOrder(t *testing.T) {
	cfg := dockertest.FakeConfig{
		Containers: []dockertest.FakeContainer{
			{ID: "cz", Names: []string{"/web"}, State: "running"},
			{ID: "ca", Names: []string{"/web"}, State: "running"},
		},
		Images:   []dockertest.FakeImage{{ID: "sha256:2", Created: 5}, {ID: "sha256:1", Created: 5}},
		Networks: []dockertest.FakeNetwork{{ID: "n2", Name: "x"}, {ID: "n1", Name: "x"}},
	}
	f, e := dashEnv(t, cfg)
	var ids []string
	for _, c := range decode[listBody[containerItem]](t, f.get(t, e, "containers")).Items {
		ids = append(ids, c.ID)
	}
	if !slices.Equal(ids, []string{"ca", "cz"}) {
		t.Errorf("containers = %v, want name then ID", ids)
	}
	ids = nil
	for _, i := range decode[listBody[imageItem]](t, f.get(t, e, "images")).Items {
		ids = append(ids, i.ID)
	}
	if !slices.Equal(ids, []string{"sha256:1", "sha256:2"}) {
		t.Errorf("images = %v, want newest first then ID", ids)
	}
	ids = nil
	for _, n := range decode[listBody[networkItem]](t, f.get(t, e, "networks")).Items {
		ids = append(ids, n.ID)
	}
	if !slices.Equal(ids, []string{"n1", "n2"}) {
		t.Errorf("networks = %v, want name then ID", ids)
	}
}

func TestListOfAnOfflineEnvironmentIs503WithSinceAndReason(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	f.fake.Stop()
	f.mon.Kick(e.ID)
	waitFor(t, "the monitor to see the engine offline", func() bool { return f.mon.Status(e.ID).State == envs.StateOffline })
	for _, what := range dataPaths {
		rec := f.get(t, e, what)
		if rec.code != http.StatusServiceUnavailable || rec.ptyp != "environment_offline" {
			t.Errorf("%s: status %d type %q, want 503 environment_offline: %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		p := decode[envProblem](t, rec)
		if p.Reason != "unreachable" {
			t.Errorf("%s: reason %q, want unreachable", what, p.Reason)
		}
		if ts, err := time.Parse(time.RFC3339, p.Since); err != nil || ts.Location() != time.UTC {
			t.Errorf("%s: since %q is not RFC 3339 UTC: %v", what, p.Since, err)
		}
	}
}

func TestListOfAnUnsupportedEnvironmentIs409WithTheEngine(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24", Hostname: "old"})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local","allowUnsupported":true}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.code, rec.body)
	}
	e := rec.env(t)
	for _, what := range dataPaths {
		rec := f.get(t, e, what)
		if rec.code != http.StatusConflict || rec.ptyp != "environment_unsupported" {
			t.Errorf("%s: status %d type %q, want 409 environment_unsupported (D-25): %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		p := decode[envProblem](t, rec)
		if p.Reason != "api_too_old" || p.Engine.Name != "Docker Engine" || p.Engine.Version != "20.10.24" || p.Engine.APIVersion != "1.41" {
			t.Errorf("%s: problem = %+v", what, p)
		}
	}
	for _, p := range f.fake.Requests() {
		if strings.HasSuffix(p, "/containers/json") {
			t.Error("an unsupported engine was asked to list its containers")
		}
	}
}

func TestListOfAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, richEngine())
	for _, what := range dataPaths {
		rec := f.req(t, http.MethodGet, "/api/env/nope/"+what, "")
		if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
			t.Errorf("%s: %d %q, want 404 environment_not_found", what, rec.code, rec.ptyp)
		}
	}
}

func TestListEachRequiresItsOwnPermissionOnTheEnvironment(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	grants := map[string][]string{
		"summaryonly": {"environments.read"},
		"imagesonly":  {"environments.read", "images.read"},
		"allreads":    {"environments.read", "containers.read", "images.read", "volumes.read", "networks.read"},
	}
	n := 0
	for user, perms := range grants {
		n++
		role := "role-" + user
		if _, err := f.st.Write.Exec(`INSERT INTO roles (id, name, builtin) VALUES (?, ?, 0)`, role, user); err != nil {
			t.Fatal(err)
		}
		for _, p := range perms {
			if _, err := f.st.Write.Exec(`INSERT INTO role_permissions (role_id, permission) VALUES (?, ?)`, role, p); err != nil {
				t.Fatal(err)
			}
		}
		f.addUser(t, user, false)
		if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
			VALUES (?, 'user', ?, ?, ?, 0)`, fmt.Sprintf("ra-%d", n), "id-"+user, role, e.ID); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]map[string]int{
		"summaryonly": {"summary": 200, "containers": 403, "images": 403, "volumes": 403, "networks": 403},
		"imagesonly":  {"summary": 200, "containers": 403, "images": 200, "volumes": 403, "networks": 403},
		"allreads":    {"summary": 200, "containers": 200, "images": 200, "volumes": 200, "networks": 200},
	}
	for user, byPath := range want {
		cookie, _ := f.signIn(t, user, false)
		for what, code := range byPath {
			rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/" + what, cookie: cookie})
			if rec.Code != code {
				t.Errorf("%s GET %s = %d, want %d: %s", user, what, rec.Code, code, rec.Body)
			}
		}
	}

	denied := f.audits(t, audit.ActionPermissionDenied)
	found := false
	for _, d := range denied {
		found = found || (d.ActorID == "id-summaryonly" && d.TargetID == "GET /api/env/{envId}/images" && d.Result == "denied")
	}
	if !found {
		t.Errorf("a denied read of images was not audited: %+v", denied)
	}

	// A caller with no session is refused before the environment is looked up.
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/summary"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous summary = %d, want 401", rec.Code)
	}
}

func TestListRoutesAreReadOnly(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	for _, what := range dataPaths {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/"+what, `{}`)
		if rec.code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", what, rec.code)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Each list documents its own 200 body in the OpenAPI contract. The generated
// TypeScript types come from it, so a list that borrowed another operation's
// schema would give the web app the wrong row type.
func TestEachListDocumentsItsOwn200Schema(t *testing.T) {
	r := httpx.New(httpx.Options{})
	api.RegisterDashboard(r, api.DashboardDeps{})
	doc := r.API().OpenAPI()
	for path, want := range map[string]string{
		"summary":    "EnvironmentSummaryBody",
		"containers": "ContainerListBody",
		"images":     "ImageListBody",
		"volumes":    "VolumeListBody",
		"networks":   "NetworkListBody",
	} {
		item := doc.Paths["/api/env/{envId}/"+path]
		if item == nil || item.Get == nil {
			t.Fatalf("%s: no GET operation in the contract", path)
		}
		ok := item.Get.Responses["200"]
		if ok == nil || ok.Content["application/json"] == nil {
			t.Fatalf("%s: no 200 application/json response", path)
		}
		if got := ok.Content["application/json"].Schema.Ref; !strings.HasSuffix(got, "/"+want) {
			t.Errorf("%s: 200 schema = %q, want %s", path, got, want)
		}
		for _, status := range []string{"409", "503"} {
			if item.Get.Responses[status] == nil {
				t.Errorf("%s: %s response is not documented", path, status)
			}
		}
	}
}

// An agent that refuses a read is not an offline environment: the page says what
// was refused (plan 02-07, D-29).
func TestAnAgentThatRefusesARequestIs502AgentPolicyDeniedNamingTheEndpoint(t *testing.T) {
	cfg := richEngine()
	cfg.PolicyDeny = true
	f, e := dashEnv(t, cfg)
	for _, what := range dataPaths {
		rec := f.get(t, e, what)
		if rec.code != http.StatusBadGateway || rec.ptyp != "agent_policy_denied" {
			t.Errorf("%s: status %d type %q, want 502 agent_policy_denied: %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		var p struct{ Detail string }
		if err := json.Unmarshal(rec.body, &p); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p.Detail, "The agent refused a request (GET /") || !strings.HasSuffix(p.Detail, ").") {
			t.Errorf("%s: detail %q does not name the endpoint", what, p.Detail)
		}
	}
}

// The dashboard, the lists and the events of an agent environment, read through
// its agent's target (plan 02-11, ENV-01, ENV-06, D-33, D-40, P1 D-58).

// agentProblem is the typed problem of a data route: the Phase 1 members plus the
// environment's state.
type agentProblem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	State  string `json:"state"`
	Since  string `json:"since"`
	Reason string `json:"reason"`
	Engine struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		APIVersion string `json:"apiVersion"`
	} `json:"engine"`
}

func (f *agentFix) get(t *testing.T, envID, what string) *envRec {
	t.Helper()
	return f.req(t, http.MethodGet, "/api/env/"+envID+"/"+what, "")
}

func TestAgentEnvironmentSummaryAndListsAreReadThroughTheTunnel(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	f.connect(t, env, dockertest.FakeConfig{
		Hostname: "edge-1", Version: "28.4.0",
		Containers: []dockertest.FakeContainer{
			{ID: "a1", Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: "a2", Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago"},
		},
		Images:   []dockertest.FakeImage{{ID: "sha256:0123456789abcdef0123456789abcdef", RepoTags: []string{"nginx:1.27"}, Size: 1234}},
		Volumes:  []dockertest.FakeVolume{{Name: "data", Driver: "local"}},
		Networks: []dockertest.FakeNetwork{{ID: "n1", Name: "bridge", Driver: "bridge", Scope: "local"}},
	}, nil)
	f.whenState(t, env.ID, "online")

	var s summaryBody
	waitFor(t, "the summary", func() bool {
		rec := f.get(t, env.ID, "summary")
		if rec.code != http.StatusOK {
			return false
		}
		s = decode[summaryBody](t, rec)
		return true
	})
	if s.Environment.ID != env.ID || s.Environment.Name != "Remote" || s.Environment.Type != "standalone" || s.Environment.Status.State != "online" {
		t.Errorf("environment = %+v", s.Environment)
	}
	if s.Engine.Version != "28.4.0" || s.Counts.Containers != 2 || s.Counts.ContainersRunning != 1 || s.Counts.Images != 1 || s.Counts.Volumes != 1 || s.Counts.Networks != 1 {
		t.Errorf("engine %+v counts %+v, want what the agent's daemon reported", s.Engine, s.Counts)
	}
	containers := decode[listBody[containerItem]](t, f.get(t, env.ID, "containers"))
	if containers.Total != 2 || containers.Items[0].Name != "db" || containers.Items[1].Name != "web" {
		t.Errorf("containers = %+v", containers)
	}
	for what, total := range map[string]int{"images": 1, "volumes": 1, "networks": 1} {
		rec := f.get(t, env.ID, what)
		if rec.code != http.StatusOK {
			t.Errorf("%s: %d %s", what, rec.code, rec.body)
			continue
		}
		var l struct {
			Total int `json:"total"`
		}
		if err := json.Unmarshal(rec.body, &l); err != nil || l.Total != total {
			t.Errorf("%s: %s, want a total of %d", what, rec.body, total)
		}
	}
}

func TestPendingAgentEnvironmentDataRoutesAnswer503EnvironmentOfflineWithStatePending(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Waiting")
	for _, what := range dataPaths {
		rec := f.get(t, env.ID, what)
		if rec.code != http.StatusServiceUnavailable || rec.ptyp != "environment_offline" {
			t.Errorf("%s: %d %q, want 503 environment_offline: %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		if p := decode[agentProblem](t, rec); p.State != "pending" || p.Reason != "" || p.Since != "" {
			t.Errorf("%s: problem = %+v, want state pending with no reason and no since", what, p)
		}
	}
}

func TestRevokedAgentEnvironmentDataRoutesAnswer503WithStateRevokedItsReasonAndSince(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	f.whenState(t, env.ID, "online")
	f.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	f.srv.Events.WaitLen(t, 2)
	f.tclk.Advance(time.Minute)
	revokedAt := f.tclk.Now()
	f.srv.Exec(t, `UPDATE agents SET status = 'revoked', revoked_at = ? WHERE id = ?`, revokedAt.UnixNano(), a.id)

	for _, what := range dataPaths {
		rec := f.get(t, env.ID, what)
		if rec.code != http.StatusServiceUnavailable || rec.ptyp != "environment_offline" {
			t.Errorf("%s: %d %q, want 503 environment_offline: %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		if p := decode[agentProblem](t, rec); p.State != "revoked" || p.Reason != "revoked" || p.Since != utc(revokedAt) {
			t.Errorf("%s: problem = %+v, want revoked / revoked since %s", what, p, utc(revokedAt))
		}
	}
}

func TestOfflineAgentEnvironmentDataRoutesCarryTheReasonAndTheTimeTheContactWasLost(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	f.whenState(t, env.ID, "online")
	f.srv.Events.WaitLen(t, 1)
	_ = a.conn.WS.CloseNow()
	f.srv.Events.WaitLen(t, 2)
	droppedAt := f.tclk.Now()
	f.tclk.Advance(31 * time.Second)

	rec := f.get(t, env.ID, "summary")
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "environment_offline" {
		t.Fatalf("%d %q, want 503 environment_offline: %s", rec.code, rec.ptyp, rec.body)
	}
	if p := decode[agentProblem](t, rec); p.State != "offline" || p.Reason != "agent_offline" || p.Since != utc(droppedAt) {
		t.Fatalf("problem = %+v, want offline / agent_offline since %s", p, utc(droppedAt))
	}
}

func TestUnsupportedAgentEngineAnswers409EnvironmentUnsupportedWithTheEngineAndTheD40Reason(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Old")
	f.connect(t, env, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24", Hostname: "old"}, nil)
	f.whenState(t, env.ID, "unsupported")
	for _, what := range dataPaths {
		rec := f.get(t, env.ID, what)
		if rec.code != http.StatusConflict || rec.ptyp != "environment_unsupported" {
			t.Errorf("%s: %d %q, want 409 environment_unsupported: %s", what, rec.code, rec.ptyp, rec.body)
			continue
		}
		p := decode[agentProblem](t, rec)
		if p.State != "unsupported" || p.Reason != "unsupported_engine" || p.Engine.Version != "20.10.24" || p.Engine.APIVersion != "1.41" {
			t.Errorf("%s: problem = %+v", what, p)
		}
	}
}

func TestAgentEnvironmentEventsStreamDeliversAnEventPublishedOnTheAgentsDaemon(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	f.whenState(t, env.ID, "online")

	ts := httptest.NewServer(f.h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/env/"+env.ID+"/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream: %d", resp.StatusCode)
	}

	a.fake.PublishEvent(dockertest.FakeEvent{Type: "container", Action: "start", ActorID: "abc123", Attributes: map[string]string{"name": "web"}})
	sc := bufio.NewScanner(resp.Body)
	got := false
	for !got && sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") && strings.Contains(line, `"action":"start"`) && strings.Contains(line, `"name":"web"`) {
			got = true
		}
	}
	if !got {
		t.Fatalf("the event published on the agent's daemon never arrived (%v)", sc.Err())
	}

	// The polling fallback shows the same event.
	waitFor(t, "the recent list", func() bool {
		rec := f.get(t, env.ID, "events")
		return rec.code == http.StatusOK && strings.Contains(string(rec.body), `"name":"web"`)
	})
}

// The summary and lists of a swarm: every node, each row with its node, the
// nodes that did not answer by name, and a node filter (plan 02-21, ENV-02, ENV-06,
// D-46, T-02-73, T-02-74).

type nodeCountsItem struct {
	Node       string `json:"node"`
	Containers int    `json:"containers"`
	Images     int    `json:"images"`
	Volumes    int    `json:"volumes"`
	Networks   int    `json:"networks"`
}

type swarmSummary struct {
	summaryBody
	NotReporting *[]string        `json:"notReporting"`
	ByNode       []nodeCountsItem `json:"byNode"`
}

type nodeListBody[T any] struct {
	Items        []T       `json:"items"`
	Total        int       `json:"total"`
	Truncated    bool      `json:"truncated"`
	Nodes        *[]string `json:"nodes"`
	NotReporting *[]string `json:"notReporting"`
}

type nodeContainer struct {
	Name string `json:"name"`
	Node string `json:"node"`
}

type nodeNetwork struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Node  string `json:"node"`
}

type nodeNamed struct {
	Node string `json:"node"`
}

// swarmFix is a swarm of two managers and a worker whose worker holds 501 containers,
// so a filter and the 500-row cap can be told apart.
type swarmFix struct {
	*agentFix
	env       tunneltest.Environment
	m1, m2, w *agentConn
}

func newSwarmFix(t *testing.T) *swarmFix {
	t.Helper()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	mgr := func(i int) (dockertest.FakeConfig, *wire.Facts) {
		name := fmt.Sprintf("mgr-%d", i)
		return dockertest.FakeConfig{
			Hostname: name, SwarmState: "active", SwarmControl: true, Nodes: 3, Services: 2,
			Containers: []dockertest.FakeContainer{{ID: fmt.Sprintf("m%d", i), Names: []string{"/web"}, State: "running"}},
			Images:     []dockertest.FakeImage{{ID: "sha256:" + strings.Repeat("a", 64), RepoTags: []string{"nginx:1"}, Size: 5}},
			Volumes:    []dockertest.FakeVolume{{Name: "data", Driver: "local"}},
			Networks: []dockertest.FakeNetwork{
				{ID: "ovl", Name: "ovl", Driver: "overlay", Scope: "swarm"},
				{ID: fmt.Sprintf("br%d", i), Name: "bridge", Driver: "bridge", Scope: "local"},
			},
		}, &wire.Facts{
			EngineID: "ENG:" + name, Hostname: name, EngineReachable: true,
			Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-" + name, ClusterID: "cluster-1", Control: true},
		}
	}
	s := &swarmFix{agentFix: f, env: env}
	cfg, facts := mgr(1)
	s.m1 = f.connect(t, env, cfg, facts)
	cfg, facts = mgr(2)
	s.m2 = f.connect(t, env, cfg, facts)
	var many []dockertest.FakeContainer
	for i := range 501 {
		many = append(many, dockertest.FakeContainer{ID: fmt.Sprintf("w%03d", i), Names: []string{fmt.Sprintf("/job-%03d", i)}, State: "running"})
	}
	s.w = f.connect(t, env, dockertest.FakeConfig{
		Hostname: "wrk-1", SwarmState: "active", Containers: many,
		Networks: []dockertest.FakeNetwork{{ID: "ovl", Name: "ovl", Driver: "overlay", Scope: "swarm"}, {ID: "brw", Name: "bridge", Driver: "bridge", Scope: "local"}},
	}, &wire.Facts{EngineID: "ENG:wrk-1", Hostname: "wrk-1", EngineReachable: true, Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-wrk-1"}})
	f.whenState(t, env.ID, "online")
	return s
}

func TestSwarmSummaryHasClusterTotalsAPerNodeBreakdownAndNobodyMissing(t *testing.T) {
	s := newSwarmFix(t)
	var sum swarmSummary
	waitFor(t, "the summary", func() bool {
		rec := s.get(t, s.env.ID, "summary")
		if rec.code != http.StatusOK {
			return false
		}
		sum = decode[swarmSummary](t, rec)
		return true
	})
	if sum.Counts.Containers != 503 || sum.Counts.Images != 2 || sum.Counts.Volumes != 2 {
		t.Errorf("counts = %+v, want the totals over the three nodes", sum.Counts)
	}
	// One swarm network from the manager in use, and a bridge on each of the nodes.
	if sum.Counts.Networks != 4 {
		t.Errorf("networks = %d, want the overlay once and three bridges", sum.Counts.Networks)
	}
	if sum.Counts.Nodes == nil || *sum.Counts.Nodes != 3 || sum.Counts.Services == nil || *sum.Counts.Services != 2 {
		t.Errorf("nodes/services = %v/%v, want the manager's 3 and 2", sum.Counts.Nodes, sum.Counts.Services)
	}
	if sum.NotReporting == nil || len(*sum.NotReporting) != 0 {
		t.Errorf("notReporting = %v, want an empty list (not null) when every node answered", sum.NotReporting)
	}
	var names []string
	for _, n := range sum.ByNode {
		names = append(names, n.Node)
	}
	if !slices.Equal(names, []string{"mgr-1", "mgr-2", "wrk-1"}) {
		t.Fatalf("byNode = %+v, want the three nodes by name", sum.ByNode)
	}
	if w := sum.ByNode[2]; w.Containers != 501 || w.Networks != 1 {
		t.Errorf("wrk-1 = %+v, want its 501 containers and one local network", w)
	}
}

func TestSwarmSummaryNamesTheNodeThatDidNotAnswer(t *testing.T) {
	s := newSwarmFix(t)
	s.srv.Hub.Kick(s.w.id, tunnel.DisconnectClosed)
	waitFor(t, "the worker to be gone", func() bool {
		for _, v := range s.srv.Hub.Views(s.env.ID) {
			if v.AgentID == s.w.id && v.Connected {
				return false
			}
		}
		return true
	})
	rec := s.get(t, s.env.ID, "summary")
	if rec.code != http.StatusOK {
		t.Fatalf("summary: %d %s", rec.code, rec.body)
	}
	sum := decode[swarmSummary](t, rec)
	if sum.NotReporting == nil || !slices.Equal(*sum.NotReporting, []string{"wrk-1"}) {
		t.Errorf("notReporting = %v, want the missing node by name", sum.NotReporting)
	}
	if sum.Counts.Containers != 2 || len(sum.ByNode) != 2 {
		t.Errorf("counts = %+v byNode = %+v, want only the two managers' totals, not a partial figure presented as the whole", sum.Counts, sum.ByNode)
	}
}

func TestSwarmListsCarryEachRowsNodeAndTheNodeNamesAvailableForTheFilter(t *testing.T) {
	s := newSwarmFix(t)
	nets := decode[nodeListBody[nodeNetwork]](t, s.get(t, s.env.ID, "networks"))
	var rows []string
	for _, n := range nets.Items {
		rows = append(rows, n.Node+"/"+n.Name+"/"+n.Scope)
	}
	want := []string{"mgr-1/bridge/local", "mgr-2/bridge/local", "wrk-1/bridge/local", "Cluster/ovl/swarm"}
	slices.Sort(rows)
	slices.Sort(want)
	if !slices.Equal(rows, want) {
		t.Errorf("networks = %v, want %v", rows, want)
	}
	if nets.Nodes == nil || !slices.Equal(*nets.Nodes, []string{"Cluster", "mgr-1", "mgr-2", "wrk-1"}) {
		t.Errorf("networks nodes = %v, want the reporting nodes and Cluster for the filter", nets.Nodes)
	}
	for _, what := range []string{"images", "volumes"} {
		l := decode[nodeListBody[nodeNamed]](t, s.get(t, s.env.ID, what))
		if l.Total != 2 || len(l.Items) != 2 || l.Items[0].Node == "" {
			t.Errorf("%s = %+v, want one row per manager with its node", what, l)
		}
		if l.Nodes == nil || !slices.Equal(*l.Nodes, []string{"mgr-1", "mgr-2", "wrk-1"}) || l.NotReporting == nil || len(*l.NotReporting) != 0 {
			t.Errorf("%s nodes %v notReporting %v, want three nodes and nobody missing", what, l.Nodes, l.NotReporting)
		}
	}
}

func TestSwarmContainerListFiltersByNodeBeforeTheCapAndKeepsEveryNodeNameForTheFilter(t *testing.T) {
	s := newSwarmFix(t)
	all := decode[nodeListBody[nodeContainer]](t, s.get(t, s.env.ID, "containers"))
	if all.Total != 503 || !all.Truncated || len(all.Items) != 500 {
		t.Fatalf("unfiltered: total %d truncated %v items %d, want 503 / true / 500", all.Total, all.Truncated, len(all.Items))
	}
	w := decode[nodeListBody[nodeContainer]](t, s.get(t, s.env.ID, "containers?node=wrk-1"))
	if w.Total != 501 || !w.Truncated || len(w.Items) != 500 {
		t.Errorf("wrk-1: total %d truncated %v items %d, want the cap applied after the filter: 501 / true / 500", w.Total, w.Truncated, len(w.Items))
	}
	m := decode[nodeListBody[nodeContainer]](t, s.get(t, s.env.ID, "containers?node=mgr-2"))
	if m.Total != 1 || m.Truncated || len(m.Items) != 1 || m.Items[0].Node != "mgr-2" {
		t.Errorf("mgr-2 = %+v, want its one container", m)
	}
	if m.Nodes == nil || !slices.Equal(*m.Nodes, []string{"mgr-1", "mgr-2", "wrk-1"}) {
		t.Errorf("nodes = %v, want every reporting node even with a filter on", m.Nodes)
	}
	none := decode[nodeListBody[nodeContainer]](t, s.get(t, s.env.ID, "containers?node=nobody"))
	if none.Total != 0 || none.Items == nil || len(none.Items) != 0 {
		t.Errorf("an unknown node = %+v, want an empty list that is not null", none)
	}
	if rec := s.get(t, s.env.ID, "containers?node="+strings.Repeat("n", 129)); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a node of 129 characters: %d, want 422 (T-02-74)", rec.code)
	}
}

func TestStandaloneListsAndSummaryAreUnchangedApartFromTheOneNode(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Containers: []dockertest.FakeContainer{{ID: "a1", Names: []string{"/web"}, State: "running"}}}, nil)
	f.whenState(t, env.ID, "online")
	var sum swarmSummary
	waitFor(t, "the summary", func() bool {
		rec := f.get(t, env.ID, "summary")
		if rec.code != http.StatusOK {
			return false
		}
		sum = decode[swarmSummary](t, rec)
		return true
	})
	if len(sum.ByNode) != 1 || sum.ByNode[0].Node != "edge-1" || sum.ByNode[0].Containers != 1 || sum.NotReporting == nil || len(*sum.NotReporting) != 0 {
		t.Errorf("summary = %+v, want the one host and nobody missing", sum)
	}
	l := decode[nodeListBody[nodeContainer]](t, f.get(t, env.ID, "containers"))
	if l.Total != 1 || l.Items[0].Node != "edge-1" || l.Nodes == nil || !slices.Equal(*l.Nodes, []string{"edge-1"}) {
		t.Errorf("containers = %+v, want the row on edge-1", l)
	}
}

func TestLocalSocketListsNameTheEngineAsTheOnlyNode(t *testing.T) {
	f, e := dashEnv(t, richEngine())
	waitFor(t, "the summary", func() bool { return f.get(t, e, "summary").code == http.StatusOK })
	sum := decode[swarmSummary](t, f.get(t, e, "summary"))
	if len(sum.ByNode) != 1 || sum.ByNode[0].Node != "node-1" || sum.ByNode[0].Containers != 3 {
		t.Errorf("byNode = %+v, want the one engine", sum.ByNode)
	}
	l := decode[nodeListBody[nodeContainer]](t, f.get(t, e, "containers?node=node-1"))
	if l.Total != 3 || l.Items[0].Node != "node-1" {
		t.Errorf("containers = %+v, want all three on node-1", l)
	}
}
