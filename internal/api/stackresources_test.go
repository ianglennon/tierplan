// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// The Resources tab of a stack (plan 07-25, STK-10, D-22): everything the stack owns on its environment, one card
// for each kind, each read on its own. Docker is a fake engine behind the real monitor, aggregator and cluster
// reader.

type resourcesCardError struct {
	Error string `json:"error"`
}

type stackResourcesAnswer struct {
	Tasks *struct {
		resourcesCardError
		Items []struct {
			Task struct {
				ID, State, DockerState, Node, NodeID string
				Slot                                 int
			} `json:"task"`
			ServiceID, Service string
			Global             bool
		} `json:"items"`
	} `json:"tasks"`
	Containers *struct {
		resourcesCardError
		Items []struct {
			ID, Name, Service, State, Health, Image string
			Ports                                   []string
		} `json:"items"`
	} `json:"containers"`
	Networks struct {
		resourcesCardError
		Items []struct{ ID, Name, Driver, Scope, Ownership string } `json:"items"`
	} `json:"networks"`
}

func TestStackResourcesOfADeployedSwarmStackListItsTasksAndNetworks(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	// Another stack of the same swarm, under its own namespace, owns tasks and a network of its own.
	other := newStack(t, admin, "other")
	if job := f.deployAndWait(t, operator, other.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Tasks == nil || got.Containers != nil {
		t.Fatalf("a swarm stack answers the tasks card and not the containers card: %s", rec.Body)
	}
	if got.Tasks.Error != "" || len(got.Tasks.Items) != 1 {
		t.Fatalf("tasks = %+v, want the one task of this stack's one service", got.Tasks)
	}
	task := got.Tasks.Items[0]
	if task.Service != "web" || task.ServiceID == "" || task.Global || task.Task.Slot != 1 || task.Task.State != "running" || task.Task.ID == "" {
		t.Errorf("task = %+v", task)
	}
	if got.Networks.Error != "" {
		t.Errorf("networks error = %q", got.Networks.Error)
	}
	if len(got.Networks.Items) != 1 || got.Networks.Items[0].Name != "tracer_default" || got.Networks.Items[0].Ownership != "stack" || got.Networks.Items[0].Scope != "swarm" {
		t.Errorf("networks = %+v, want this stack's default network alone, not the other stack's", got.Networks.Items)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestStackResourcesOfAStackNeverDeployedAreEmptyCardsNotAnError(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "fresh")

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tasks":{"items":[],"error":""}`) || !strings.Contains(body, `"networks":{"items":[],"error":""}`) {
		t.Errorf("resources = %s, want empty cards with no error, not a missing card", body)
	}
}

func TestStackResourcesOfAStackThatIsNotThisEnvironmentsIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")

	rec := viewer.do(http.MethodGet, "stacks/00000000-0000-0000-0000-000000000000/resources", "")

	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("resources of a foreign stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
}

func TestStackResourcesOfAStandaloneStackListItsContainersAndNetworks(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, standaloneReviewCompose)
	standOnHost(f, h)

	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/resources", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("resources: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackResourcesAnswer](t, rec)
	if got.Containers == nil || got.Tasks != nil {
		t.Fatalf("a standalone stack answers the containers card and not the tasks card: %s", rec.Body)
	}
	if got.Containers.Error != "" || len(got.Containers.Items) != 1 {
		t.Fatalf("containers = %+v", got.Containers)
	}
	web := got.Containers.Items[0]
	if web.ID != h.IDs["web"] || web.Service != "web" || web.State != "running" || web.Name == "" || strings.HasPrefix(web.Name, "/") {
		t.Errorf("web = %+v", web)
	}
	if len(h.Networks) == 0 || len(got.Networks.Items) != len(h.Networks) {
		t.Fatalf("networks = %+v, want the %d the project made", got.Networks.Items, len(h.Networks))
	}
	for _, n := range got.Networks.Items {
		if n.Ownership != "stack" || !strings.HasPrefix(n.Name, "solo_") {
			t.Errorf("network %+v is the project's own, owned by the stack", n)
		}
	}
}
