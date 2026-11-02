// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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
