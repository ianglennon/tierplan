// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
)

// GET /api/environments/warnings (plan 02-31, ENV-06, ENV-08, D-37, D-39, T-02-98): the
// API is the production router over a real tunnel hub, and the clock is the tunnel's.

type envWarning struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	EnvID    string `json:"envId"`
	EnvName  string `json:"envName"`
	Node     string `json:"node"`
	Reason   string `json:"reason"`
	Since    string `json:"since"`
	Count    int    `json:"count"`
}

// warnings decodes the answer of a call and checks that it is never cached.
func (f *agentFix) warnings(t *testing.T, cookie, csrf string) (int, []envWarning) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/warnings", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var body struct {
		Items []envWarning `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if body.Items == nil {
		t.Fatalf("items is null in %s, want a list", rec.Body)
	}
	return rec.Code, body.Items
}

func TestTheEnvironmentWarningsListAnOfflineEnvironmentWithSinceAndReasonAndNothingForOnesThatAreFine(t *testing.T) {
	f := newAgentFix(t)
	down := f.srv.NewEnvironment(t, "Edge")
	fine := f.srv.NewEnvironment(t, "Fine")
	f.connect(t, down, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	steady := f.connect(t, fine, dockertest.FakeConfig{Hostname: "fine-1"}, nil)
	f.srv.Events.WaitLen(t, 2)
	f.whenState(t, down.ID, "online")
	f.whenState(t, fine.ID, "online")

	if code, got := f.warnings(t, f.cookie, f.csrf); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("two online environments: %d %+v, want an empty list", code, got)
	}

	// Only the first environment hears nothing: its heartbeat lapses, the other's goes on.
	f.tclk.Advance(20 * time.Second)
	tunneltest.SendHeartbeat(t, steady.control, true, f.tclk.Now().UnixMilli())
	f.tclk.Advance(11 * time.Second)
	f.srv.Events.WaitLen(t, 3)
	droppedAt := f.tclk.Now()
	f.whenState(t, down.ID, "offline")

	code, got := f.warnings(t, f.cookie, f.csrf)
	if code != http.StatusOK || len(got) != 1 {
		t.Fatalf("one offline environment: %d %+v, want exactly one item", code, got)
	}
	w := got[0]
	if w.ID != "env_offline" || w.Severity != "error" || w.EnvID != down.ID || w.EnvName != "Edge" || w.Reason != "timeout" || w.Since != utc(droppedAt) {
		t.Fatalf("warning = %+v, want env_offline / error for Edge / timeout since %s", w, utc(droppedAt))
	}
}

func TestTheEnvironmentWarningsOfAUserWhoReadsOnlyOneEnvironmentAreThatEnvironmentsAlone(t *testing.T) {
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "Alpha")
	b := f.srv.NewEnvironment(t, "Beta")
	f.connect(t, a, dockertest.FakeConfig{Hostname: "a-1"}, nil)
	f.connect(t, b, dockertest.FakeConfig{Hostname: "b-1"}, nil)
	f.srv.Events.WaitLen(t, 2)
	f.whenState(t, a.ID, "online")
	f.whenState(t, b.ID, "online")
	f.tclk.Advance(31 * time.Second)
	f.srv.Events.WaitLen(t, 4)
	f.whenState(t, a.ID, "offline")
	f.whenState(t, b.ID, "offline")

	if _, all := f.warnings(t, f.cookie, f.csrf); len(all) != 2 {
		t.Fatalf("the admin sees %+v, want both environments", all)
	}

	f.addUser(t, "alice", false)
	f.grantRead(t, "alice", a.ID)
	cookie, csrf := f.signIn(t, "alice", false)
	code, got := f.warnings(t, cookie, csrf)
	if code != http.StatusOK || len(got) != 1 || got[0].EnvID != a.ID || got[0].EnvName != "Alpha" {
		t.Fatalf("a user who may read Alpha only: %d %+v, want Alpha's item alone", code, got)
	}

	f.addUser(t, "nobody", false)
	cookie, csrf = f.signIn(t, "nobody", false)
	if code, got := f.warnings(t, cookie, csrf); code != http.StatusOK || len(got) != 0 {
		t.Fatalf("a user who may read nothing: %d %+v, want an empty list", code, got)
	}
	if code, _ := f.warnings(t, "", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d, want 401", code)
	}
}

func TestTheEnvironmentWarningsCountAgentsThatNeedAManualUpdateInOneItem(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Production")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "refusing", SwarmState: "active"}, swarmFacts("refusing", true, false))
	f.connect(t, env, dockertest.FakeConfig{Hostname: "letting", SwarmState: "active"}, swarmFacts("letting", false, true))
	f.srv.Events.WaitLen(t, 2)

	code, got := f.warnings(t, f.cookie, f.csrf)
	if code != http.StatusOK || len(got) != 1 {
		t.Fatalf("%d %+v, want one item: only the host that refuses self-update needs a manual update", code, got)
	}
	if w := got[0]; w.ID != "manual_update_needed" || w.Count != 1 || w.EnvID != env.ID || w.Severity != "warning" {
		t.Fatalf("warning = %+v, want manual_update_needed, count 1, for the environment", w)
	}
}

func TestAPendingEnvironmentIsALongWaitOnlyAfterSevenDays(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Waiting")
	if _, got := f.warnings(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("a new pending environment: %+v, want nothing", got)
	}
	f.tclk.Advance(7*24*time.Hour + time.Minute)
	_, got := f.warnings(t, f.cookie, f.csrf)
	if len(got) != 1 || got[0].ID != "pending_long" || got[0].EnvID != env.ID || got[0].Severity != "warning" {
		t.Fatalf("after 7 days: %+v, want pending_long for the environment", got)
	}
}

func TestAFoldOfferIsAnItemOnlyForSomeoneWhoMayManageEnvironments(t *testing.T) {
	f := newFoldFix(t, nil)

	_, got := f.warnings(t, f.cookie, f.csrf)
	var fold []envWarning
	for _, w := range got {
		if w.ID == "fold_offered" {
			fold = append(fold, w)
		}
	}
	if len(fold) != 1 || fold[0].EnvID != f.env.ID || fold[0].EnvName != "Swarm" || fold[0].Severity != "warning" {
		t.Fatalf("the admin's fold items = %+v (all %+v), want one naming the swarm environment", fold, got)
	}

	f.addUserWithGlobalPerms(t, "reader", "environments.read")
	cookie, csrf := f.signIn(t, "reader", false)
	_, got = f.warnings(t, cookie, csrf)
	for _, w := range got {
		if w.ID == "fold_offered" {
			t.Fatalf("someone without environments.manage was told of the fold: %+v", got)
		}
	}
}
