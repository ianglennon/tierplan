// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
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

func TestUnmanagedStackDetailOfAStandaloneProject(t *testing.T) {
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
