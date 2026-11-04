// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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

// deployedSwarmStack is a stack that has deployed once, whose services the fake then holds as services.
func deployedSwarmStack(t *testing.T, f *envFix, e envBody, admin, operator *roleCaller, name string) stackAnswer {
	t.Helper()
	st := newStack(t, admin, name)
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
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
	operator := f.caller(t, e, "operator-1", "operator")
	st := deployedSwarmStack(t, f, e, admin, operator, "shop")
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
