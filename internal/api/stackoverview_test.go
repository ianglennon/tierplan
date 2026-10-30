// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// The Overview of a stack (plan 07-23, STK-10, D-22): what the stack runs now, read from the engine. Docker is a
// fake engine behind the real monitor, aggregator and cluster reader.

type stackServicesAnswer struct {
	Items []struct {
		ID, Name, Short, Mode, State, UpdateState string
		Running, Desired                          int
		Image, Pinned, PinState                   string
		Ports                                     []string
		Containers                                []struct{ ID, Name, State, Health string }
	} `json:"items"`
}

func TestStackServicesOfADeployedSwarmStackAnswerWhatRunsWithItsPinnedImage(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/services", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("services: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackServicesAnswer](t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v, want the one service the starter file has", got.Items)
	}
	web := got.Items[0]
	if web.Name != "tracer_web" || web.Short != "web" || web.ID == "" || web.State != "running" || web.Running != 1 || web.Desired != 1 {
		t.Errorf("web = %+v", web)
	}
	if web.Image != "nginx:1.27-alpine" || web.PinState != "pinned" || !strings.HasPrefix(web.Pinned, "nginx:1.27-alpine@sha256:") {
		t.Errorf("web image = %q pinned = %q state = %q", web.Image, web.Pinned, web.PinState)
	}
	if len(web.Containers) != 0 {
		t.Errorf("a swarm service lists no containers: %+v", web.Containers)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestStackServicesOfAStackNeverDeployedIsAnEmptyList(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "fresh")

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/services", "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("services of a stack never deployed: %d %s, want an empty list a Viewer may read", rec.Code, rec.Body)
	}
}

func TestStackServicesOfAStackThatIsNotThisEnvironmentsIs404(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	viewer := f.caller(t, e, "viewer-1", "viewer")

	rec := viewer.do(http.MethodGet, "stacks/00000000-0000-0000-0000-000000000000/services", "")

	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("services of a foreign stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
}

func TestStackServicesLeaveOutAServiceOfTheSameNamespaceCoamingDidNotMake(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	// Another stack of the swarm, under its own namespace, is never this stack's.
	other := newStack(t, admin, "other")
	if job := f.deployAndWait(t, operator, other.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	got := answerOf[stackServicesAnswer](t, operator.do(http.MethodGet, "stacks/"+st.ID+"/services", ""))

	if len(got.Items) != 1 || got.Items[0].Name != "tracer_web" {
		t.Errorf("items = %+v, want this stack's service alone", got.Items)
	}
}

func TestStackServicesOfAStandaloneStackListEachServiceWithItsContainers(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	operator, st, h := deployedStandalone(t, f, e, standaloneReviewCompose)
	standOnHost(f, h)

	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/services", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("services: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackServicesAnswer](t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("items = %+v", got.Items)
	}
	web := got.Items[0]
	if web.Name != "web" || web.ID != "" || web.State != "running" || web.Running != 1 || web.Desired != 1 || web.Image != "nginx:1.27-alpine" {
		t.Errorf("web = %+v", web)
	}
	if len(web.Containers) != 1 || web.Containers[0].ID != h.IDs["web"] || web.Containers[0].State != "running" {
		t.Errorf("containers = %+v", web.Containers)
	}
	if web.PinState != "pinned" || !strings.Contains(web.Pinned, "@sha256:") {
		t.Errorf("pinned = %q state = %q", web.Pinned, web.PinState)
	}
}
