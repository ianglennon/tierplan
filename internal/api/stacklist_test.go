// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The stack lists (plan 07-24, STK-10, STK-02, STK-01, UI-SPEC "Stack lists"): the All stacks list across
// environments. Environment A is the local swarm manager the stack tests use; environment B is an agent
// environment with no agent connected, whose stacks are stored rows only.

// statusCountsAnswer is how many stacks each status chip counts.
type statusCountsAnswer struct {
	Degraded     int `json:"degraded"`
	Stopped      int `json:"stopped"`
	Draft        int `json:"draft"`
	DeployFailed int `json:"deployFailed"`
}

// stackListAnswer is the list of one environment's stacks.
type stackListAnswer struct {
	Items           []stackAnswer      `json:"items"`
	Total           int                `json:"total"`
	UnfilteredTotal int                `json:"unfilteredTotal"`
	Capped          bool               `json:"capped"`
	StatusCounts    statusCountsAnswer `json:"statusCounts"`
}

type allStacksAnswer struct {
	Total           int                `json:"total"`
	UnfilteredTotal int                `json:"unfilteredTotal"`
	Capped          bool               `json:"capped"`
	StatusCounts    statusCountsAnswer `json:"statusCounts"`
	Groups          []struct {
		EnvID   string             `json:"envId"`
		EnvName string             `json:"envName"`
		Target  string             `json:"target"`
		Stacks  []stackAnswer      `json:"stacks"`
		Counts  statusCountsAnswer `json:"counts"`
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

// listWith reads the environment's stack list with a query string such as "?q=web".
func listWith(t *testing.T, c *roleCaller, query string) stackListAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks"+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list stacks%s: %d %s", query, rec.Code, rec.Body)
	}
	return answerOf[stackListAnswer](t, rec)
}

func stackNames(items []stackAnswer) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

// mixedStacks makes five stacks on the environment, one in each of the states the list filters and
// sorts by, and returns the env Admin who made them:
//
//	alpha    deployed, one of its two services running: degraded, and the only one ever deployed
//	bravo    stopped, and deployed once after alpha
//	charlie  its last deploy failed
//	delta    a draft saved since it was made
//	echo     made and left alone: not deployed
func mixedStacks(t *testing.T, f *envFix, e envBody) *roleCaller {
	t.Helper()
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	repo := stackops.Repo{Store: f.st, Vault: f.v}

	alpha := newStack(t, admin, "alpha")
	if job := f.deployAndWait(t, operator, alpha.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy alpha: %+v", job)
	}
	reconfigureSwarm(t, f, e, ownedService(alpha, "worker", 1, 3))

	bravo := f.seedStack(t, e.ID, "bravo", stacks.TargetSwarm)
	if _, err := f.st.Write.Exec(`UPDATE stacks SET stopped = 1 WHERE id = ?`, bravo); err != nil {
		t.Fatal(err)
	}

	earlier, err := repo.StartDeploy(testsupport.Context(t), bravo, e.ID, stackops.DeployKindDeploy, 1, "job-bravo", "operator-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDeploy(testsupport.Context(t), earlier.ID, stackops.DeployDone, "", ""); err != nil {
		t.Fatal(err)
	}

	charlie := f.seedStack(t, e.ID, "charlie", stacks.TargetSwarm)
	row, err := repo.StartDeploy(testsupport.Context(t), charlie, e.ID, stackops.DeployKindDeploy, 1, "job-charlie", "operator-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDeploy(testsupport.Context(t), row.ID, stackops.DeployFailed, "web", "service_rejected"); err != nil {
		t.Fatal(err)
	}

	delta := newStack(t, admin, "delta")
	saveCompose(t, operator, delta.ID, stacks.StarterCompose+"# tuned\n", `[]`)

	newStack(t, admin, "echo")
	return admin
}

func TestStackListSearchMatchesAStackNameOrOneOfItsServiceNames(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	tracer := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, tracer.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	newStack(t, admin, "other")

	for query, want := range map[string][]string{
		"?q=tracer":       {"tracer"},
		"?q=TRA":          {"tracer"},
		"?q=web":          {"tracer"}, // the starter file's one service
		"?q=oth":          {"other"},
		"?q=%20%20web%20": {"tracer"},
		"?q=nomatch":      {},
		"":                {"other", "tracer"},
	} {
		got := listWith(t, operator, query)
		if names := stackNames(got.Items); !slices.Equal(names, want) {
			t.Errorf("list%s = %v, want %v", query, names, want)
		}
		if got.Total != len(want) || got.UnfilteredTotal != 2 || got.Capped {
			t.Errorf("list%s: total %d, unfiltered %d, capped %v, want %d, 2, false", query, got.Total, got.UnfilteredTotal, got.Capped, len(want))
		}
	}
}

func TestStackListStatusFiltersCombineAndTheirCountsComeFromTheUnfilteredSet(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	operator := mixedStacks(t, f, e)
	wantCounts := statusCountsAnswer{Degraded: 1, Stopped: 1, Draft: 1, DeployFailed: 1}

	for query, want := range map[string][]string{
		"":                           {"alpha", "bravo", "charlie", "delta", "echo"},
		"?status=degraded":           {"alpha"},
		"?status=stopped":            {"bravo"},
		"?status=deploy_failed":      {"charlie"},
		"?status=draft":              {"delta"},
		"?status=stopped,draft":      {"bravo", "delta"},
		"?status=draft,degraded":     {"alpha", "delta"},
		"?status=draft&q=alp":        {},
		"?status=degraded,draft&q=a": {"alpha", "delta"},
	} {
		got := listWith(t, operator, query)
		if names := stackNames(got.Items); !slices.Equal(names, want) {
			t.Errorf("list%s = %v, want %v", query, names, want)
		}
		if got.Total != len(want) || got.UnfilteredTotal != 5 {
			t.Errorf("list%s: total %d, unfiltered %d, want %d, 5", query, got.Total, got.UnfilteredTotal, len(want))
		}
		if got.StatusCounts != wantCounts {
			t.Errorf("list%s: counts %+v, want %+v whatever is typed or chosen", query, got.StatusCounts, wantCounts)
		}
	}

	rec := operator.do(http.MethodGet, "stacks?status=nonsense", "")
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "status_invalid" {
		t.Errorf("an unknown status: %d %s, want 422 status_invalid", rec.Code, rec.Body)
	}
}

func TestStackListSortsByNameStateAndLastDeployedBothWays(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	operator := mixedStacks(t, f, e)

	for query, want := range map[string][]string{
		"":                            {"alpha", "bravo", "charlie", "delta", "echo"},
		"?sort=name&dir=desc":         {"echo", "delta", "charlie", "bravo", "alpha"},
		"?sort=state":                 {"bravo", "charlie", "alpha", "delta", "echo"},
		"?sort=state&dir=desc":        {"delta", "echo", "alpha", "charlie", "bravo"},
		"?sort=lastDeployed":          {"alpha", "bravo", "charlie", "delta", "echo"},
		"?sort=lastDeployed&dir=desc": {"bravo", "alpha", "charlie", "delta", "echo"},
		"?sort=name&dir=asc&status=stopped,draft": {"bravo", "delta"},
	} {
		got := listWith(t, operator, query)
		if names := stackNames(got.Items); !slices.Equal(names, want) {
			t.Errorf("list%s = %v, want %v", query, names, want)
		}
	}
	// The state order is the UI-SPEC priority (stopped, deploy failed, degraded, running, not deployed) and
	// breaks a tie by name; a stack never deployed comes after every deployed one whichever way the list runs.
	for _, bad := range []string{"?sort=sideways", "?dir=up"} {
		if rec := operator.do(http.MethodGet, "stacks"+bad, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("list%s: %d, want 422", bad, rec.Code)
		}
	}
}

func TestStackListRefusesASearchOverTheLimit(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	rec := admin.do(http.MethodGet, "stacks?q="+strings.Repeat("a", 129), "")
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "query_too_long" {
		t.Errorf("a 129-character search: %d %s, want 422 query_too_long", rec.Code, rec.Body)
	}
	if rec := admin.do(http.MethodGet, "stacks?q="+strings.Repeat("a", 128), ""); rec.Code != http.StatusOK {
		t.Errorf("a 128-character search: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestAllStacksTakesTheSameSearchStatusAndSortAndCountsAcrossEnvironments(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	admin := f.caller(t, a, "envadmin-1", "admin")
	web := newStack(t, admin, "web")
	saveCompose(t, admin, web.ID, stacks.StarterCompose+"# tuned\n", `[]`)
	f.seedStack(t, a.ID, "api", stacks.TargetSwarm)
	edge := f.seedStack(t, b.ID, "edge", stacks.TargetSwarm)
	if _, err := f.st.Write.Exec(`UPDATE stacks SET stopped = 1 WHERE id = ?`, edge); err != nil {
		t.Fatal(err)
	}
	f.seedStack(t, b.ID, "web-edge", stacks.TargetSwarm)

	all := func(query string) allStacksAnswer {
		rec := f.allStacks(t, f.cookie, query)
		if rec.Code != http.StatusOK {
			t.Fatalf("all stacks%s: %d %s", query, rec.Code, rec.Body)
		}
		return answerOf[allStacksAnswer](t, rec)
	}
	names := func(g allStacksAnswer) map[string][]string {
		out := map[string][]string{}
		for _, grp := range g.Groups {
			out[grp.EnvID] = stackNames(grp.Stacks)
		}
		return out
	}

	got := all("")
	if got.Total != 4 || got.UnfilteredTotal != 4 || got.Capped {
		t.Errorf("total %d unfiltered %d capped %v, want 4, 4, false", got.Total, got.UnfilteredTotal, got.Capped)
	}
	if want := (statusCountsAnswer{Stopped: 1, Draft: 1}); got.StatusCounts != want {
		t.Errorf("counts = %+v, want %+v", got.StatusCounts, want)
	}

	got = all("?q=web")
	if want := map[string][]string{a.ID: {"web"}, b.ID: {"web-edge"}}; !equalGroups(names(got), want) {
		t.Errorf("q=web = %v, want %v", names(got), want)
	}
	if got.Total != 2 || got.UnfilteredTotal != 4 {
		t.Errorf("q=web total %d unfiltered %d, want 2, 4", got.Total, got.UnfilteredTotal)
	}
	if want := (statusCountsAnswer{Stopped: 1, Draft: 1}); got.StatusCounts != want {
		t.Errorf("counts under a search = %+v, want the unfiltered %+v", got.StatusCounts, want)
	}

	got = all("?status=stopped,draft")
	if want := map[string][]string{a.ID: {"web"}, b.ID: {"edge"}}; !equalGroups(names(got), want) {
		t.Errorf("status=stopped,draft = %v, want %v", names(got), want)
	}

	got = all("?sort=name&dir=desc")
	if want := map[string][]string{a.ID: {"web", "api"}, b.ID: {"web-edge", "edge"}}; !equalGroups(names(got), want) {
		t.Errorf("sort=name&dir=desc = %v, want %v", names(got), want)
	}

	// The environment filter narrows what the counts and the totals are of.
	got = all("?env=" + b.ID)
	if got.Total != 2 || got.UnfilteredTotal != 2 || got.StatusCounts != (statusCountsAnswer{Stopped: 1}) {
		t.Errorf("env=B: total %d unfiltered %d counts %+v, want 2, 2 and one stopped", got.Total, got.UnfilteredTotal, got.StatusCounts)
	}

	if rec := f.allStacks(t, f.cookie, "?status=nonsense"); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "status_invalid" {
		t.Errorf("an unknown status: %d %s, want 422 status_invalid", rec.Code, rec.Body)
	}
	if rec := f.allStacks(t, f.cookie, "?q="+strings.Repeat("a", 129)); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "query_too_long" {
		t.Errorf("a long search: %d %s, want 422 query_too_long", rec.Code, rec.Body)
	}
}

func equalGroups(got, want map[string][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if !slices.Equal(got[k], v) {
			return false
		}
	}
	return true
}

func TestAllStacksCapsTheRowsAcrossEnvironmentsAndSaysHowManyThereAre(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	for i := range 300 {
		f.seedStack(t, a.ID, fmt.Sprintf("a%03d", i), stacks.TargetSwarm)
	}
	for i := range 201 {
		f.seedStack(t, b.ID, fmt.Sprintf("b%03d", i), stacks.TargetSwarm)
	}

	got := answerOf[allStacksAnswer](t, f.allStacks(t, f.cookie, ""))
	shown := 0
	for _, g := range got.Groups {
		shown += len(g.Stacks)
	}
	if shown != 500 || got.Total != 501 || got.UnfilteredTotal != 501 || !got.Capped {
		t.Errorf("shown %d, total %d, unfiltered %d, capped %v, want 500, 501, 501, true", shown, got.Total, got.UnfilteredTotal, got.Capped)
	}
}

// The live name check of the Create stack dialog (plan 07-24, STK-01, D-12): it answers one environment's
// verdict on a name and writes nothing.

type nameCheckAnswer struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func nameCheck(t *testing.T, c *roleCaller, name string) (int, nameCheckAnswer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/name-check?name="+url.QueryEscape(name), "")
	var got nameCheckAnswer
	if rec.Code == http.StatusOK {
		got = answerOf[nameCheckAnswer](t, rec)
	}
	return rec.Code, got, rec
}

func TestNameCheckAnswersAvailableTakenClashAndInvalid(t *testing.T) {
	t.Parallel()
	foreign := dockertest.FakeService{ID: "s1", Name: "legacy_app", Mode: "replicated", Image: "x", Labels: map[string]string{"com.docker.stack.namespace": "legacy"}}
	f, e := stackEnv(t, nil, foreign)
	admin := f.caller(t, e, "envadmin-1", "admin")
	newStack(t, admin, "web")

	for _, c := range []struct {
		name, status, message string
	}{
		{"fresh", "available", ""},
		{"web", "taken", "A stack named web already exists on " + e.Name + "."},
		{"legacy", "clash", "A stack named legacy already exists on this environment, not managed by Bayplan."},
		{"Bad Name", "invalid", "Use lowercase letters, numbers, dash or underscore, and start with a letter or number."},
		{"-dash", "invalid", "Use lowercase letters, numbers, dash or underscore, and start with a letter or number."},
		{strings.Repeat("a", 64), "invalid", "Use lowercase letters, numbers, dash or underscore, and start with a letter or number."},
		{"", "invalid", "Use lowercase letters, numbers, dash or underscore, and start with a letter or number."},
	} {
		code, got, rec := nameCheck(t, admin, c.name)
		if code != http.StatusOK || got.Status != c.status || got.Message != c.message {
			t.Errorf("name-check %q = %d %s, want %s %q", c.name, code, rec.Body, c.status, c.message)
		}
	}
}

func TestNameCheckWritesNothingAnywhere(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	count := func(table string) int { return f.count(t, "SELECT COUNT(*) FROM "+table) }
	stacksBefore, auditBefore, snapshotsBefore := count("stacks"), count("audit_log"), count("stack_snapshots")

	for _, name := range []string{"fresh", "Bad Name", "fresh"} {
		if code, _, rec := nameCheck(t, admin, name); code != http.StatusOK {
			t.Fatalf("name-check %q: %d %s", name, code, rec.Body)
		}
	}

	if got := count("stacks"); got != stacksBefore {
		t.Errorf("stacks = %d after a name check, want %d", got, stacksBefore)
	}
	if got := count("stack_snapshots"); got != snapshotsBefore {
		t.Errorf("stack snapshots = %d after a name check, want %d", got, snapshotsBefore)
	}
	if got := count("audit_log"); got != auditBefore {
		t.Errorf("audit rows = %d after a name check, want %d: a check is not an action", got, auditBefore)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a name check must change nothing on an engine", w)
	}
}

func TestNameCheckNeedsTheRightToCreateStacksOnThatEnvironment(t *testing.T) {
	t.Parallel()
	f, a, b := twoStackEnvironments(t, nil)
	f.seedStack(t, b.ID, "edge-proxy", stacks.TargetSwarm)
	operator := f.caller(t, a, "operator-1", "operator")
	viewer := f.caller(t, a, "viewer-1", "viewer")

	for name, c := range map[string]*roleCaller{"an Operator": operator, "a Viewer": viewer} {
		if code, _, rec := nameCheck(t, c, "fresh"); code != http.StatusForbidden {
			t.Errorf("name-check as %s: %d %s, want 403", name, code, rec.Body)
		}
	}
	// An Admin of A learns nothing of B: it is not found, as every route of an environment is.
	admin := f.caller(t, a, "envadmin-1", "admin")
	probe := &roleCaller{f: f, t: t, e: b, cookie: admin.cookie, csrf: admin.csrf}
	if code, got, rec := nameCheck(t, probe, "edge-proxy"); code != http.StatusNotFound {
		t.Errorf("name-check on an environment the caller has no grant on: %d %s %+v, want 404", code, rec.Body, got)
	}
}
