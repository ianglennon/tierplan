// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Clean now (plan 11-04, JAN-09, JAN-06, D-08, D-14, D-21). The engines are fake daemons behind the
// nodes of a swarm, reached through real agent sessions; the route, the lock, the jobs hub and the
// runner are the real ones, and the clock of the run is the fixture's, so the containers of
// janitorSwarm are two days old.

// nowClock is a stackops.Clock that tells the time a function does. It never fires After: nothing
// in a janitor run waits on the clock.
type nowClock func() time.Time

func (c nowClock) Now() time.Time                       { return c() }
func (c nowClock) After(time.Duration) <-chan time.Time { return nil }

type janitorStarted struct {
	RunID string `json:"runId"`
	JobID string `json:"jobId"`
}

// tick makes the body of a run from the refs of preview items.
func tick(items ...janitorItem) string {
	type ref struct {
		Type   string `json:"type"`
		NodeID string `json:"nodeId"`
		Ref    string `json:"ref"`
	}
	refs := make([]ref, 0, len(items))
	for _, it := range items {
		refs = append(refs, ref{it.Type, it.NodeID, it.Ref})
	}
	raw, err := json.Marshal(map[string]any{"items": refs})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// runJanitor presses Clean now as the fixture's instance admin.
func (s *swarmFix) runJanitor(t *testing.T, body string) (*envRec, janitorStarted) {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/janitor/runs", body)
	var got janitorStarted
	if rec.code == http.StatusAccepted {
		got = decode[janitorStarted](t, rec)
	}
	return rec, got
}

// awaitRun waits for the job of a run to end and returns it.
func (s *swarmFix) awaitRun(t *testing.T, id string) jobs.Job {
	t.Helper()
	job, err := s.jobHub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatalf("the job %s never ended: %v", id, err)
	}
	return job
}

// containerDeletes lists the container deletes the worker's daemon served, as it saw them.
func (s *swarmFix) containerDeletes() []string {
	var out []string
	for _, a := range []*agentConn{s.m1, s.m2, s.w} {
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "DELETE /containers/") {
				out = append(out, w)
			}
		}
	}
	return out
}

func TestJanitorRunRemovesTickedTaskContainer(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	if len(preview.Items) != 3 {
		t.Fatalf("preview items = %+v, want 3", preview.Items)
	}
	rec, started := s.runJanitor(t, tick(preview.Items[0]))
	if rec.code != http.StatusAccepted || started.RunID == "" || started.JobID == "" {
		t.Fatalf("Clean now: %d %s, want 202 with a run and a job", rec.code, rec.body)
	}
	job := s.awaitRun(t, started.JobID)
	if job.Kind != "janitor_run" || job.State != jobs.JobDone || len(job.Items) != 1 || job.Items[0].State != jobs.ItemDone {
		t.Fatalf("job = %s %s items %+v, want a done janitor_run with one done item", job.Kind, job.State, job.Items)
	}
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/web-x1"}) {
		t.Fatalf("the daemons saw %v, want one delete of web-x1", got)
	}
	// The container is gone, so the next preview no longer lists it.
	_, after := s.previewJanitor(t)
	if len(after.Items) != 2 || after.Items[0].Ref != "web-x2" {
		t.Fatalf("preview after = %+v, want web-x2 and web-x3 left", after.Items)
	}
}
