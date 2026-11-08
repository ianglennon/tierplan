// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// A run's record names its job only once the job's body has begun, which can be after the start
// request answered. A stream opened in that gap must still find the job, by the run ID in its meta,
// and only a janitor job of the same environment counts.
func TestJanitorRunJobFindsTheJobBeforeTheRunNamesIt(t *testing.T) {
	t.Parallel()
	hub := jobs.New(jobs.Options{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	start := func(kind, env, runID string) jobs.Job {
		t.Helper()
		j, err := hub.Start(jobs.Spec{Kind: kind, EnvID: env, Meta: map[string]string{janitor.MetaRunID: runID}},
			func(ctx context.Context, _ *jobs.Reporter) {
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		if err != nil {
			t.Fatalf("start %s job: %v", kind, err)
		}
		return j
	}
	want := start(janitor.JobKind, "env-1", "run-1")
	start(janitor.JobKind, "env-2", "run-2")
	start("pull", "env-1", "run-3")

	cases := []struct {
		name   string
		run    janitor.Run
		wantOK bool
	}{
		{"job not yet on record", janitor.Run{ID: "run-1"}, true},
		{"job on record", janitor.Run{ID: "run-1", JobID: want.ID}, true},
		{"another environment's run", janitor.Run{ID: "run-2"}, false},
		{"a job of another kind", janitor.Run{ID: "run-3"}, false},
		{"no such run", janitor.Run{ID: "run-9"}, false},
		{"recorded job is gone", janitor.Run{ID: "run-1", JobID: "gone"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := janitorRunJob(hub, "env-1", tc.run)
			if ok != tc.wantOK || (ok && got.ID != want.ID) {
				t.Fatalf("janitorRunJob = %q, %v; want %q, %v", got.ID, ok, want.ID, tc.wantOK)
			}
		})
	}
}
