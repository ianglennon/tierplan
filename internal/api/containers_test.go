// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// The first endpoint that changes Docker state (plan 04-01, RES-01, D-06, D-11, D-14,
// D-51, P2 D-42). Docker is a FakeDaemon behind the real monitor, snapshot cache and
// aggregator; the router, policy, CSRF guard and audit writer are the real ones.

const (
	webID   = "aaaaaaaaaaaa11111111111111111111111111111111111111111111111111"
	dbID    = "bbbbbbbbbbbb22222222222222222222222222222222222222222222222222"
	agentID = "cccccccccccc33333333333333333333333333333333333333333333333333"
	selfCtr = selfID + "ffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func actionEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: selfCtr, Names: []string{"/bayplan"}, Image: "bayplan:edge", State: "running", Status: "Up 1 hour"},
			{ID: webID, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: dbID, Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago"},
			{ID: agentID, Names: []string{"/bayplan-agent"}, Image: "bayplan-agent:edge", State: "running", Status: "Up 1 hour",
				Labels: map[string]string{"bayplan.agent": "true"}},
		},
	}
}

type actionResult struct {
	NodeID string `json:"nodeId"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	Result string `json:"result"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type actionBody struct {
	Results []actionResult `json:"results"`
}

func actionJSON(action string, targets ...[2]string) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf(`{"nodeId":%q,"id":%q}`, t[0], t[1]))
	}
	return fmt.Sprintf(`{"action":%q,"targets":[%s]}`, action, strings.Join(parts, ","))
}

func (f *envFix) act(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/containers/actions", body)
}

type auditRowFull struct {
	Action, TargetType, TargetID, EnvID, Result, Reason string
	Details                                             map[string]string
}

func (f *envFix) actionAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action IN ('container.started', 'container.stopped') ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestContainerActionStopRoutesByNodeIDAuditsOnceAndTheListRefreshes(t *testing.T) {
	f, e := dashEnv(t, actionEngine())

	// The list rows carry the node ID the write is routed by.
	list := decode[listBody[containerItem]](t, f.get(t, e, "containers"))
	var web containerItem
	for _, c := range list.Items {
		if c.Name == "web" {
			web = c
		}
	}
	if web.NodeID != e.ID || web.State != "running" {
		t.Fatalf("web row = %+v, want nodeId %q and running", web, e.ID)
	}

	rec := f.act(t, e, actionJSON("stop", [2]string{web.NodeID, web.ID}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s\n%s", rec.code, rec.body, f.logs)
	}
	got := decode[actionBody](t, rec)
	if len(got.Results) != 1 || got.Results[0].Result != "done" || got.Results[0].Name != "web" || got.Results[0].ID != webID {
		t.Fatalf("results = %+v", got.Results)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /containers/" + webID + "/stop"}) {
		t.Fatalf("the daemon saw %v", w)
	}

	// The snapshot was invalidated: the very next read is fresh.
	after := decode[listBody[containerItem]](t, f.get(t, e, "containers"))
	for _, c := range after.Items {
		if c.Name == "web" && c.State != "exited" {
			t.Fatalf("after the stop the list still says %s", c.State)
		}
	}

	rows := f.actionAudits(t)
	if len(rows) != 1 {
		t.Fatalf("%d audit rows, want exactly one: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Action != audit.ActionContainerStopped || r.TargetType != "container" || r.TargetID != webID || r.EnvID != e.ID || r.Result != "success" {
		t.Errorf("audit row = %+v", r)
	}
	if r.Details["container"] != "web" || r.Details["node"] != e.ID || r.Details["code"] != "done" {
		t.Errorf("audit details = %v, want container web, node %s, code done", r.Details, e.ID)
	}
}

func TestContainerActionStartAnExitedContainerNeedsNoConfirmationAndIsAudited(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	rec := f.act(t, e, actionJSON("start", [2]string{e.ID, "db"}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec)
	if got.Results[0].Result != "done" || got.Results[0].Name != "db" {
		t.Fatalf("results = %+v", got.Results)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /containers/" + dbID + "/start"}) {
		t.Fatalf("the daemon saw %v (the container is addressed by its full ID once inspected)", w)
	}
	if rows := f.actionAudits(t); len(rows) != 1 || rows[0].Action != audit.ActionContainerStarted || rows[0].TargetID != "db" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestContainerActionAnUnknownNodeIsNodeUnreachableAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	// "node-1" is the display name of the host: a name never routes a write.
	for _, node := range []string{"nope", "node-1"} {
		rec := f.act(t, e, actionJSON("stop", [2]string{node, webID}))
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", node, rec.code, rec.body)
		}
		res := decode[actionBody](t, rec).Results[0]
		if res.Result != "failed" || res.Code != "node_unreachable" {
			t.Fatalf("%s: result = %+v, want failed node_unreachable", node, res)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a node that is not the environment's received %v", w)
	}
	rows := f.actionAudits(t)
	if len(rows) != 2 || rows[0].Result != "failure" || rows[0].Reason != "node_unreachable" || rows[0].Details["code"] != "node_unreachable" {
		t.Fatalf("a refused target must be audited with its code: %+v", rows)
	}
}

func TestContainerActionBayplansOwnContainersAreRefusedFromAFreshInspect(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	// The server's own container, named by a short prefix of its ID, and an agent
	// container, found by its label: neither the list nor the client is trusted.
	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, selfID}, [2]string{e.ID, "bayplan-agent"}, [2]string{e.ID, selfCtr}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	for i, res := range decode[actionBody](t, rec).Results {
		if res.Result != "failed" || res.Code != "own_container" {
			t.Errorf("target %d: %+v, want failed own_container", i, res)
		}
	}
	for _, w := range f.fake.Writes() {
		t.Errorf("an own container was written to: %s", w)
	}
	if rows := f.actionAudits(t); len(rows) != 3 || rows[0].Result != "failure" || rows[0].Reason != "own_container" {
		t.Fatalf("audit = %+v, want three failure rows", rows)
	}
}

func TestContainerActionAnUnknownContainerIsNotFound(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, "ghost"}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "failed" || res.Code != "not_found" {
		t.Fatalf("result = %+v, want failed not_found", res)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestContainerActionEachTargetIsAnsweredInOrderAndOneFailureDoesNotStopTheRest(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, "ghost"}, [2]string{e.ID, webID}, [2]string{e.ID, "bayplan-agent"}))
	got := decode[actionBody](t, rec).Results
	if len(got) != 3 || got[0].ID != "ghost" || got[1].ID != webID || got[2].ID != "bayplan-agent" {
		t.Fatalf("results are not in request order: %+v", got)
	}
	want := []string{"failed/not_found", "done/", "failed/own_container"}
	for i, r := range got {
		if r.Result+"/"+r.Code != want[i] {
			t.Errorf("target %d = %s/%s, want %s", i, r.Result, r.Code, want[i])
		}
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /containers/" + webID + "/stop"}) {
		t.Fatalf("writes = %v", w)
	}
	if rows := f.actionAudits(t); len(rows) != 3 {
		t.Fatalf("%d audit rows for 3 targets: %+v", len(rows), rows)
	}
}

func TestContainerActionTheSchemaRefusesBadBodies(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	many := make([][2]string, 26)
	for i := range many {
		many[i] = [2]string{e.ID, "web"}
	}
	for name, body := range map[string]string{
		"no targets":       `{"action":"stop","targets":[]}`,
		"26 targets":       actionJSON("stop", many...),
		"restart":          actionJSON("restart", [2]string{e.ID, "web"}),
		"an unknown field": `{"action":"stop","targets":[{"nodeId":"x","id":"web"}],"force":true}`,
		"a path in an id":  actionJSON("stop", [2]string{e.ID, "../../etc"}),
		"an empty node":    actionJSON("stop", [2]string{"", "web"}),
	} {
		rec := f.act(t, e, body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body sent %v", w)
	}
}

func TestContainerActionAViewerIsRefusedAndAnOperatorIsNot(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")

	vCookie, vCSRF := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/containers/actions",
		body: actionJSON("stop", [2]string{e.ID, "web"}), cookie: vCookie, csrf: vCSRF})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 || len(f.actionAudits(t)) != 0 {
		t.Fatalf("a refused viewer reached the engine or the container audit: %v", w)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/containers/actions" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}

	oCookie, oCSRF := f.signIn(t, "operator", false)
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/containers/actions",
		body: actionJSON("stop", [2]string{e.ID, "web"}), cookie: oCookie, csrf: oCSRF})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator: status = %d: %s", rec.Code, rec.Body)
	}

	// No CSRF token: the router's mutation guard refuses it before the handler.
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/containers/actions",
		body: actionJSON("start", [2]string{e.ID, "db"}), cookie: oCookie})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no CSRF token: status = %d, want 403", rec.Code)
	}
}

func TestContainerActionAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, actionEngine())
	rec := f.req(t, http.MethodPost, "/api/env/nope/containers/actions", actionJSON("stop", [2]string{"x", "web"}))
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("%d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}

// A remote node is refused by name before any call when its agent reported a read-only
// host, and an agent that refuses by its own policy is a different answer (plan 04-03,
// D-35, Pitfall 3, Pitfall 13).

func (f *agentFix) actOn(t *testing.T, envID, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+envID+"/containers/actions", body)
}

func TestContainerActionOnAReadOnlyHostIsHostReadOnlyAuditedAndNothingIsSent(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Locked")
	a := f.connect(t, env, actionEngine(), &wire.Facts{
		EngineID: "ENG:ro", Hostname: "node-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true},
	})
	f.whenState(t, env.ID, "online")

	rec := f.actOn(t, env.ID, actionJSON("stop", [2]string{a.id, webID}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec)
	if len(got.Results) != 1 || got.Results[0].Result != "failed" || got.Results[0].Code != "host_read_only" {
		t.Fatalf("results = %+v, want failed with host_read_only", got.Results)
	}
	if w := a.fake.Writes(); len(w) != 0 {
		t.Fatalf("a read-only host received %v", w)
	}
	rows := f.actionAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "host_read_only" || rows[0].Details["code"] != "host_read_only" {
		t.Fatalf("audit rows = %+v, want one failure with reason host_read_only", rows)
	}
}

func TestContainerActionAnAgentThatRefusesByPolicyIsAgentPolicyDenied(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Old agent")
	cfg := actionEngine()
	cfg.PolicyDeny = true
	a := f.connect(t, env, cfg, nil)
	f.whenState(t, env.ID, "online")

	rec := f.actOn(t, env.ID, actionJSON("stop", [2]string{a.id, webID}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec)
	if len(got.Results) != 1 || got.Results[0].Result != "failed" || got.Results[0].Code != "agent_policy_denied" {
		t.Fatalf("results = %+v, want failed with agent_policy_denied", got.Results)
	}
	rows := f.actionAudits(t)
	if len(rows) != 1 || rows[0].Reason != "agent_policy_denied" {
		t.Fatalf("audit rows = %+v, want one failure with reason agent_policy_denied", rows)
	}
}
