// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The read-only page of a stack Bayplan did not make (plan 08-09, STK-12, D-09, D-10, D-47): found by its
// labels, read now, with nothing stored and nothing written to an engine.

type unmanagedAnswer struct {
	Name            string `json:"name"`
	Target          string `json:"target"`
	State           string `json:"state"`
	ServicesRunning int    `json:"servicesRunning"`
	ServicesTotal   int    `json:"servicesTotal"`
	Services        []struct {
		ID, Name, Short, State, Image string
		Running, Desired              int
		Ports                         []string
		Containers                    []struct{ ID, Name, State string }
	} `json:"services"`
	FoundBy struct {
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"foundBy"`
	Orphan    bool   `json:"orphan"`
	Portainer bool   `json:"portainer"`
	Owner     string `json:"owner"`
	Adoptable bool   `json:"adoptable"`
	Reason    string `json:"reason"`
}

// unmanagedPath is the path of a route of an unmanaged stack, after /api/env/{envId}/.
func unmanagedPath(name, tail string) string {
	return "unmanaged-stacks/" + url.PathEscape(name) + tail
}

// foreignSecretService is a service of a swarm namespace that holds values a Viewer must never read: an
// environment value, a command, an argument, a health check and a log option, and a label.
func foreignSecretService(namespace, short string, running, desired uint64, labels map[string]string) dockertest.FakeService {
	svc := foreignSwarmService(namespace, short, running, desired, labels)
	spec := sdSpec("nginx:1.27", desired)
	spec.Name = svc.Name
	spec.Labels = svc.Labels
	spec.Labels["note"] = "label-secret-value"
	svc.Spec = spec
	return svc
}

func TestUnmanagedStackDetail(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	newStack(t, admin, "web")
	reconfigureSwarm(t, f, e,
		foreignSecretService("legacy", "app", 2, 2, map[string]string{"io.portainer.accesscontrol.teams": "x", "bayplan.stack.id": "stack-old"}),
		foreignSwarmService("legacy", "db", 0, 1, nil),
		foreignSwarmService("other", "app", 1, 1, nil),
	)
	writesBefore := len(f.fake.Writes())

	rec := viewer.do(http.MethodGet, unmanagedPath("legacy", ""), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged stack: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[unmanagedAnswer](t, rec)
	if got.Name != "legacy" || got.Target != "swarm" || got.State != "degraded" || got.ServicesRunning != 1 || got.ServicesTotal != 2 {
		t.Errorf("stack = %+v, want legacy on a swarm, degraded, 1 of 2", got)
	}
	if got.FoundBy.Label != "com.docker.stack.namespace" || got.FoundBy.Value != "legacy" {
		t.Errorf("foundBy = %+v, want the namespace label and the name", got.FoundBy)
	}
	if len(got.Services) != 2 || got.Services[0].Short != "app" || got.Services[1].Short != "db" || got.Services[0].ID != "svc-legacy-app" {
		t.Errorf("services = %+v, want legacy's own two, app then db, and not other's", got.Services)
	}
	if !got.Orphan || !got.Portainer || got.Owner != "" || !got.Adoptable || got.Reason != "" {
		t.Errorf("flags = orphan %v portainer %v owner %q adoptable %v, want the orphan and Portainer notes of a plain adoptable stack", got.Orphan, got.Portainer, got.Owner, got.Adoptable)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	// D-47: no value of the foreign services, and no label ID, leaves the server.
	body := rec.Body.String()
	for what, secret := range sdSecrets {
		if strings.Contains(body, secret) {
			t.Errorf("the answer carries the %s value %q of a foreign service", what, secret)
		}
	}
	for _, leaked := range []string{"stack-old", "label-secret-value", "accesscontrol"} {
		if strings.Contains(body, leaked) {
			t.Errorf("the answer carries %q, a label of the foreign stack", leaked)
		}
	}
	var keys struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	for _, svc := range keys.Services {
		for k := range svc {
			switch strings.ToLower(k) {
			case "env", "environment", "command", "args", "labels", "logdriver", "healthcheck":
				t.Errorf("a service of the answer has the field %q", k)
			}
		}
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("the engine received %d writes while reading an unmanaged stack, want none", after-writesBefore)
	}
}

func TestUnmanagedStandaloneProjectPage(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	cfg.Containers = []dockertest.FakeContainer{
		{ID: "c1", Names: []string{"/shop-web-1"}, Image: "nginx:1.27", State: "running", Ports: []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 8080, Type: "tcp"}},
			Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web", "com.docker.compose.project.working_dir": "/data/compose/3"}},
		{ID: "c2", Names: []string{"/shop-db-1"}, Image: "postgres:17", State: "exited",
			Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "db"}},
		{ID: "c3", Names: []string{"/shop-run-1"}, Image: "nginx:1.27", State: "running",
			Labels: map[string]string{"com.docker.compose.project": "shop", "com.docker.compose.service": "web", "com.docker.compose.oneoff": "True"}},
		{ID: "c4", Names: []string{"/elsewhere-web-1"}, Image: "nginx:1.27", State: "running",
			Labels: map[string]string{"com.docker.compose.project": "elsewhere", "com.docker.compose.service": "web"}},
	}
	f.fake.Reconfigure(cfg)

	rec := viewer.do(http.MethodGet, unmanagedPath("shop", ""), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged stack: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[unmanagedAnswer](t, rec)
	if got.Target != "standalone" || got.State != "degraded" || got.ServicesRunning != 1 || got.ServicesTotal != 2 || !got.Portainer || got.Orphan {
		t.Errorf("stack = %+v, want the shop project, degraded, with the Portainer note", got)
	}
	if got.FoundBy.Label != "com.docker.compose.project" || got.FoundBy.Value != "shop" {
		t.Errorf("foundBy = %+v, want the project label", got.FoundBy)
	}
	if len(got.Services) != 2 || got.Services[0].Name != "db" || got.Services[1].Name != "web" || len(got.Services[1].Containers) != 1 {
		t.Errorf("services = %+v, want db and web, the one-off container left out", got.Services)
	}
}

func TestUnmanagedStackGoneOrManaged(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	web := newStack(t, admin, "web")
	reconfigureSwarm(t, f, e, foreignSwarmService("legacy", "app", 1, 1, nil), foreignSwarmService("web", "app", 1, 1, nil))

	rec := viewer.do(http.MethodGet, unmanagedPath("nothing-here", ""), "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
		t.Fatalf("a name nothing carries: %d %s, want 404 unmanaged_stack_not_found", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "managedStackId") {
		t.Errorf("a name nothing carries must not name a managed stack: %s", rec.Body)
	}

	rec = viewer.do(http.MethodGet, unmanagedPath("web", ""), "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
		t.Fatalf("a name a managed stack holds: %d %s, want 404 unmanaged_stack_not_found", rec.Code, rec.Body)
	}
	var p struct {
		Errors []struct{ Location, Message string } `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Errors) != 1 || p.Errors[0].Location != "managedStackId" || p.Errors[0].Message != web.ID {
		t.Errorf("errors = %+v, want managedStackId = %s so the page can offer Open stack", p.Errors, web.ID)
	}

	// The stack vanishes from the engine while its page is open.
	reconfigureSwarm(t, f, e, foreignSwarmService("web", "app", 1, 1, nil))
	rec = viewer.do(http.MethodGet, unmanagedPath("legacy", ""), "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
		t.Errorf("a stack removed outside Bayplan: %d %s, want 404 unmanaged_stack_not_found", rec.Code, rec.Body)
	}
}

func TestUnmanagedStackNameIsBounded(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")

	rec := viewer.do(http.MethodGet, unmanagedPath(strings.Repeat("a", 256), ""), "")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a name of 256 characters: %d %s, want 422", rec.Code, rec.Body)
	}
}

// foreignStackWithData is a swarm namespace, legacy, that holds a service with a labelled network, a labelled
// volume, a network and a volume it only uses, and a secret and a config, beside another namespace's objects.
func foreignStackWithData() dockertest.FakeConfig {
	ns := func(n string) map[string]string { return map[string]string{"com.docker.stack.namespace": n} }
	cfg := swarmManagerEngine(
		dockertest.FakeService{
			ID: "svc-legacy-app", Name: "legacy_app", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Labels: ns("legacy"),
			Networks: []string{"n-default", "shared"},
			Mounts: []dockertest.FakeServiceMount{
				{Type: "volume", Source: "legacy_data", Target: "/data"}, {Type: "volume", Source: "shared-vol", Target: "/s"},
			},
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-pw", Name: "legacy_pw", File: "pw"}},
			Configs: []dockertest.FakeServiceObject{{ID: "cfg-app", Name: "legacy_conf", File: "/etc/app.conf"}},
		},
		dockertest.FakeService{ID: "svc-blog-app", Name: "blog_app", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Labels: ns("blog")},
	)
	cfg.Networks = []dockertest.FakeNetwork{
		{ID: "n-default", Name: "legacy_default", Driver: "overlay", Scope: "swarm", Labels: ns("legacy")},
		{ID: "n-shared", Name: "shared", Driver: "overlay", Scope: "swarm"},
		{ID: "n-blog", Name: "blog_default", Driver: "overlay", Scope: "swarm", Labels: ns("blog")},
	}
	cfg.Volumes = []dockertest.FakeVolume{
		{Name: "legacy_data", Driver: "local", Labels: ns("legacy")}, {Name: "shared-vol", Driver: "local"},
		{Name: "blog_data", Driver: "local", Labels: ns("blog")},
	}
	cfg.Secrets = []dockertest.FakeSecret{{ID: "sec-pw", Name: "legacy_pw", Labels: ns("legacy")}}
	cfg.Configs = []dockertest.FakeConfigObject{{ID: "cfg-app", Name: "legacy_conf", Labels: ns("legacy")}}
	return cfg
}

func TestUnmanagedResourcesPreviewOwnership(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")
	f.fake.Reconfigure(foreignStackWithData())
	if err := f.clusters.Refresh(testsupport.Context(t), e.ID); err != nil {
		t.Fatal(err)
	}
	writesBefore := len(f.fake.Writes())

	rec := viewer.do(http.MethodGet, unmanagedPath("legacy", "/resources"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("unmanaged resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Tasks == nil || got.Containers != nil {
		t.Fatalf("a swarm stack answers the tasks card and not the containers card: %s", rec.Body)
	}
	type row struct{ name, owner string }
	var nets, vols, secrets, configs []row
	for _, n := range got.Networks.Items {
		nets = append(nets, row{n.Name, n.Ownership})
	}
	for _, v := range got.Volumes.Items {
		vols = append(vols, row{v.Name, v.Ownership})
	}
	for _, o := range got.Secrets.Items {
		secrets = append(secrets, row{o.Name, o.Ownership})
	}
	for _, o := range got.Configs.Items {
		configs = append(configs, row{o.Name, o.Ownership})
	}
	check := func(what string, got, want []row) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	check("networks", nets, []row{{"legacy_default", "stack"}, {"shared", "external"}})
	check("volumes", vols, []row{{"legacy_data", "stack"}, {"shared-vol", "external"}})
	check("secrets", secrets, []row{{"legacy_pw", "external"}})
	check("configs", configs, []row{{"legacy_conf", "external"}})
	if got.Tasks.Error != "" || got.Networks.Error != "" || got.Volumes.Error != "" || got.Secrets.Error != "" || got.Configs.Error != "" {
		t.Errorf("a card carries an error: %s", rec.Body)
	}
	// D-47: names and ownership only. No secret value, no config content, no label leaves.
	if strings.Contains(rec.Body.String(), "labels") || strings.Contains(rec.Body.String(), "namespace") {
		t.Errorf("the answer carries labels of the foreign stack: %s", rec.Body)
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("the engine received %d writes while reading an unmanaged stack's resources, want none", after-writesBefore)
	}
}

func TestUnmanagedResourcesOfAStackThatIsGoneOrManagedIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	newStack(t, admin, "web")
	for _, name := range []string{"nothing-here", "web"} {
		rec := viewer.do(http.MethodGet, unmanagedPath(name, "/resources"), "")
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
			t.Errorf("resources of %s: %d %s, want 404 unmanaged_stack_not_found", name, rec.Code, rec.Body)
		}
	}
}

// legacyLogs is the log fixture of an unmanaged stack: the tasks and lines of the stack-log tests, under the
// namespace legacy that Bayplan did not make.
func newLegacyLogsFix(t *testing.T) *stackLogsFix {
	t.Helper()
	f := newStackLogsFix(t, stackLogsTasks(), stackLogsBurst(), nil)
	cfg := f.cfg
	ns := map[string]string{"com.docker.stack.namespace": "legacy"}
	cfg.ServiceRows = []dockertest.FakeService{
		{ID: "svc-web", Name: "legacy_web", Mode: "replicated", Running: 2, Desired: 2, Image: "nginx:1.27", Labels: ns},
		{ID: "svc-api", Name: "legacy_api", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Labels: ns},
	}
	f.reconfigure(t, cfg)
	return f
}

func (f *stackLogsFix) unmanagedPath(name, tail, query string) string {
	return "/api/env/" + f.env.ID + "/" + unmanagedPath(name, tail) + "?" + query
}

func TestUnmanagedLogsFollowTheLabel(t *testing.T) {
	t.Parallel()
	f := newLegacyLogsFix(t)

	c := openSSE(t, f.authFixture, f.unmanagedPath("legacy", "/logs/stream", "sid="+sidA), f.user.cookie, "")

	if c.status0() != http.StatusOK {
		t.Fatalf("status = %d: %s", c.status0(), c.bodyString())
	}
	rd := stackReadyOf(t, c)
	wantTasks := []stackReadyTask{
		{"tk-a1", "api", 1, "mgr-1", "running"},
		{"tk-w1", "web", 1, "mgr-1", "running"},
		{"tk-w2", "web", 2, "wrk-ready", "running"},
	}
	if rd.Target != "swarm" || !slices.Equal(rd.Tasks, wantTasks) {
		t.Fatalf("ready = %+v, want the live tasks of the services found by label: %+v", rd, wantTasks)
	}
	lines, _ := readStackLines(t, c, 5)
	if got, want := stackTexts(lines), []string{"web1 a", "api a", "web2 a", "api b", "web1 b"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v: both services merged by time", got, want)
	}
	// One viewed row names the stack and has no stack ID.
	rows := f.viewedRows(t)
	if len(rows) != 1 || rows[0].TargetType != "stack" || rows[0].TargetID != "" || rows[0].Details["stack"] != "legacy" {
		t.Errorf("logs.viewed rows = %+v, want one that names legacy and carries no stack ID", rows)
	}
}

func TestUnmanagedLogDownloadMergesTheServicesFoundByLabel(t *testing.T) {
	t.Parallel()
	f := newLegacyLogsFix(t)

	w := downloadAt(t, f.envFix, f.unmanagedPath("legacy", "/logs/download", ""), f.user.cookie)

	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	want := strings.Join([]string{
		dlLine("web.1@mgr-1", 5, "web0 old"),
		dlLine("web.1@mgr-1", 10, "web1 a"),
		dlLine("api.1@mgr-1", 20, "api a"),
		dlLine("web.2@wrk-ready", 30, "web2 a"),
		dlLine("api.1@mgr-1", 40, "api b"),
		dlLine("web.1@mgr-1", 60, "web1 b"),
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="legacy-`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	rows := f.auditRowsOf(t, audit.ActionLogsDownloaded)
	if len(rows) != 1 || rows[0].TargetType != "stack" || rows[0].TargetID != "" || rows[0].Details["stack"] != "legacy" {
		t.Errorf("logs.downloaded rows = %+v, want one that names legacy and carries no stack ID", rows)
	}
}

func TestUnmanagedLogsRefuseACallerWithoutStacksReadAndAStackThatIsGoneOrManaged(t *testing.T) {
	t.Parallel()
	f := newLegacyLogsFix(t)
	f.addUser(t, "nobody", false)
	cookie, _ := f.signIn(t, "nobody", false)

	if c := openSSE(t, f.authFixture, f.unmanagedPath("legacy", "/logs/stream", "sid="+sidA), cookie, ""); c.status0() != http.StatusForbidden && c.status0() != http.StatusNotFound {
		t.Errorf("stream, a user with no role: status = %d, want a refusal", c.status0())
	}
	if w := downloadAt(t, f.envFix, f.unmanagedPath("legacy", "/logs/download", ""), cookie); w.code() != http.StatusForbidden && w.code() != http.StatusNotFound {
		t.Errorf("download, a user with no role: status = %d, want a refusal", w.code())
	}
	// shop is a stack of Bayplan's, and nothing carries the name nowhere.
	for _, name := range []string{"shop", "nowhere"} {
		if c := openSSE(t, f.authFixture, f.unmanagedPath(name, "/logs/stream", "sid="+sidA), f.user.cookie, ""); c.status0() != http.StatusNotFound {
			t.Errorf("stream of %s: status = %d: %s, want 404", name, c.status0(), c.bodyString())
		}
		if w := downloadAt(t, f.envFix, f.unmanagedPath(name, "/logs/download", ""), f.user.cookie); w.code() != http.StatusNotFound {
			t.Errorf("download of %s: status = %d, want 404", name, w.code())
		}
	}
}

func TestUnmanagedLogDownloadOfAStandaloneProjectMergesItsContainers(t *testing.T) {
	t.Parallel()
	legacy := func(_ string) []dockertest.FakeContainer {
		c := func(id, service string, number int) dockertest.FakeContainer {
			return dockertest.FakeContainer{
				ID: id, Names: []string{"/legacy-" + service + "-" + strconv.Itoa(number)}, Image: "img", State: "running", Created: 100,
				Labels: map[string]string{
					"com.docker.compose.project": "legacy", "com.docker.compose.service": service,
					"com.docker.compose.container-number": strconv.Itoa(number),
				},
			}
		}
		return []dockertest.FakeContainer{c("cweb1", "web", 1), c("cweb2", "web", 2), c("cdb1", "db", 1)}
	}
	f := newSoloFix(t, legacy, nil)
	f.seed()

	w := downloadAt(t, f.envFix, "/api/env/"+f.env.ID+"/"+unmanagedPath("legacy", "/logs/download"), f.user.cookie)

	if w.code() != http.StatusOK {
		t.Fatalf("status = %d: %s", w.code(), w.body.String())
	}
	stamp := func(ms int) string {
		return soloT0.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.000000000Z")
	}
	want := strings.Join([]string{
		"web-1 " + stamp(10) + " web1 a",
		"db-1 " + stamp(20) + " db a",
		"web-2 " + stamp(30) + " web2 a",
		"db-1 " + stamp(40) + " db b",
		"web-1 " + stamp(60) + " web1 b",
	}, "\n") + "\n"
	if got := w.body.String(); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestUnmanagedStackWithAnUnusualName(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	reconfigureSwarm(t, f, e,
		foreignSwarmService("MyApp", "web", 1, 1, nil),
		foreignSwarmService("My App", "web", 1, 1, nil),
		foreignSwarmService("a/b", "web", 1, 1, nil),
	)

	by := map[string]stackAnswer{}
	for _, it := range listWith(t, admin, "").Items {
		by[it.Name] = it
	}
	if u := by["MyApp"].Unmanaged; u == nil || !u.Adoptable || u.Reason != "" {
		t.Errorf("MyApp = %+v, want listed and adoptable", by["MyApp"])
	}
	for _, name := range []string{"My App", "a/b"} {
		u := by[name].Unmanaged
		want := "Bayplan can't adopt " + name + ": a stack name on a swarm takes only " + stacks.NameRuleText(stacks.TargetSwarm) + "."
		if u == nil || u.Adoptable || u.Reason != want {
			t.Errorf("%q = %+v, want listed, not adoptable, with the swarm reason", name, by[name])
		}
	}

	// The page of each opens, the name travelling as an encoded path parameter.
	for _, name := range []string{"MyApp", "My App", "a/b"} {
		rec := viewer.do(http.MethodGet, unmanagedPath(name, ""), "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %q: %d %s, want 200", name, rec.Code, rec.Body)
			continue
		}
		got := answerOf[unmanagedAnswer](t, rec)
		if got.Name != name || got.FoundBy.Value != name || got.ServicesTotal != 1 {
			t.Errorf("GET %q = %+v, want the stack of that exact name", name, got)
		}
		if adoptable := name == "MyApp"; got.Adoptable != adoptable || (got.Reason == "") != adoptable {
			t.Errorf("GET %q: adoptable %v reason %q, want adoptable %v", name, got.Adoptable, got.Reason, adoptable)
		}
	}
	// Resources of the name too.
	if rec := viewer.do(http.MethodGet, unmanagedPath("My App", "/resources"), ""); rec.Code != http.StatusOK {
		t.Errorf("resources of My App: %d %s, want 200", rec.Code, rec.Body)
	}
}

// generatedAnswer is the Generated compose answer.
type generatedAnswer struct {
	YAML  string `json:"yaml"`
	Notes []struct {
		Service string `json:"service"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"notes"`
	ExternalSecrets int `json:"externalSecrets"`
	ExternalConfigs int `json:"externalConfigs"`
}

// TestGeneratedCompose is the Generated compose tab (plan 08-11, STK-12, D-09, D-12, D-47): the compose file
// that rebuilds the running services of a stack Bayplan did not make, masked for a caller without
// secrets.reveal, with nothing stored and nothing written to an engine.
func TestGeneratedCompose(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	reconfigureSwarm(t, f, e, foreignSecretService("legacy", "app", 2, 2, nil))
	writesBefore := len(f.fake.Writes())

	rec := viewer.do(http.MethodGet, unmanagedPath("legacy", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose for a viewer: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	got := answerOf[generatedAnswer](t, rec)
	for what, secret := range sdSecrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the answer for a viewer carries the %s value %q", what, secret)
		}
	}
	if !strings.Contains(got.YAML, "<hidden>") || !strings.Contains(got.YAML, "image: nginx:1.27") {
		t.Errorf("yaml for a viewer = %q, want the rebuilt services with the hidden values marked", got.YAML)
	}
	if got.ExternalSecrets != 1 || got.ExternalConfigs != 1 {
		t.Errorf("externals = %d secrets, %d configs, want 1 and 1", got.ExternalSecrets, got.ExternalConfigs)
	}

	rec = admin.do(http.MethodGet, unmanagedPath("legacy", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose for an admin: %d %s", rec.Code, rec.Body)
	}
	shown := answerOf[generatedAnswer](t, rec)
	for what, secret := range sdSecrets {
		if !strings.Contains(shown.YAML, secret) {
			t.Errorf("the answer for a caller who may reveal lacks the %s value %q", what, secret)
		}
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("the engine received %d writes while rebuilding, want none", after-writesBefore)
	}
}

func TestGeneratedComposeOfAStackThatIsGoneOrManagedIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	newStack(t, admin, "web")
	for _, name := range []string{"nothing-here", "web"} {
		rec := viewer.do(http.MethodGet, unmanagedPath(name, "/compose"), "")
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "unmanaged_stack_not_found" {
			t.Errorf("generated compose of %s: %d %s, want 404 unmanaged_stack_not_found", name, rec.Code, rec.Body)
		}
	}
}

// TestGeneratedComposeCountsExternals: the preview says how many secrets and configs the file references as external
// (D-13), and lists the notes the rebuild made, in service then code order.
func TestGeneratedComposeCountsExternals(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")
	svc := foreignSecretService("legacy", "app", 1, 1, nil)
	other := foreignSecretService("legacy", "worker", 1, 1, nil)
	// A second service that uses the same secret and config counts them once.
	reconfigureSwarm(t, f, e, svc, other)

	rec := viewer.do(http.MethodGet, unmanagedPath("legacy", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[generatedAnswer](t, rec)
	if got.ExternalSecrets != 1 || got.ExternalConfigs != 1 {
		t.Errorf("externals = %d secrets, %d configs, want each counted once", got.ExternalSecrets, got.ExternalConfigs)
	}
	var codes []string
	for _, n := range got.Notes {
		codes = append(codes, n.Service+":"+n.Code)
		if n.Message == "" {
			t.Errorf("the note %s:%s has no sentence", n.Service, n.Code)
		}
	}
	if want := []string{"app:image_unpinned", "worker:image_unpinned"}; !slices.Equal(codes, want) {
		t.Errorf("notes = %v, want %v", codes, want)
	}
}

// foreignStandaloneCompose is a project somebody ran with docker compose up: values a Viewer must not read in
// its environment, its command and its log options.
const foreignStandaloneCompose = `
services:
  web:
    image: nginx:1.27
    environment:
      DB_PASSWORD: s3cret-value
      MODE: prod
    command: ["/entry.sh", "--token", "cmd-secret"]
    logging:
      driver: syslog
      options:
        syslog-address: tcp://log-secret:514
    depends_on:
      - db
    networks: [back]
    volumes:
      - data:/data
  db:
    image: postgres:17
    networks: [back]
networks:
  back: {}
volumes:
  data: {}
`

// TestGeneratedComposeStandalone is the Generated compose tab of a docker compose up project (plan 08-12, STK-13,
// D-12, D-47): rebuilt from its containers, masked for a caller without secrets.reveal, nothing written.
func TestGeneratedComposeStandalone(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	standOnHost(f, stackopstest.Foreign(t, "shop", foreignStandaloneCompose))
	writesBefore := len(f.fake.Writes())

	rec := viewer.do(http.MethodGet, unmanagedPath("shop", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose for a viewer: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[generatedAnswer](t, rec)
	for _, secret := range []string{"s3cret-value", "cmd-secret", "log-secret"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the answer for a viewer carries %q", secret)
		}
	}
	for _, want := range []string{"<hidden>", "image: nginx:1.27", "image: postgres:17", "depends_on:", "data:/data"} {
		if !strings.Contains(got.YAML, want) {
			t.Errorf("yaml for a viewer lacks %q:\n%s", want, got.YAML)
		}
	}
	if got.ExternalSecrets != 0 || got.ExternalConfigs != 0 {
		t.Errorf("externals = %d secrets, %d configs, want none on a standalone host", got.ExternalSecrets, got.ExternalConfigs)
	}

	rec = admin.do(http.MethodGet, unmanagedPath("shop", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose for an admin: %d %s", rec.Code, rec.Body)
	}
	shown := answerOf[generatedAnswer](t, rec)
	for _, secret := range []string{"s3cret-value", "cmd-secret", "log-secret"} {
		if !strings.Contains(shown.YAML, secret) {
			t.Errorf("the answer for a caller who may reveal lacks %q", secret)
		}
	}
	if after := len(f.fake.Writes()); after != writesBefore {
		t.Errorf("the engine received %d writes while rebuilding, want none", after-writesBefore)
	}
}

// TestGeneratedComposeStandaloneNotes: the preview of a docker compose project says where it could not write the
// original exactly, by service or object then code (plan 08-12, D-14, D-46): an image built on the host is
// pinned to its image ID, and a volume whose settings the host's hash does not confirm is kept external.
func TestGeneratedComposeStandaloneNotes(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")
	h := stackopstest.Foreign(t, "shop", "services:\n  web:\n    image: nginx:1.27\n    volumes: [data:/data]\n  api:\n    image: shop-api\nvolumes:\n  data: {}\n")
	for i := range h.Images {
		if h.Images[i].RepoTags[0] == "shop-api" {
			h.Images[i].RepoDigests = nil
		}
	}
	for i := range h.Volumes {
		h.Volumes[i].Labels["com.docker.compose.config-hash"] = strings.Repeat("0", 64)
	}
	standOnHost(f, h)

	rec := viewer.do(http.MethodGet, unmanagedPath("shop", "/compose"), "")

	if rec.Code != http.StatusOK {
		t.Fatalf("generated compose: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[generatedAnswer](t, rec)
	type note struct{ service, code, message string }
	var notes []note
	for _, n := range got.Notes {
		notes = append(notes, note{n.Service, n.Code, n.Message})
	}
	want := []note{
		{"api", "image_local", "api runs an image built on this host. It is pinned to its image ID; Bayplan can't look it up in a registry."},
		{"shop_data", "object_external_hash", "shop_data is kept external: Bayplan couldn't match its settings, so a deploy leaves it as it is."},
	}
	if !slices.Equal(notes, want) {
		t.Errorf("notes = %+v, want %+v", notes, want)
	}
	if !strings.Contains(got.YAML, "external: true") {
		t.Errorf("yaml does not reference the volume as external:\n%s", got.YAML)
	}
}
