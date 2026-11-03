// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Crash evidence (plan 11-13, JAN-10, D-06, T-11-35, T-11-38). The engines are fake daemons behind
// the nodes of a swarm, reached through real agent sessions; the route, the lock, the jobs hub, the
// runner and the SQLite store are the real ones.

// janitorEvidenceRead is the answer of GET .../items/{seq}/evidence, as the tests read it.
type janitorEvidenceRead struct {
	ExitCode    int      `json:"exitCode"`
	Error       string   `json:"error"`
	OOMKilled   bool     `json:"oomKilled"`
	StartedAt   *string  `json:"startedAt"`
	FinishedAt  *string  `json:"finishedAt"`
	Image       string   `json:"image"`
	ImageID     string   `json:"imageId"`
	ImageDigest string   `json:"imageDigest"`
	Node        string   `json:"node"`
	LogLines    []string `json:"logLines"`
	LogState    string   `json:"logState"`
}

// crashed makes the container id of the worker one that exited with code, killed for memory when
// oom, with the given environment.
func crashed(id string, code int, oom bool, env []string) func(host string, cfg *dockertest.FakeConfig) {
	return func(host string, cfg *dockertest.FakeConfig) {
		if host != "wrk-1" {
			return
		}
		for i := range cfg.Containers {
			if cfg.Containers[i].ID == id {
				in := &cfg.Containers[i].Inspect
				in.ExitCode, in.OOMKilled, in.Env = code, oom, env
			}
		}
	}
}

// getEvidence reads the evidence of an item of a run as the fixture's instance admin.
func (s *swarmFix) getEvidence(t *testing.T, runID string, seq int) (*envRec, janitorEvidenceRead) {
	t.Helper()
	rec := s.req(t, http.MethodGet, s.evidencePath(runID, seq), "")
	var got janitorEvidenceRead
	if rec.code == http.StatusOK {
		got = decode[janitorEvidenceRead](t, rec)
	}
	return rec, got
}

func (s *swarmFix) evidencePath(runID string, seq int) string {
	return "/api/env/" + s.env.ID + "/janitor/runs/" + runID + "/items/" + strconv.Itoa(seq) + "/evidence"
}

// evidenceRows counts the evidence rows on record, in every run.
func (s *swarmFix) evidenceRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.st.Read.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM janitor_evidence`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The tracer of JAN-10's evidence: a task container that exited 137, killed for memory, is removed
// by Clean now; its facts and its log, masked, are on record, and the row was committed before the
// daemon saw the DELETE.
func TestJanitorEvidenceSavedBeforeRemove(t *testing.T) {
	t.Parallel()
	var fix atomic.Pointer[swarmFix]
	var rowsAtDelete atomic.Int64
	rowsAtDelete.Store(-1)
	tune := crashed("web-x1", 137, true, []string{"DB_PASSWORD=hunter2", "PORT=8080"})
	s := janitorSwarm(t, onWorker(exitedSlot(5)), func(host string, cfg *dockertest.FakeConfig) {
		tune(host, cfg)
		if host != "wrk-1" {
			return
		}
		cfg.OnWrite = func(w string) {
			if w != "DELETE /containers/web-x1" {
				return
			}
			var n int
			if f := fix.Load(); f != nil {
				if err := f.st.Read.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM janitor_evidence`).Scan(&n); err != nil {
					n = -2
				}
			}
			rowsAtDelete.Store(int64(n))
		}
	})
	fix.Store(s)
	s.w.fake.AppendLogs("web-x1",
		dockertest.FakeLogLine{Text: "starting up"},
		dockertest.FakeLogLine{Text: "connecting with password=hunter2"},
		dockertest.FakeLogLine{Stream: "stderr", Text: "killed: out of memory"},
	)
	_, preview := s.previewJanitor(t)
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)

	if got := rowsAtDelete.Load(); got != 1 {
		t.Fatalf("the daemon saw the DELETE with %d evidence rows on record, want the row committed first", got)
	}
	if got := s.containerDeletes(); len(got) != 1 || got[0] != "DELETE /containers/web-x1" {
		t.Fatalf("the daemons saw %v, want one delete of web-x1", got)
	}
	_, run := s.getRun(t, started.RunID)
	if len(run.Items) != 1 || run.Items[0].Outcome != "removed" || !run.Items[0].HasEvidence {
		t.Fatalf("items = %+v, want the container removed and flagged as having evidence", run.Items)
	}
	rec, ev := s.getEvidence(t, started.RunID, run.Items[0].Seq)
	if rec.code != http.StatusOK {
		t.Fatalf("GET evidence: %d %s, want 200", rec.code, rec.body)
	}
	if ev.ExitCode != 137 || !ev.OOMKilled || ev.Node != "wrk-1" || ev.Image != "nginx:1" || ev.LogState != "saved" || ev.StartedAt == nil || ev.FinishedAt == nil {
		t.Fatalf("evidence = %+v, want exit 137, OOM killed, on wrk-1, image nginx:1 with its log saved", ev)
	}
	if len(ev.LogLines) != 3 || !strings.HasSuffix(ev.LogLines[0], " starting up") || !strings.HasSuffix(ev.LogLines[2], " killed: out of memory") {
		t.Fatalf("log lines = %q, want the three lines the container wrote", ev.LogLines)
	}
	if !strings.HasSuffix(ev.LogLines[1], " connecting with password=<hidden>") {
		t.Fatalf("log line = %q, want the value of password masked", ev.LogLines[1])
	}
	if strings.Contains(string(rec.body), "hunter2") {
		t.Fatalf("the answer holds the secret: %s", rec.body)
	}
}

// The log driver of the container cannot be read back (the engine answers 501): the evidence says so
// and the container is removed all the same.
func TestJanitorEvidenceLogsUnreadable(t *testing.T) {
	t.Parallel()
	crash := crashed("web-x1", 1, false, nil)
	s := janitorSwarm(t, onWorker(exitedSlot(5)), func(host string, cfg *dockertest.FakeConfig) {
		crash(host, cfg)
		if host != "wrk-1" {
			return
		}
		for i := range cfg.Containers {
			if cfg.Containers[i].ID == "web-x1" {
				cfg.Containers[i].Inspect.LogType = "none"
			}
		}
	})
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)
	if got := s.containerDeletes(); len(got) != 1 {
		t.Fatalf("the daemons saw %v, want web-x1 removed although its log could not be read", got)
	}
	rec, ev := s.getEvidence(t, started.RunID, 1)
	if rec.code != http.StatusOK || ev.LogState != "unavailable" || len(ev.LogLines) != 0 || ev.ExitCode != 1 {
		t.Fatalf("evidence = %d %+v, want the exit code with the log unavailable and no lines (not null)", rec.code, ev)
	}
	if !strings.Contains(string(rec.body), `"logLines":[]`) {
		t.Fatalf("body = %s, want logLines an empty list", rec.body)
	}
}

// A container that exited 0 leaves no evidence, and asking for it is a 404 with its own word.
func TestJanitorEvidenceNotForCleanExit(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)
	_, run := s.getRun(t, started.RunID)
	if len(run.Items) != 1 || run.Items[0].Outcome != "removed" || run.Items[0].HasEvidence {
		t.Fatalf("items = %+v, want web-x1 removed with no evidence", run.Items)
	}
	if rec, _ := s.getEvidence(t, started.RunID, 1); rec.code != http.StatusNotFound || rec.ptyp != "janitor_evidence_not_found" {
		t.Fatalf("GET evidence: %d %s (%s), want 404 janitor_evidence_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rec, _ := s.getEvidence(t, "no-such-run", 1); rec.code != http.StatusNotFound || rec.ptyp != "janitor_evidence_not_found" {
		t.Fatalf("GET evidence of a run nobody has: %d %s (%s), want 404 janitor_evidence_not_found", rec.code, rec.body, rec.ptyp)
	}
	if n := s.evidenceRows(t); n != 0 {
		t.Fatalf("%d evidence rows on record, want none", n)
	}
}

// Evidence is a container's log, so whoever may read container logs may read it: a Viewer and an
// Operator can, and a person with no role on the environment finds nothing there.
func TestJanitorEvidenceAccess(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), crashed("web-x1", 137, true, nil))
	s.w.fake.AppendLogs("web-x1", dockertest.FakeLogLine{Text: "going down"})
	_, preview := s.previewJanitor(t)
	_, started := s.runJanitor(t, tick(preview.Items[0]))
	s.awaitRun(t, started.JobID)
	path := s.evidencePath(started.RunID, 1)

	as := func(user, role string) *httptest.ResponseRecorder {
		s.addUser(t, user, false)
		if role != "" {
			s.addEnvRole(t, user, s.env.ID, role)
		}
		cookie, csrf := s.signIn(t, user, false)
		return s.call(t, authReq{method: http.MethodGet, path: path, cookie: cookie, csrf: csrf})
	}
	for _, c := range []struct{ user, role string }{{"vera", "viewer"}, {"olga", "operator"}, {"ada", "admin"}} {
		rec := as(c.user, c.role)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "going down") {
			t.Errorf("a %s: %d %s, want 200 with the evidence", c.role, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("a %s: Cache-Control = %q, want no-store", c.role, got)
		}
	}
	if rec := as("nora", ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "going down") {
		t.Errorf("a person with no role here: %d %s, want 404 and nothing of the evidence", rec.Code, rec.Body)
	}
	if rec := s.call(t, authReq{method: http.MethodGet, path: path}); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out: %d, want 401", rec.Code)
	}
}

// maskingFacts are the stack records of an environment that knows one stack, whose deployed
// revision holds a secret; only the masker is used.
type maskingFacts struct {
	envs.StackFacts
	masker stacks.Masker
	err    error
}

func (f maskingFacts) DeployedMasker(_ context.Context, stackID string) (stacks.Masker, error) {
	if f.err != nil {
		return stacks.Masker{}, f.err
	}
	if stackID != "stk-1" {
		return stacks.Masker{}, nil
	}
	return f.masker, nil
}

// The engine's evidence for a container of a TierPlan stack hides the stack's secret values, and the
// container's own secret-looking environment values, in every line of the log.
func TestJanitorEvidenceMasksTheStacksSecrets(t *testing.T) {
	t.Parallel()
	crash := crashed("web-x1", 1, false, []string{"API_TOKEN=tok-abcdef", "PORT=8080"})
	s := janitorSwarm(t, onWorker(exitedSlot(5)), func(host string, cfg *dockertest.FakeConfig) {
		crash(host, cfg)
		if host != "wrk-1" {
			return
		}
		for i := range cfg.Containers {
			if cfg.Containers[i].ID == "web-x1" {
				cfg.Containers[i].Labels["tierplan.stack.id"] = "stk-1"
			}
		}
	})
	s.w.fake.AppendLogs("web-x1",
		dockertest.FakeLogLine{Text: "db refused STACK-SECRET-VALUE for user"},
		dockertest.FakeLogLine{Text: "sent tok-abcdef to the gateway"},
		dockertest.FakeLogLine{Text: "listening on 8080"},
	)
	_, preview := s.previewJanitor(t)
	node := preview.Items[0].NodeID
	ctx := testsupport.Context(t)

	facts := maskingFacts{masker: stacks.NewMasker([]stacks.Var{{Name: "DB_PASS", Value: "STACK-SECRET-VALUE", Secret: true}}, nil)}
	src, err := api.NewJanitorEngine(s.agg, s.clusters, facts, janitor.KeepStore{}, nil).Evidence(ctx, s.env.ID, node, "web-x1")
	if err != nil {
		t.Fatalf("Evidence: %v", err)
	}
	if src.End.ExitCode != 1 || src.LogsUnavailable || len(src.Lines) != 3 || src.Labels["tierplan.stack.id"] != "stk-1" {
		t.Fatalf("source = %+v, want exit 1, the three lines read and the stack label", src)
	}
	ev := janitor.BuildEvidence(src)
	joined := strings.Join(ev.Lines, "\n")
	if strings.Contains(joined, "STACK-SECRET-VALUE") || strings.Contains(joined, "tok-abcdef") || !strings.Contains(joined, "listening on 8080") {
		t.Fatalf("lines = %q, want both secrets hidden and the plain port left", ev.Lines)
	}

	// While the stack's values cannot be read, no line of the log is kept.
	bad := maskingFacts{err: errors.New("the record is unreadable")}
	src, err = api.NewJanitorEngine(s.agg, s.clusters, bad, janitor.KeepStore{}, nil).Evidence(ctx, s.env.ID, node, "web-x1")
	if err != nil || !src.LogsUnavailable || len(src.Lines) != 0 {
		t.Fatalf("source = %+v, %v; want the log withheld because the stack's secrets could not be read", src, err)
	}
	// A container that is not a stack's needs no masker.
	src, err = api.NewJanitorEngine(s.agg, s.clusters, bad, janitor.KeepStore{}, nil).Evidence(ctx, s.env.ID, node, "web-x2")
	if err != nil || src.LogsUnavailable {
		t.Fatalf("source of a container with no stack = %+v, %v; want its log read", src, err)
	}
}
