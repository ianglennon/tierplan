// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/tunnel"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The swarm's task history limit (plan 11-11, JAN-03, D-17, D-21). A swarm of two managers and a
// worker behind real agent sessions, each manager's fake daemon reporting the cluster with a limit
// of five; the router, policy, aggregator and cluster reader are the real ones.

const taskHistoryAudit = "swarm.task_history_limit_changed"

type taskHistoryAnswer struct {
	Limit         int64 `json:"limit"`
	DockerDefault int   `json:"dockerDefault"`
	KeepsAll      bool  `json:"keepsAll"`
	Changed       bool  `json:"changed"`
}

// engineSpec is the swarm spec the managers report besides the limit: every setting has a value that
// is not the zero value, so an update that dropped one differs from it.
func engineSpec() swarm.Spec {
	keep := uint64(2)
	return swarm.Spec{
		Annotations: swarm.Annotations{Name: "default", Labels: map[string]string{"env": "prod"}},
		Raft:        swarm.RaftConfig{SnapshotInterval: 10000, KeepOldSnapshots: &keep, ElectionTick: 10, HeartbeatTick: 1},
		Dispatcher:  swarm.DispatcherConfig{HeartbeatPeriod: 5_000_000_000},
	}
}

// taskHistorySwarm is the swarm of newSwarmFix where both managers report a task history limit of
// five, and tune changes each node's engine last.
func taskHistorySwarm(t *testing.T, tune func(host string, cfg *dockertest.FakeConfig)) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if strings.HasPrefix(host, "mgr-") {
			cfg.HistoryLimit = 5
			cfg.SwarmSpec = engineSpec()
		}
		if tune != nil {
			tune(host, cfg)
		}
	})
}

func (s *swarmFix) taskHistoryPath() string {
	return "/api/env/" + s.env.ID + "/janitor/task-history"
}

func (s *swarmFix) getTaskHistory(t *testing.T) (*envRec, taskHistoryAnswer) {
	t.Helper()
	rec := s.req(t, http.MethodGet, s.taskHistoryPath(), "")
	var got taskHistoryAnswer
	if rec.code == http.StatusOK {
		got = decode[taskHistoryAnswer](t, rec)
	}
	return rec, got
}

func (s *swarmFix) putTaskHistory(t *testing.T, body string) (*envRec, taskHistoryAnswer) {
	t.Helper()
	rec := s.req(t, http.MethodPut, s.taskHistoryPath(), body)
	var got taskHistoryAnswer
	if rec.code == http.StatusOK {
		got = decode[taskHistoryAnswer](t, rec)
	}
	return rec, got
}

// swarmUpdates are the swarm updates the managers accepted.
func (s *swarmFix) swarmUpdates() []dockertest.FakeSwarmUpdate {
	return slices.Concat(s.m1.fake.SwarmUpdates(), s.m2.fake.SwarmUpdates())
}

// swarmUpdateRequests are the swarm updates any node received, accepted or not.
func (s *swarmFix) swarmUpdateRequests() []string {
	var out []string
	for _, c := range []*agentConn{s.m1, s.m2, s.w} {
		for _, w := range c.fake.Writes() {
			if strings.HasPrefix(w, "POST /swarm/update") {
				out = append(out, w)
			}
		}
	}
	return out
}

// The tracer: an Env Admin sets the limit through a manager, one update goes out with the version
// the manager reported, the limit reads back, and the change is on record.
func TestTaskHistoryPutAndGet(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, nil)
	if rec, got := s.getTaskHistory(t); rec.code != http.StatusOK || got.Limit != 5 || got.DockerDefault != 5 || got.KeepsAll || got.Changed {
		t.Fatalf("before: %d %s, want 200 with the limit 5 and Docker's default 5", rec.code, rec.body)
	}
	rec, got := s.putTaskHistory(t, `{"limit":10}`)
	if rec.code != http.StatusOK || got.Limit != 10 || !got.Changed {
		t.Fatalf("put: %d %s, want 200, limit 10, changed true", rec.code, rec.body)
	}
	ups := s.swarmUpdates()
	if len(ups) != 1 || ups[0].Version != "1" {
		t.Fatalf("updates = %+v, want one against the version the manager reported (1)", ups)
	}
	if lim := ups[0].Spec.Orchestration.TaskHistoryRetentionLimit; lim == nil || *lim != 10 {
		t.Fatalf("the update carried the limit %v, want 10", lim)
	}
	if rec, got := s.getTaskHistory(t); rec.code != http.StatusOK || got.Limit != 10 {
		t.Fatalf("after: %d %s, want 200 with the limit 10", rec.code, rec.body)
	}
	rows := s.auditRows(t, taskHistoryAudit)
	if len(rows) != 1 || rows[0].Result != "success" || rows[0].EnvID != s.env.ID || rows[0].Details["from"] != "5" || rows[0].Details["to"] != "10" || len(rows[0].Details) != 2 {
		t.Fatalf("audit rows = %+v, want one success with exactly from 5 and to 10", rows)
	}
}

// 0 and 1000 are the ends of the range; below, above, not whole, out of range of a 64-bit integer
// and missing are 422 and nothing is sent.
func TestTaskHistoryRange(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, nil)
	for _, limit := range []string{"0", "1000"} {
		if rec, got := s.putTaskHistory(t, `{"limit":`+limit+`}`); rec.code != http.StatusOK || !got.Changed {
			t.Fatalf("limit %s: %d %s, want 200 changed", limit, rec.code, rec.body)
		}
	}
	if len(s.swarmUpdates()) != 2 {
		t.Fatalf("updates = %+v, want the two accepted", s.swarmUpdates())
	}
	for _, body := range []string{
		`{"limit":-1}`, `{"limit":1001}`, `{"limit":2.5}`, `{"limit":1e30}`, `{"limit":99999999999999999999}`,
		`{"limit":"7"}`, `{"limit":null}`, `{}`,
	} {
		rec, _ := s.putTaskHistory(t, body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("body %q: %d %s, want 422", body, rec.code, rec.body)
		}
	}
	if got := s.swarmUpdateRequests(); len(got) != 2 {
		t.Fatalf("update requests = %v, want only the two accepted: a refused value sends nothing", got)
	}
	if rec, got := s.getTaskHistory(t); rec.code != http.StatusOK || got.Limit != 1000 {
		t.Fatalf("after: %d %s, want the last accepted limit, 1000", rec.code, rec.body)
	}
}

// Setting the limit the swarm already has sends nothing and records nothing.
func TestTaskHistoryNoOp(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, nil)
	rec, got := s.putTaskHistory(t, `{"limit":5}`)
	if rec.code != http.StatusOK || got.Changed || got.Limit != 5 {
		t.Fatalf("put: %d %s, want 200, limit 5, changed false", rec.code, rec.body)
	}
	if reqs := s.swarmUpdateRequests(); len(reqs) != 0 {
		t.Fatalf("update requests = %v, want none", reqs)
	}
	if rows := s.auditRows(t, taskHistoryAudit); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none for a no-op", rows)
	}
}

// A swarm that changed between the read and the update is 409 swarm_changed, the update is sent once
// and never again, and the refusal is on record.
func TestTaskHistorySwarmChanged(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, func(host string, cfg *dockertest.FakeConfig) {
		cfg.SwarmChangedUnderUpdate = strings.HasPrefix(host, "mgr-")
	})
	rec, _ := s.putTaskHistory(t, `{"limit":10}`)
	if rec.code != http.StatusConflict || rec.ptyp != "swarm_changed" {
		t.Fatalf("put: %d %s, want 409 swarm_changed", rec.code, rec.body)
	}
	if !strings.Contains(string(rec.body), "The swarm changed while TierPlan was saving. Try again.") {
		t.Errorf("body = %s, want the try-again sentence", rec.body)
	}
	if reqs := s.swarmUpdateRequests(); len(reqs) != 1 {
		t.Fatalf("update requests = %v, want exactly one: a conflict is never retried", reqs)
	}
	if ups := s.swarmUpdates(); len(ups) != 0 {
		t.Fatalf("accepted updates = %+v, want none", ups)
	}
	rows := s.auditRows(t, taskHistoryAudit)
	if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != "swarm_changed" {
		t.Fatalf("audit rows = %+v, want one failure with the reason swarm_changed", rows)
	}
}

// The manager's own word is passed on: -1 means it keeps every task, and the figure is not turned into Docker's default.
func TestTaskHistoryKeepsAll(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, func(host string, cfg *dockertest.FakeConfig) {
		if strings.HasPrefix(host, "mgr-") {
			cfg.HistoryLimit = -1
		}
	})
	rec, got := s.getTaskHistory(t)
	if rec.code != http.StatusOK || got.Limit != -1 || !got.KeepsAll || got.DockerDefault != 5 {
		t.Fatalf("get: %d %s, want 200 with limit -1, keepsAll true", rec.code, rec.body)
	}
	if rec, got := s.putTaskHistory(t, `{"limit":5}`); rec.code != http.StatusOK || !got.Changed || got.Limit != 5 || got.KeepsAll {
		t.Fatalf("put 5: %d %s, want a change from keeping every task to five", rec.code, rec.body)
	}
	rows := s.auditRows(t, taskHistoryAudit)
	if len(rows) != 1 || rows[0].Details["from"] != "-1" || rows[0].Details["to"] != "5" {
		t.Fatalf("audit rows = %+v, want from -1 to 5", rows)
	}
}

// A standalone host has no swarm to change.
func TestTaskHistoryNotSwarm(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, richEngine())
	f.awaitFirstProbe(t, e.ID) // until then the host is not known to be a swarm or not
	path := "/api/env/" + e.ID + "/janitor/task-history"
	if rec := f.req(t, http.MethodGet, path, ""); rec.code != http.StatusConflict || rec.ptyp != "not_swarm" {
		t.Fatalf("get: %d %s, want 409 not_swarm", rec.code, rec.body)
	}
	if rec := f.req(t, http.MethodPut, path, `{"limit":10}`); rec.code != http.StatusConflict || rec.ptyp != "not_swarm" {
		t.Fatalf("put: %d %s, want 409 not_swarm", rec.code, rec.body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
}

// A swarm with no manager answering is 503 no_manager and nothing is sent.
func TestTaskHistoryNoManager(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, nil)
	for _, m := range []*agentConn{s.m1, s.m2} {
		s.srv.Hub.Kick(m.id, tunnel.DisconnectClosed)
		if _, err := s.srv.Hub.AwaitSession(testsupport.Context(t), m.id, func(sess *tunnel.Session) bool { return sess == nil }); err != nil {
			t.Fatalf("the session of %s never went: %v", m.id, err)
		}
	}
	if rec, _ := s.getTaskHistory(t); rec.code != http.StatusServiceUnavailable || rec.ptyp != "no_manager" {
		t.Fatalf("get: %d %s, want 503 no_manager", rec.code, rec.body)
	}
	if rec, _ := s.putTaskHistory(t, `{"limit":10}`); rec.code != http.StatusServiceUnavailable || rec.ptyp != "no_manager" {
		t.Fatalf("put: %d %s, want 503 no_manager", rec.code, rec.body)
	}
	if reqs := s.swarmUpdateRequests(); len(reqs) != 0 {
		t.Fatalf("update requests = %v, want none", reqs)
	}
	if rows := s.auditRows(t, taskHistoryAudit); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none: nothing was attempted", rows)
	}
}

// An agent older than this server refuses the update by its own policy: 502 agent_policy_denied
// that says what to do, on record as a failure.
func TestTaskHistoryAgentTooOld(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, func(host string, cfg *dockertest.FakeConfig) {
		cfg.SwarmUpdateDeny = strings.HasPrefix(host, "mgr-")
	})
	if rec, got := s.getTaskHistory(t); rec.code != http.StatusOK || got.Limit != 5 {
		t.Fatalf("get: %d %s, want the read to work through the older agent", rec.code, rec.body)
	}
	rec, _ := s.putTaskHistory(t, `{"limit":10}`)
	if rec.code != http.StatusBadGateway || rec.ptyp != "agent_policy_denied" {
		t.Fatalf("put: %d %s, want 502 agent_policy_denied", rec.code, rec.body)
	}
	if !strings.Contains(string(rec.body), "Update the agent on a manager to change this limit.") {
		t.Errorf("body = %s, want the update-the-agent sentence", rec.body)
	}
	if reqs := s.swarmUpdateRequests(); len(reqs) != 1 {
		t.Fatalf("update requests = %v, want exactly one: a refusal is never retried", reqs)
	}
	rows := s.auditRows(t, taskHistoryAudit)
	if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != "agent_policy_denied" {
		t.Fatalf("audit rows = %+v, want one failure with the reason agent_policy_denied", rows)
	}
}

// Only an Env Admin changes the limit (janitor.run); a Viewer may read it; a person with no access to
// the environment does not learn it exists.
func TestTaskHistoryPutNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	s := taskHistorySwarm(t, nil)
	as := func(user, role, method, body string) *httptest.ResponseRecorder {
		s.addUser(t, user, false)
		if role != "" {
			s.addEnvRole(t, user, s.env.ID, role)
		}
		cookie, csrf := s.signIn(t, user, false)
		return s.call(t, authReq{method: method, path: s.taskHistoryPath(), body: body, cookie: cookie, csrf: csrf})
	}
	if rec := as("vera", "viewer", http.MethodGet, ""); rec.Code != http.StatusOK {
		t.Fatalf("a viewer reading: %d %s, want 200", rec.Code, rec.Body)
	}
	for _, role := range []string{"viewer", "operator"} {
		if rec := as("u-"+role, role, http.MethodPut, `{"limit":10}`); rec.Code != http.StatusForbidden {
			t.Fatalf("a %s changing the limit: %d %s, want 403", role, rec.Code, rec.Body)
		}
	}
	if rec := as("nobody", "", http.MethodGet, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a user with no access: %d %s, want 404 as every environment route answers", rec.Code, rec.Body)
	}
	if reqs := s.swarmUpdateRequests(); len(reqs) != 0 {
		t.Fatalf("update requests = %v, want none from a refused caller", reqs)
	}
	if rec := as("ada", "admin", http.MethodPut, `{"limit":10}`); rec.Code != http.StatusOK {
		t.Fatalf("an Env Admin changing the limit: %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := s.call(t, authReq{method: http.MethodPut, path: s.taskHistoryPath(), body: `{"limit":3}`}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d, want 401", rec.Code)
	}
}
