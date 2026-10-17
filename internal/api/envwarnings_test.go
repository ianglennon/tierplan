// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel"
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
	t.Parallel()
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
	awaitHeartbeat(t, f.srv.Hub.Session(steady.id), func(st tunnel.SessionStatus) bool { return st.LastHeartbeat.Equal(f.tclk.Now()) })
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestAFoldOfferIsAnItemOnlyForAnInstanceAdmin(t *testing.T) {
	t.Parallel()
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

	f.addUser(t, "reader", false)
	f.addEnvRole(t, "reader", "", "viewer")
	cookie, csrf := f.signIn(t, "reader", false)
	_, got = f.warnings(t, cookie, csrf)
	for _, w := range got {
		if w.ID == "fold_offered" {
			t.Fatalf("someone who is not an instance admin was told of the fold: %+v", got)
		}
	}
}

// Restart-loop items (plan 06-35, RES-13, D-15). loopEnv is a swarm manager whose history
// holds web in a loop; the bell reads the cluster reader's last read, which the first list of
// services (or the background loop, in production) has made.

type loopWarning struct {
	ID            string `json:"id"`
	Severity      string `json:"severity"`
	EnvID         string `json:"envId"`
	EnvName       string `json:"envName"`
	Service       string `json:"service"`
	ServiceID     string `json:"serviceId"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"windowMinutes"`
	LatestError   string `json:"latestError"`
	Since         string `json:"since"`
}

func (f *envFix) loopWarnings(t *testing.T, cookie, csrf string) []loopWarning {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/warnings", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("warnings = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []loopWarning `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var out []loopWarning
	for _, w := range body.Items {
		if w.ID == "restart_loop" {
			out = append(out, w)
		}
	}
	return out
}

func TestEnvWarningsRestartLoopNamesTheServiceItsFailuresAndTheLatestError(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	listServicesQuery(t, f, e, "")
	got := f.loopWarnings(t, f.cookie, f.csrf)
	if len(got) != 1 {
		t.Fatalf("restart_loop items = %+v, want one, for web", got)
	}
	w := got[0]
	if w.Severity != "error" || w.EnvID != e.ID || w.EnvName != e.Name || w.Service != "web" || w.ServiceID != "svc-web" ||
		w.Count != 3 || w.WindowMinutes != 10 || w.LatestError != "task: non-zero exit (1)" {
		t.Errorf("item = %+v", w)
	}
	if _, err := time.Parse(time.RFC3339, w.Since); err != nil {
		t.Errorf("since = %q: %v", w.Since, err)
	}
}

func TestEnvWarningsRestartLoopLeavesOutAMutedServiceAndComesBackWhenItIsUnmuted(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	listServicesQuery(t, f, e, "")
	op := loopAs(t, f, e, "bell-operator", "operator")
	if rec := op(http.MethodPut, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if got := f.loopWarnings(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("items after the mute = %+v, want none", got)
	}
	if rec := op(http.MethodDelete, "services/svc-web/restart-loop-mute", ""); rec.code != http.StatusOK {
		t.Fatalf("unmute = %d %s", rec.code, rec.body)
	}
	if got := f.loopWarnings(t, f.cookie, f.csrf); len(got) != 1 {
		t.Fatalf("items after the unmute = %+v, want web again", got)
	}
}

func TestEnvWarningsRestartLoopIsShownToAViewerOfTheEnvironmentAndToNobodyElse(t *testing.T) {
	t.Parallel()
	f, e := loopEnv(t)
	listServicesQuery(t, f, e, "")
	f.addUser(t, "bell-viewer", false)
	f.addEnvRole(t, "bell-viewer", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "bell-viewer", false)
	if got := f.loopWarnings(t, cookie, csrf); len(got) != 1 || got[0].Service != "web" {
		t.Errorf("a Viewer's items = %+v, want web's", got)
	}
	f.addUser(t, "bell-nobody", false)
	cookie, csrf = f.signIn(t, "bell-nobody", false)
	if got := f.loopWarnings(t, cookie, csrf); len(got) != 0 {
		t.Errorf("a user with no grant got %+v, want nothing: the bell must not reveal what the caller cannot open", got)
	}
}

// Restart loops of standalone containers in the bell (plan 06-36, RES-13, D-15).

type containerLoopWarning struct {
	ID            string `json:"id"`
	Severity      string `json:"severity"`
	EnvID         string `json:"envId"`
	EnvName       string `json:"envName"`
	Container     string `json:"container"`
	ContainerID   string `json:"containerId"`
	NodeID        string `json:"nodeId"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"windowMinutes"`
	LastExitCode  string `json:"lastExitCode"`
	Since         string `json:"since"`
}

func (f *envFix) containerLoopWarnings(t *testing.T, cookie, csrf string) []containerLoopWarning {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments/warnings", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("warnings = %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Items []containerLoopWarning `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var out []containerLoopWarning
	for _, w := range body.Items {
		if w.ID == "restart_loop_container" {
			out = append(out, w)
		}
	}
	return out
}

func TestEnvWarningsRestartLoopContainerNamesTheContainerItsNodeAndTheLastExitCode(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	got := f.containerLoopWarnings(t, f.cookie, f.csrf)
	if len(got) != 1 {
		t.Fatalf("restart_loop_container items = %+v, want one, for crashy", got)
	}
	w := got[0]
	if w.Severity != "error" || w.EnvID != e.ID || w.EnvName != e.Name || w.Container != "crashy" || w.ContainerID != loopCrashyID || w.NodeID != e.ID ||
		w.Count != 3 || w.WindowMinutes != 10 || w.LastExitCode != "3" {
		t.Errorf("item = %+v", w)
	}
	if _, err := time.Parse(time.RFC3339, w.Since); err != nil {
		t.Errorf("since = %q: %v", w.Since, err)
	}
}

func TestEnvWarningsRestartLoopContainerLeavesOutAMutedContainerAndComesBackWhenItIsUnmuted(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	op := loopAs(t, f, e, "bell-operator", "operator")
	if rec := op(http.MethodPut, containerMutePath(e, "crashy"), ""); rec.code != http.StatusOK {
		t.Fatalf("mute = %d %s", rec.code, rec.body)
	}
	if got := f.containerLoopWarnings(t, f.cookie, f.csrf); len(got) != 0 {
		t.Fatalf("items after the mute = %+v, want none", got)
	}
	if rec := op(http.MethodDelete, containerMutePath(e, "crashy"), ""); rec.code != http.StatusOK {
		t.Fatalf("unmute = %d %s", rec.code, rec.body)
	}
	if got := f.containerLoopWarnings(t, f.cookie, f.csrf); len(got) != 1 {
		t.Fatalf("items after the unmute = %+v, want crashy again", got)
	}
}

func TestEnvWarningsRestartLoopContainerIsShownToAViewerOfTheEnvironmentAndToNobodyElse(t *testing.T) {
	t.Parallel()
	f, e := containerLoopEnv(t)
	f.addUser(t, "bell-viewer", false)
	f.addEnvRole(t, "bell-viewer", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "bell-viewer", false)
	if got := f.containerLoopWarnings(t, cookie, csrf); len(got) != 1 || got[0].Container != "crashy" {
		t.Errorf("a Viewer's items = %+v, want crashy's", got)
	}
	f.addUser(t, "bell-nobody", false)
	cookie, csrf = f.signIn(t, "bell-nobody", false)
	if got := f.containerLoopWarnings(t, cookie, csrf); len(got) != 0 {
		t.Errorf("a user with no grant got %+v, want nothing: the bell must not reveal what the caller cannot open", got)
	}
}
