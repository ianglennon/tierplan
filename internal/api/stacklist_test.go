// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

// The stack lists (plan 07-24, STK-10, STK-02, STK-01, UI-SPEC "Stack lists"): the All stacks list across
// environments. Environment A is the local swarm manager the stack tests use; environment B is an agent
// environment with no agent connected, whose stacks are stored rows only.

type allStacksAnswer struct {
	Groups []struct {
		EnvID   string        `json:"envId"`
		EnvName string        `json:"envName"`
		Target  string        `json:"target"`
		Stacks  []stackAnswer `json:"stacks"`
	} `json:"groups"`
	FailedEnvironments []struct {
		EnvID   string `json:"envId"`
		EnvName string `json:"envName"`
	} `json:"failedEnvironments"`
}

// twoStackEnvironments makes environment A (the local swarm) and environment B (an agent environment).
// failB, when set, names the environment whose stacks cannot be read.
func twoStackEnvironments(t *testing.T, failB *atomic.Pointer[string]) (*envFix, envBody, envBody) {
	t.Helper()
	f, a := stackEnvTuned(t, func(f *envFix) {
		if failB != nil {
			f.stackReadEnv = func(_ context.Context, id string) error {
				if p := failB.Load(); p != nil && *p == id {
					return errors.New("the read failed")
				}
				return nil
			}
		}
	})
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated {
		t.Fatalf("add the agent environment: %d %s", rec.code, rec.body)
	}
	return f, a, envBody{ID: got.ID, Name: "edge"}
}

func (f *envFix) allStacks(t *testing.T, cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: "/api/stacks" + query, cookie: cookie})
}

func TestAllStacksGroupsTheStacksOfEveryEnvironmentTheCallerCanRead(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	admin := f.caller(t, a, "envadmin-1", "admin")
	web := newStack(t, admin, "web")
	edgeID := f.seedStack(t, b.ID, "edge-proxy", stacks.TargetSwarm)

	rec := f.allStacks(t, f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("all stacks: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[allStacksAnswer](t, rec)
	if len(got.Groups) != 2 || len(got.FailedEnvironments) != 0 {
		t.Fatalf("answer = %s, want two groups and none failed", rec.Body)
	}
	byEnv := map[string]int{}
	for i, g := range got.Groups {
		byEnv[g.EnvID] = i
		if g.EnvName == "" || g.Target != "swarm" || len(g.Stacks) != 1 {
			t.Errorf("group %+v, want a named swarm group with one stack", g)
		}
	}
	if s := got.Groups[byEnv[a.ID]].Stacks[0]; s.ID != web.ID || s.Name != "web" || s.State != "not_deployed" {
		t.Errorf("group A holds %+v, want the web stack not deployed", s)
	}
	if s := got.Groups[byEnv[b.ID]].Stacks[0]; s.ID != edgeID || s.Name != "edge-proxy" {
		t.Errorf("group B holds %+v, want edge-proxy", s)
	}
	if got.Groups[0].EnvName > got.Groups[1].EnvName {
		t.Errorf("groups are %q then %q, want them by environment name", got.Groups[0].EnvName, got.Groups[1].EnvName)
	}
}

func TestAllStacksLeavesOutAnEnvironmentTheCallerHoldsNoStacksReadOnAndItsName(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	admin := f.caller(t, a, "envadmin-1", "admin")
	newStack(t, admin, "web")
	f.seedStack(t, b.ID, "secret-edge-stack", stacks.TargetSwarm)
	f.addUser(t, "alice", false)
	f.addEnvRole(t, "alice", a.ID, "viewer")
	cookie, _ := f.signIn(t, "alice", false)

	rec := f.allStacks(t, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("all stacks: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[allStacksAnswer](t, rec)
	if len(got.Groups) != 1 || got.Groups[0].EnvID != a.ID {
		t.Fatalf("groups = %s, want environment A only", rec.Body)
	}
	for _, leak := range []string{b.ID, "edge", "secret-edge-stack"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the answer to a caller with no grant on B names %q: %s", leak, rec.Body)
		}
	}
	if len(got.FailedEnvironments) != 0 {
		t.Errorf("failedEnvironments = %+v, want none: an environment the caller cannot read is not a failed one", got.FailedEnvironments)
	}
}

func TestAllStacksListsAnEnvironmentWhoseReadFailsAsFailedAndStillAnswersTheOthers(t *testing.T) {
	t.Parallel()
	var failB atomic.Pointer[string]
	f, a, b := twoStackEnvironments(t, &failB)
	admin := f.caller(t, a, "envadmin-1", "admin")
	newStack(t, admin, "web")
	f.seedStack(t, b.ID, "edge-proxy", stacks.TargetSwarm)
	failB.Store(&b.ID)

	rec := f.allStacks(t, f.cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("all stacks: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[allStacksAnswer](t, rec)
	if len(got.Groups) != 1 || got.Groups[0].EnvID != a.ID {
		t.Fatalf("groups = %s, want A's alone", rec.Body)
	}
	if len(got.FailedEnvironments) != 1 || got.FailedEnvironments[0].EnvID != b.ID || got.FailedEnvironments[0].EnvName != "edge" {
		t.Fatalf("failedEnvironments = %+v, want B named edge", got.FailedEnvironments)
	}
}

func TestAllStacksTakesAnEnvironmentFilterAndIgnoresOneTheCallerCannotRead(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	admin := f.caller(t, a, "envadmin-1", "admin")
	newStack(t, admin, "web")
	f.seedStack(t, b.ID, "edge-proxy", stacks.TargetSwarm)

	only := answerOf[allStacksAnswer](t, f.allStacks(t, f.cookie, "?env="+b.ID))
	if len(only.Groups) != 1 || only.Groups[0].EnvID != b.ID {
		t.Fatalf("env=B answered %+v, want B's group alone", only.Groups)
	}

	f.addUser(t, "alice", false)
	f.addEnvRole(t, "alice", a.ID, "viewer")
	cookie, _ := f.signIn(t, "alice", false)
	rec := f.allStacks(t, cookie, "?env="+b.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("env=B as alice: %d %s", rec.Code, rec.Body)
	}
	if got := answerOf[allStacksAnswer](t, rec); len(got.Groups) != 0 || len(got.FailedEnvironments) != 0 {
		t.Errorf("env=B as a caller with no grant on B answered %s, want nothing", rec.Body)
	}
}

func TestAllStacksNeedsASignedInCaller(t *testing.T) {
	t.Parallel()
	f, _, _ := twoStackEnvironments(t, nil)
	if rec := f.allStacks(t, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("all stacks signed out: %d %s, want 401", rec.Code, rec.Body)
	}
}
