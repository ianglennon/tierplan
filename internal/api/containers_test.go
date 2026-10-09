// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
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
	BulkID  string         `json:"bulkId"`
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
	// A write goes only to a node the monitor has seen online, so wait for its
	// first probe: without it a fast run reads the node as offline.
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/containers/actions", body)
}

type auditRowFull struct {
	Action, TargetType, TargetID, EnvID, Result, Reason string
	Details                                             map[string]string
}

func (f *envFix) actionAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'container.%' ORDER BY seq`)
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

func TestContainerActionBayplansOwnContainersAreSkippedInABulkFromAFreshInspect(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	// The server's own container, named by a short prefix of its ID, and an agent
	// container, found by its label: neither the list nor the client is trusted.
	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, selfID}, [2]string{e.ID, "bayplan-agent"}, [2]string{e.ID, selfCtr}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	for i, res := range decode[actionBody](t, rec).Results {
		if res.Result != "skipped" || res.Code != "own_container" {
			t.Errorf("target %d: %+v, want skipped own_container", i, res)
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
	want := []string{"failed/not_found", "done/", "skipped/own_container"}
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
		"no targets":          `{"action":"stop","targets":[]}`,
		"26 targets":          actionJSON("stop", many...),
		"an unknown verb":     actionJSON("exec", [2]string{e.ID, "web"}),
		"a lower-case signal": strings.Replace(actionJSON("kill", [2]string{e.ID, "web"}), `{"action"`, `{"signal":"sigterm","action"`, 1),
		"a negative timeout":  strings.Replace(actionJSON("stop", [2]string{e.ID, "web"}), `{"action"`, `{"timeout":-1,"action"`, 1),
		"a day and a second":  strings.Replace(actionJSON("stop", [2]string{e.ID, "web"}), `{"action"`, `{"timeout":86401,"action"`, 1),
		"an unknown field":    `{"action":"stop","targets":[{"nodeId":"x","id":"web"}],"force":true}`,
		"a path in an id":     actionJSON("stop", [2]string{e.ID, "../../etc"}),
		"an empty node":       actionJSON("stop", [2]string{"", "web"}),
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

// Bulk requests (plan 04-05, D-13, D-14): up to 25 targets, one result and one audit row
// each, a shared bulk ID, bounded parallelism per node.

const (
	cacheID = "dddddddddddd44444444444444444444444444444444444444444444444444"
)

func bulkEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Containers: []dockertest.FakeContainer{
			{ID: webID, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
			{ID: cacheID, Names: []string{"/cache"}, Image: "redis:8", State: "running", Status: "Up 2 hours"},
			{ID: dbID, Names: []string{"/db"}, Image: "postgres:17", State: "exited", Status: "Exited (0) 3 hours ago"},
		},
	}
}

func TestBulkStopAnswersInRequestOrderAndEveryAuditRowSharesTheServersBulkID(t *testing.T) {
	f, e := dashEnv(t, bulkEngine())
	rec := f.act(t, e, actionJSON("stop", [2]string{e.ID, webID}, [2]string{e.ID, cacheID}, [2]string{e.ID, "ghost"}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s\n%s", rec.code, rec.body, f.logs)
	}
	got := decode[actionBody](t, rec)
	want := []string{"done/", "done/", "failed/not_found"}
	if len(got.Results) != 3 {
		t.Fatalf("results = %+v", got.Results)
	}
	for i, r := range got.Results {
		if r.Result+"/"+r.Code != want[i] {
			t.Errorf("target %d = %s/%s, want %s", i, r.Result, r.Code, want[i])
		}
	}
	id, err := uuid.Parse(got.BulkID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("bulkId = %q (%v), want a UUIDv7 the server made", got.BulkID, err)
	}
	rows := f.actionAudits(t)
	if len(rows) != 3 {
		t.Fatalf("%d audit rows for 3 targets: %+v", len(rows), rows)
	}
	for i, r := range rows {
		if r.Details["bulk"] != got.BulkID {
			t.Errorf("row %d bulk = %q, want %q", i, r.Details["bulk"], got.BulkID)
		}
	}
	if rows[2].Result != "failure" || rows[2].Reason != "not_found" {
		t.Errorf("the unknown container's row = %+v", rows[2])
	}
}

func TestBulkIDSentByTheClientIsUsedVerbatimAndANonUUIDIsRefused(t *testing.T) {
	f, e := dashEnv(t, bulkEngine())
	const sent = "0190a0a0-1b2c-7d3e-8f40-5a6b7c8d9e0f"
	body := strings.Replace(actionJSON("stop", [2]string{e.ID, webID}, [2]string{e.ID, cacheID}), `{"action"`, `{"bulkId":"`+sent+`","action"`, 1)
	rec := f.act(t, e, body)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got := decode[actionBody](t, rec); got.BulkID != sent {
		t.Fatalf("bulkId = %q, want the one sent", got.BulkID)
	}
	for i, r := range f.actionAudits(t) {
		if r.Details["bulk"] != sent {
			t.Errorf("row %d bulk = %q", i, r.Details["bulk"])
		}
	}

	before := len(f.fake.Writes())
	bad := strings.Replace(actionJSON("stop", [2]string{e.ID, dbID}), `{"action"`, `{"bulkId":"not-a-uuid","action"`, 1)
	if rec := f.act(t, e, bad); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("a bulkId that is not a UUID: status = %d, want 422: %s", rec.code, rec.body)
	}
	if after := len(f.fake.Writes()); after != before {
		t.Fatalf("a refused body sent %d writes", after-before)
	}
}

func TestBulkOfNoTargetsOrMoreThan25IsRefusedBeforeAnyDockerCall(t *testing.T) {
	f, e := dashEnv(t, bulkEngine())
	many := make([][2]string, 26)
	for i := range many {
		many[i] = [2]string{e.ID, "web"}
	}
	for name, body := range map[string]string{
		"zero targets": `{"action":"stop","targets":[]}`,
		"26 targets":   actionJSON("stop", many...),
	} {
		if rec := f.act(t, e, body); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body sent %v", w)
	}
	if rows := f.actionAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited: %+v", rows)
	}
}

func TestBulkRunsAtMostFourCallsAtOnceOnOneNode(t *testing.T) {
	cfg := dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", WriteDelay: 150 * time.Millisecond}
	var targets [][2]string
	for i := range 12 {
		id := fmt.Sprintf("%012x%052x", i+1, i+1)
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{ID: id, Names: []string{fmt.Sprintf("/c%d", i)}, Image: "nginx:1.27", State: "running", Status: "Up 1 hour"})
		targets = append(targets, [2]string{"", id})
	}
	f, e := dashEnv(t, cfg)
	for i := range targets {
		targets[i][0] = e.ID
	}
	rec := f.act(t, e, actionJSON("stop", targets...))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	for i, r := range decode[actionBody](t, rec).Results {
		if r.Result != "done" {
			t.Errorf("target %d = %+v", i, r)
		}
	}
	if got := f.fake.MaxConcurrentWrites(); got < 2 || got > 4 {
		t.Fatalf("the node served up to %d writes at once, want between 2 (it ran in parallel) and 4 (the per-node limit)", got)
	}
	if rows := f.actionAudits(t); len(rows) != 12 {
		t.Fatalf("%d audit rows for 12 targets", len(rows))
	}
}

func TestBulkATargetOnANodeThatIsNotConnectedIsNodeUnreachableAndTheOthersStillRun(t *testing.T) {
	f, e := dashEnv(t, bulkEngine())
	rec := f.act(t, e, actionJSON("stop", [2]string{"elsewhere", webID}, [2]string{e.ID, cacheID}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec).Results
	if got[0].Result != "failed" || got[0].Code != "node_unreachable" || got[1].Result != "done" {
		t.Fatalf("results = %+v", got)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /containers/" + cacheID + "/stop"}) {
		t.Fatalf("the daemon saw %v, want only the connected node's target", w)
	}
}

// All six verbs, the single-target options, and container remove (plan 04-05, D-06 to
// D-09, D-60, RES-01).

func (f *envFix) remove(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/containers/remove", body)
}

func removeJSON(force, volumes bool, targets ...[2]string) string {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf(`{"nodeId":%q,"id":%q}`, t[0], t[1]))
	}
	return fmt.Sprintf(`{"force":%t,"removeVolumes":%t,"targets":[%s]}`, force, volumes, strings.Join(parts, ","))
}

// withOptions adds timeout and signal to an actions body.
func withOptions(body string, timeout int, signal string) string {
	extra := ""
	if timeout >= 0 {
		extra += fmt.Sprintf(`"timeout":%d,`, timeout)
	}
	if signal != "" {
		extra += fmt.Sprintf(`"signal":%q,`, signal)
	}
	return strings.Replace(body, `{"action"`, `{`+extra+`"action"`, 1)
}

func TestContainerActionRestartKillPauseAndUnpauseReachTheEngineAsTheirOwnCalls(t *testing.T) {
	for _, tc := range []struct {
		verb, container, id, want string
		wantAudit                 string
	}{
		{"restart", "web", webID, "POST /containers/" + webID + "/restart", audit.ActionContainerRestarted},
		{"kill", "web", webID, "POST /containers/" + webID + "/kill", audit.ActionContainerKilled},
		{"pause", "web", webID, "POST /containers/" + webID + "/pause", audit.ActionContainerPaused},
		{"unpause", "web", webID, "POST /containers/" + webID + "/unpause", audit.ActionContainerUnpaused},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			f, e := dashEnv(t, actionEngine())
			rec := f.act(t, e, actionJSON(tc.verb, [2]string{e.ID, tc.container}))
			if rec.code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.code, rec.body)
			}
			if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" {
				t.Fatalf("result = %+v", res)
			}
			if w := f.fake.Writes(); !slices.Equal(w, []string{tc.want}) {
				t.Fatalf("the daemon saw %v, want [%s]", w, tc.want)
			}
			rows := f.actionAudits(t)
			if len(rows) != 1 || rows[0].Action != tc.wantAudit || rows[0].Result != "success" {
				t.Fatalf("audit = %+v, want one %s", rows, tc.wantAudit)
			}
		})
	}
}

func TestContainerActionAStopOrKillWithOptionsSendsThemAndWithoutSendsNeither(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"stop with no option", actionJSON("stop", [2]string{"", "web"}), "POST /containers/" + webID + "/stop"},
		{"stop with a timeout", withOptions(actionJSON("stop", [2]string{"", "web"}), 5, ""), "POST /containers/" + webID + "/stop?t=5"},
		{"stop with a zero timeout", withOptions(actionJSON("stop", [2]string{"", "web"}), 0, ""), "POST /containers/" + webID + "/stop?t=0"},
		{"kill with a signal", withOptions(actionJSON("kill", [2]string{"", "web"}), -1, "SIGTERM"), "POST /containers/" + webID + "/kill?signal=SIGTERM"},
		{"kill with no option", actionJSON("kill", [2]string{"", "web"}), "POST /containers/" + webID + "/kill"},
		{"restart with both", withOptions(actionJSON("restart", [2]string{"", "web"}), 3, "SIGINT"), "POST /containers/" + webID + "/restart?signal=SIGINT&t=3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, e := dashEnv(t, actionEngine())
			rec := f.act(t, e, strings.Replace(tc.body, `"nodeId":""`, `"nodeId":"`+e.ID+`"`, 1))
			if rec.code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.code, rec.body)
			}
			if w := f.fake.Writes(); !slices.Equal(w, []string{tc.want}) {
				t.Fatalf("the daemon saw %v, want [%s]", w, tc.want)
			}
		})
	}
}

func TestContainerActionOptionsWithMoreThanOneTargetAreRefusedAsSingleOnly(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	two := actionJSON("stop", [2]string{e.ID, "web"}, [2]string{e.ID, "db"})
	for name, body := range map[string]string{
		"a timeout": withOptions(two, 5, ""),
		"a signal":  withOptions(two, -1, "SIGTERM"),
	} {
		rec := f.act(t, e, body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "options_single_only" {
			t.Errorf("%s: %d %q, want 422 options_single_only: %s", name, rec.code, rec.ptyp, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body sent %v", w)
	}
	if rows := f.actionAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited: %+v", rows)
	}
}

func TestContainerActionOptionsAreRecordedInTheAuditRowOfTheVerbsThatUseThem(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	if rec := f.act(t, e, withOptions(actionJSON("stop", [2]string{e.ID, "web"}), 7, "SIGQUIT")); rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	rows := f.actionAudits(t)
	if len(rows) != 1 || rows[0].Details["timeout"] != "7" || rows[0].Details["signal"] != "SIGQUIT" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestContainerActionRepeatingAnActionIsAnsweredDoneAndARemovedContainerIsNotFound(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	// Start of a running container and stop of a stopped one are Docker's 304.
	for _, tc := range []struct{ action, id string }{{"start", "web"}, {"stop", "db"}} {
		res := decode[actionBody](t, f.act(t, e, actionJSON(tc.action, [2]string{e.ID, tc.id}))).Results[0]
		if res.Result != "done" {
			t.Errorf("%s %s = %+v, want done", tc.action, tc.id, res)
		}
	}
	// A container removed meanwhile is not_found, for an action and for a second remove.
	if res := decode[actionBody](t, f.remove(t, e, removeJSON(false, false, [2]string{e.ID, "db"}))).Results[0]; res.Result != "done" {
		t.Fatalf("first remove = %+v", res)
	}
	if res := decode[actionBody](t, f.remove(t, e, removeJSON(false, false, [2]string{e.ID, "db"}))).Results[0]; res.Code != "not_found" {
		t.Fatalf("second remove = %+v, want not_found", res)
	}
	if res := decode[actionBody](t, f.act(t, e, actionJSON("start", [2]string{e.ID, "db"}))).Results[0]; res.Code != "not_found" {
		t.Fatalf("start after remove = %+v, want not_found", res)
	}
}

func TestContainerRemoveSendsNeitherForceNorVolumesByDefaultAndEachWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name          string
		force, volume bool
		id, want      string
	}{
		{"neither", false, false, "db", "DELETE /containers/" + dbID},
		{"force", true, false, "web", "DELETE /containers/" + webID + "?force=1"},
		{"volumes", false, true, "db", "DELETE /containers/" + dbID + "?v=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, e := dashEnv(t, actionEngine())
			rec := f.remove(t, e, removeJSON(tc.force, tc.volume, [2]string{e.ID, tc.id}))
			if rec.code != http.StatusOK {
				t.Fatalf("status = %d: %s\n%s", rec.code, rec.body, f.logs)
			}
			if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" {
				t.Fatalf("result = %+v", res)
			}
			if w := f.fake.Writes(); !slices.Equal(w, []string{tc.want}) {
				t.Fatalf("the daemon saw %v, want [%s]", w, tc.want)
			}
			rows := f.actionAudits(t)
			if len(rows) != 1 || rows[0].Action != audit.ActionContainerRemoved ||
				rows[0].Details["force"] != fmt.Sprint(tc.force) || rows[0].Details["volumes"] != fmt.Sprint(tc.volume) {
				t.Fatalf("audit = %+v", rows)
			}
		})
	}
}

func TestContainerRemoveOfARunningContainerWithoutForceIsContainerRunningAndSendsNothing(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	rec := f.remove(t, e, removeJSON(false, false, [2]string{e.ID, "web"}, [2]string{e.ID, "db"}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[actionBody](t, rec).Results
	if got[0].Result != "failed" || got[0].Code != "container_running" || got[1].Result != "done" {
		t.Fatalf("results = %+v", got)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /containers/" + dbID}) {
		t.Fatalf("the daemon saw %v, want only the exited container's remove", w)
	}
	rows := f.actionAudits(t)
	if len(rows) != 2 || rows[0].Result != "failure" || rows[0].Reason != "container_running" {
		t.Fatalf("audit = %+v", rows)
	}
}

func TestContainerRemoveAskingForVolumesNeedsTheDeleteVolumesExtra(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "plain", false)
	f.addEnvRole(t, "plain", e.ID, "operator")
	f.addUser(t, "trusted", false)
	f.addEnvRole(t, "trusted", e.ID, "operator")
	if _, err := f.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'volumes.delete')`, "ra-trusted-"+e.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/env/" + e.ID + "/containers/remove"
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	as := func(user, body string) *httptest.ResponseRecorder {
		cookie, csrf := f.signIn(t, user, false)
		return f.call(t, authReq{method: http.MethodPost, path: path, body: body, cookie: cookie, csrf: csrf})
	}

	// A Viewer is refused by the route policy, whatever the body.
	if rec := as("viewer", removeJSON(false, false, [2]string{e.ID, "db"})); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: status = %d, want 403: %s", rec.Code, rec.Body)
	}

	// An Operator without the extra may remove, but not with volumes.
	if rec := as("plain", removeJSON(false, false, [2]string{e.ID, "db"})); rec.Code != http.StatusOK {
		t.Fatalf("operator, no volumes: status = %d: %s", rec.Code, rec.Body)
	}
	before := len(f.fake.Writes())
	rec := as("plain", removeJSON(false, true, [2]string{e.ID, "web"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator, volumes: status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Removing anonymous volumes needs the delete volumes permission") {
		t.Errorf("body = %s", rec.Body)
	}
	if after := len(f.fake.Writes()); after != before {
		t.Fatalf("a refused request sent %d writes", after-before)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	last := denied[len(denied)-1]
	if last.Reason != "forbidden" || last.TargetID != "POST /api/env/{envId}/containers/remove" {
		t.Fatalf("the refusal = %+v", last)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(details, '{}') FROM audit_log WHERE action = ? ORDER BY seq DESC LIMIT 1`, audit.ActionPermissionDenied).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"permission":"volumes.delete"`) {
		t.Errorf("details = %s, want permission volumes.delete", details)
	}
	for _, r := range f.actionAudits(t) {
		if r.Details["volumes"] == "true" {
			t.Errorf("a refused request left a container.removed row: %+v", r)
		}
	}

	// With the extra it goes through.
	if rec := as("trusted", removeJSON(true, true, [2]string{e.ID, "web"})); rec.Code != http.StatusOK {
		t.Fatalf("operator with the extra: status = %d: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); !slices.Contains(w, "DELETE /containers/"+webID+"?force=1&v=1") {
		t.Fatalf("the daemon saw %v", w)
	}
}

func TestContainerActionsOnTwoNodesWithSameNamedContainersActOnExactlyTheNodeNamed(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	const alphaWeb = "1111111111111111111111111111111111111111111111111111111111111111"
	const bravoWeb = "2222222222222222222222222222222222222222222222222222222222222222"
	box := func(name, id string) dockertest.FakeConfig {
		return dockertest.FakeConfig{Hostname: name, SwarmState: "active", SwarmControl: true, Containers: []dockertest.FakeContainer{
			{ID: id, Names: []string{"/web"}, Image: "nginx:1.27", State: "running", Status: "Up 2 hours"},
		}}
	}
	alpha := f.connect(t, env, box("alpha", alphaWeb), swarmNode("alpha", true))
	bravo := f.connect(t, env, box("bravo", bravoWeb), swarmNode("bravo", true))
	f.whenState(t, env.ID, "online")

	rec := f.actOn(t, env.ID, actionJSON("stop", [2]string{bravo.id, "web"}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" || res.NodeID != bravo.id {
		t.Fatalf("result = %+v", res)
	}
	if w := bravo.fake.Writes(); !slices.Equal(w, []string{"POST /containers/" + bravoWeb + "/stop"}) {
		t.Fatalf("bravo saw %v", w)
	}
	if w := alpha.fake.Writes(); len(w) != 0 {
		t.Fatalf("a container of the same name on another node received %v", w)
	}

	// Both at once: each node receives exactly its own call, and the results keep the order sent.
	rec = f.req(t, http.MethodPost, "/api/env/"+env.ID+"/containers/remove",
		removeJSON(true, false, [2]string{alpha.id, "web"}, [2]string{bravo.id, "web"}))
	got := decode[actionBody](t, rec).Results
	if len(got) != 2 || got[0].NodeID != alpha.id || got[1].NodeID != bravo.id || got[0].Result != "done" || got[1].Result != "done" {
		t.Fatalf("results = %+v", got)
	}
	if w := alpha.fake.Writes(); !slices.Equal(w, []string{"DELETE /containers/" + alphaWeb + "?force=1"}) {
		t.Fatalf("alpha saw %v", w)
	}
}

// Bayplan's own containers (plan 04-05, D-11, D-12, RESEARCH Pitfall 10): stopping the
// server or an agent cuts Bayplan off from a host, so one at a time it needs the name
// typed, in a bulk it is never reached, and the attempt is recorded before the call.

func withConfirm(body, confirm string) string {
	return strings.Replace(body, `{"`, `{"confirm":`+fmt.Sprintf("%q", confirm)+`,"`, 1)
}

func TestOwnContainerSingleTargetNeedsItsNameTypedAndNothingIsSentWithout(t *testing.T) {
	for _, tc := range []struct{ name, ref, container string }{
		{"the server", selfID, "bayplan"},
		{"an agent", "bayplan-agent", "bayplan-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, e := dashEnv(t, actionEngine())
			one := actionJSON("stop", [2]string{e.ID, tc.ref})
			for label, body := range map[string]string{"no confirm": one, "a wrong name": withConfirm(one, "yes"), "the id": withConfirm(one, selfCtr)} {
				rec := f.act(t, e, body)
				if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
					t.Errorf("%s: %d %q, want 422 confirmation_required: %s", label, rec.code, rec.ptyp, rec.body)
				}
			}
			if w := f.fake.Writes(); len(w) != 0 {
				t.Fatalf("an unconfirmed request sent %v", w)
			}
			if rows := f.actionAudits(t); len(rows) != 0 {
				t.Fatalf("a refused request was audited: %+v", rows)
			}

			rec := f.act(t, e, withConfirm(one, tc.container))
			if rec.code != http.StatusOK {
				t.Fatalf("confirmed: status = %d: %s", rec.code, rec.body)
			}
			if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" || res.Name != tc.container {
				t.Fatalf("confirmed result = %+v", res)
			}
			if w := f.fake.Writes(); len(w) != 1 {
				t.Fatalf("the daemon saw %v, want exactly the confirmed stop", w)
			}
		})
	}
}

func TestOwnContainerEveryDisruptiveVerbNeedsTheNameAndStartAndUnpauseDoNot(t *testing.T) {
	for _, verb := range []string{"stop", "kill", "restart", "pause"} {
		f, e := dashEnv(t, actionEngine())
		rec := f.act(t, e, actionJSON(verb, [2]string{e.ID, "bayplan-agent"}))
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("%s: %d %q, want 422 confirmation_required", verb, rec.code, rec.ptyp)
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("%s sent %v", verb, w)
		}
	}
	rec := func() *envRec {
		f, e := dashEnv(t, actionEngine())
		r := f.remove(t, e, removeJSON(true, false, [2]string{e.ID, "bayplan-agent"}))
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("remove sent %v", w)
		}
		return r
	}()
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
		t.Errorf("remove: %d %q, want 422 confirmation_required", rec.code, rec.ptyp)
	}

	for _, verb := range []string{"start", "unpause"} {
		f, e := dashEnv(t, actionEngine())
		rec := f.act(t, e, actionJSON(verb, [2]string{e.ID, "bayplan-agent"}))
		if rec.code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", verb, rec.code, rec.body)
		}
		if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" {
			t.Errorf("%s of an own container = %+v, want done with no confirmation", verb, res)
		}
	}
}

func TestOwnContainerInAMultiTargetRequestIsSkippedNeverActedOnAndTheOthersRun(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	for _, tc := range []struct {
		name string
		rec  func() *envRec
	}{
		{"stop", func() *envRec {
			return f.act(t, e, actionJSON("stop", [2]string{e.ID, "web"}, [2]string{e.ID, selfID}, [2]string{e.ID, "bayplan-agent"}))
		}},
		{"remove", func() *envRec {
			return f.remove(t, e, removeJSON(true, false, [2]string{e.ID, selfID}, [2]string{e.ID, "db"}, [2]string{e.ID, "bayplan-agent"}))
		}},
	} {
		got := decode[actionBody](t, tc.rec()).Results
		var skipped, done int
		for _, r := range got {
			switch {
			case r.Result == "skipped" && r.Code == "own_container":
				skipped++
			case r.Result == "done":
				done++
			default:
				t.Errorf("%s: unexpected result %+v", tc.name, r)
			}
		}
		if skipped != 2 || done != 1 {
			t.Errorf("%s: %d skipped and %d done, want 2 and 1: %+v", tc.name, skipped, done, got)
		}
	}
	for _, w := range f.fake.Writes() {
		if strings.Contains(w, selfCtr) || strings.Contains(w, agentID) {
			t.Errorf("a bulk reached an own container: %s", w)
		}
	}
	// A skipped target is audited once, as a refusal with its code, and a confirm cannot make
	// a bulk act on it.
	rows := f.actionAudits(t)
	if len(rows) != 6 {
		t.Fatalf("%d audit rows for 6 targets: %+v", len(rows), rows)
	}
	rec := f.act(t, e, withConfirm(actionJSON("stop", [2]string{e.ID, "web"}, [2]string{e.ID, selfID}), "bayplan"))
	if rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("confirm with two targets: status = %d, want 422: %s", rec.code, rec.body)
	}
}

func TestOwnContainerAnAttemptIsRecordedAsInitiatedBeforeTheCallAndCompletedAfter(t *testing.T) {
	f, e := dashEnv(t, actionEngine())
	rec := f.act(t, e, withConfirm(actionJSON("restart", [2]string{e.ID, "bayplan-agent"}), "bayplan-agent"))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	rows := f.actionAudits(t)
	if len(rows) != 2 {
		t.Fatalf("%d audit rows, want an initiated row and a completed row: %+v", len(rows), rows)
	}
	if rows[0].Details["stage"] != "initiated" || rows[0].Details["own"] != "agent" || rows[0].Action != audit.ActionContainerRestarted {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].Details["stage"] != "completed" || rows[1].Details["own"] != "agent" || rows[1].Result != "success" || rows[1].Details["code"] != "done" {
		t.Errorf("second row = %+v", rows[1])
	}
	// The ordinary target has one row, and no stage.
	f2, e2 := dashEnv(t, actionEngine())
	f2.act(t, e2, actionJSON("stop", [2]string{e2.ID, "web"}))
	if rows := f2.actionAudits(t); len(rows) != 1 || rows[0].Details["stage"] != "" {
		t.Fatalf("an ordinary target: %+v", rows)
	}
}

func TestOwnContainerADroppedConnectionIsRecordedAsConnectionLostWithResultSuccess(t *testing.T) {
	cfg := actionEngine()
	cfg.DropWrites = true
	f, e := dashEnv(t, cfg)
	rec := f.act(t, e, withConfirm(actionJSON("stop", [2]string{e.ID, "bayplan-agent"}), "bayplan-agent"))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" || res.Code != "connection_lost" {
		t.Fatalf("result = %+v, want done with connection_lost", res)
	}
	if w := f.fake.Writes(); len(w) != 1 {
		t.Fatalf("the daemon saw %v, want the one call, never repeated", w)
	}
	rows := f.actionAudits(t)
	if len(rows) != 2 || rows[0].Details["stage"] != "initiated" || rows[1].Details["stage"] != "connection_lost" || rows[1].Result != "success" {
		t.Fatalf("audit = %+v, want initiated then connection_lost with result success", rows)
	}

	// The same dropped connection on a container that is not Bayplan's is a failure: the
	// action may not have happened and the caller must see that.
	rec = f.act(t, e, actionJSON("stop", [2]string{e.ID, "web"}))
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "failed" {
		t.Fatalf("an ordinary container with a dropped connection = %+v, want failed", res)
	}
}

func TestOwnContainerADroppedAgentConnectionIsRecordedAsConnectionLost(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	cfg := actionEngine()
	cfg.DropWrites = true
	a := f.connect(t, env, cfg, nil)
	f.whenState(t, env.ID, "online")

	rec := f.actOn(t, env.ID, withConfirm(actionJSON("stop", [2]string{a.id, "bayplan-agent"}), "bayplan-agent"))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if res := decode[actionBody](t, rec).Results[0]; res.Result != "done" || res.Code != "connection_lost" {
		t.Fatalf("result = %+v, want done with connection_lost", res)
	}
	rows := f.actionAudits(t)
	if len(rows) != 2 || rows[1].Details["stage"] != "connection_lost" || rows[1].Result != "success" || rows[1].Details["own"] != "agent" {
		t.Fatalf("audit = %+v", rows)
	}
}

// The container detail (plan 04-10, RES-01, D-04, P3 D-15a, T-04-40). Values of
// variables and of log-driver options are masked on the server unless the caller
// holds secrets.reveal.

type detailBody struct {
	Node      string `json:"node"`
	NodeID    string `json:"nodeId"`
	ReadOnly  bool   `json:"readOnly"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Health    string `json:"health"`
	ExitCode  int    `json:"exitCode"`
	Command   []string
	Masked    bool `json:"masked"`
	IsSelf    bool `json:"isSelf"`
	SelfKind  string
	Project   string `json:"project"`
	Stack     string `json:"stack"`
	Service   string `json:"service"`
	Restart   string `json:"restartPolicy"`
	Retries   int    `json:"restartMaxRetries"`
	StartedAt string `json:"startedAt"`
	Env       []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Masked bool   `json:"masked"`
	} `json:"env"`
	LogConfig struct {
		Type    string `json:"type"`
		Options []struct {
			Name   string `json:"name"`
			Value  string `json:"value"`
			Masked bool   `json:"masked"`
		} `json:"options"`
	} `json:"logConfig"`
	Ports []struct {
		ContainerPort int    `json:"containerPort"`
		Protocol      string `json:"protocol"`
		HostPort      string `json:"hostPort"`
	} `json:"ports"`
	Mounts []struct {
		Type, Destination, Mode string
	} `json:"mounts"`
	Networks []struct {
		Name    string   `json:"name"`
		Aliases []string `json:"aliases"`
	} `json:"networks"`
	Labels map[string]string `json:"labels"`
}

func detailEngine() dockertest.FakeConfig {
	cfg := actionEngine()
	cfg.Containers[1].Labels = map[string]string{"com.docker.compose.project": "shop"}
	cfg.Containers[1].Ports = []dockertest.FakePort{{IP: "0.0.0.0", Private: 80, Public: 8080, Type: "tcp"}}
	cfg.Containers[1].Mounts = []string{"data"}
	cfg.Containers[1].Networks = map[string]dockertest.FakeEndpoint{"front": {NetworkID: "n1", IPv4: "10.0.0.2", Aliases: []string{"web"}}}
	cfg.Containers[1].Health = "healthy"
	cfg.Containers[1].Inspect = dockertest.FakeInspect{
		Env: []string{"DB_PASSWORD=hunter2", "MODE=prod"}, Cmd: []string{"nginx", "-g", "daemon off;"},
		StartedAt: "2026-10-01T10:00:00Z", FinishedAt: "0001-01-01T00:00:00Z",
		LogType: "syslog", LogOptions: map[string]string{"splunk-token": "s3cret-token"},
	}
	cfg.RestartPolicies = map[string]string{webID: "on-failure:3"}
	return cfg
}

func (f *envFix) detail(t *testing.T, cookie string, e envBody, id, query string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/containers/" + id + query, cookie: cookie})
	ptyp := ""
	if rec.Code >= 400 {
		ptyp = problemType(t, rec)
	}
	return &envRec{rec.Code, rec.Body.Bytes(), ptyp}
}

func TestContainerDetailOfAViewerNamesTheVariablesAndWithholdsEveryValue(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	cookie, _ := f.signIn(t, "viewer", false)

	rec := f.detail(t, cookie, e, "web", "?nodeId="+e.ID)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s\n%s", rec.code, rec.body, f.logs)
	}
	for _, leak := range []string{"hunter2", "s3cret", "prod"} {
		if strings.Contains(string(rec.body), leak) {
			t.Fatalf("a Viewer's body carries %q: %s", leak, rec.body)
		}
	}
	d := decode[detailBody](t, rec)
	if !d.Masked || len(d.Env) != 2 || d.Env[0].Name != "DB_PASSWORD" || !d.Env[0].Masked || d.Env[0].Value != "" || d.Env[1].Name != "MODE" || !d.Env[1].Masked {
		t.Errorf("env = %+v masked=%v", d.Env, d.Masked)
	}
	if d.LogConfig.Type != "syslog" || len(d.LogConfig.Options) != 1 || !d.LogConfig.Options[0].Masked || d.LogConfig.Options[0].Value != "" {
		t.Errorf("log config = %+v", d.LogConfig)
	}
	// What is not secret is shown.
	if d.ID != webID || d.Name != "web" || d.Image != "nginx:1.27" || d.State != "running" || d.Health != "healthy" || d.NodeID != e.ID || d.Node != "node-1" {
		t.Errorf("identity = %+v", d)
	}
	if d.Project != "shop" || d.Labels["com.docker.compose.project"] != "shop" || d.Restart != "on-failure" || d.Retries != 3 {
		t.Errorf("labels and restart = %+v", d)
	}
	if len(d.Ports) != 1 || d.Ports[0].ContainerPort != 80 || d.Ports[0].HostPort != "8080" || len(d.Mounts) != 1 || d.Mounts[0].Type != "volume" {
		t.Errorf("ports and mounts = %+v %+v", d.Ports, d.Mounts)
	}
	if len(d.Networks) != 1 || d.Networks[0].Name != "front" || len(d.Networks[0].Aliases) != 1 || d.Networks[0].Aliases[0] != "web" {
		t.Errorf("networks = %+v", d.Networks)
	}
	if d.StartedAt != "2026-10-01T10:00:00Z" || strings.Contains(string(rec.body), "0001-01-01") {
		t.Errorf("times: the engine's zero time must be left out: %s", rec.body)
	}
	if got := rec.body; !strings.Contains(string(got), `"command":["nginx","-g","daemon off;"]`) {
		t.Errorf("command missing: %s", got)
	}
}

func TestContainerDetailOfAnAdminCarriesTheValues(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	rec := f.detail(t, f.cookie, e, webID, "?nodeId="+e.ID)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	d := decode[detailBody](t, rec)
	if d.Masked || len(d.Env) != 2 || d.Env[0].Value != "hunter2" || d.Env[0].Masked || d.Env[1].Value != "prod" {
		t.Errorf("env = %+v masked=%v", d.Env, d.Masked)
	}
	if len(d.LogConfig.Options) != 1 || d.LogConfig.Options[0].Value != "s3cret-token" {
		t.Errorf("log options = %+v", d.LogConfig.Options)
	}
}

func TestContainerDetailOfAnOperatorShowsValuesOnlyWithTheRevealExtra(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	f.addUser(t, "plain", false)
	f.addEnvRole(t, "plain", e.ID, "operator")
	alice := f.addUser(t, "alice", false)
	if rec := f.putGrant(t, f.cookie, f.csrf, "users", alice, `{"scope":"env","envId":"`+e.ID+`","role":"operator","extras":["secrets.reveal"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}

	plain, _ := f.signIn(t, "plain", false)
	rec := f.detail(t, plain, e, "web", "?nodeId="+e.ID)
	if rec.code != http.StatusOK || strings.Contains(string(rec.body), "hunter2") || !decode[detailBody](t, rec).Masked {
		t.Fatalf("an Operator without the extra: %d %s", rec.code, rec.body)
	}
	revealing, _ := f.signIn(t, "alice", false)
	rec = f.detail(t, revealing, e, "web", "?nodeId="+e.ID)
	if d := decode[detailBody](t, rec); rec.code != http.StatusOK || d.Masked || d.Env[0].Value != "hunter2" {
		t.Fatalf("an Operator with secrets.reveal: %d %s", rec.code, rec.body)
	}
}

func TestContainerDetailAnUnknownContainerIs404AndWithoutAGrantTheEnvironmentIsNotFound(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	rec := f.detail(t, f.cookie, e, "ghost", "?nodeId="+e.ID)
	if rec.code != http.StatusNotFound || rec.ptyp != "container_not_found" {
		t.Fatalf("unknown container = %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	f.addUser(t, "nobody", false)
	nobody, _ := f.signIn(t, "nobody", false)
	rec = f.detail(t, nobody, e, "web", "?nodeId="+e.ID)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("no grant = %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if anon := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/containers/web"}); anon.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", anon.Code)
	}
}

func TestContainerDetailNodeIDMayBeLeftOutOnOneNodeAndAnUnknownNodeIs503(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	rec := f.detail(t, f.cookie, e, "web", "")
	if rec.code != http.StatusOK || decode[detailBody](t, rec).NodeID != e.ID {
		t.Fatalf("no nodeId on one node = %d: %s", rec.code, rec.body)
	}
	rec = f.detail(t, f.cookie, e, "web", "?nodeId=elsewhere")
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Fatalf("unknown node = %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestContainerDetailOnASwarmWithoutANodeIDIs422NodeRequired(t *testing.T) {
	cfg := detailEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.SwarmClusterID, cfg.SwarmNodeID = "active", true, "cluster-1", "node-a"
	f, e := dashEnv(t, cfg)
	rec := f.detail(t, f.cookie, e, "web", "")
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "node_required" {
		t.Fatalf("swarm without nodeId = %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if rec = f.detail(t, f.cookie, e, "web", "?nodeId="+e.ID); rec.code != http.StatusOK {
		t.Fatalf("swarm with nodeId = %d: %s", rec.code, rec.body)
	}
}

func TestContainerDetailMarksBayplansOwnContainers(t *testing.T) {
	f, e := dashEnv(t, detailEngine())
	own := decode[detailBody](t, f.detail(t, f.cookie, e, selfCtr, "?nodeId="+e.ID))
	if !own.IsSelf || own.SelfKind != "server" {
		t.Errorf("own server = %+v", own)
	}
	agent := decode[detailBody](t, f.detail(t, f.cookie, e, "bayplan-agent", "?nodeId="+e.ID))
	if agent.IsSelf || agent.SelfKind != "agent" {
		t.Errorf("agent = %+v", agent)
	}
	// A bad ID is refused by the schema before anything is asked of the engine.
	if rec := f.detail(t, f.cookie, e, "bad%20id", "?nodeId="+e.ID); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("bad id = %d: %s", rec.code, rec.body)
	}
}
