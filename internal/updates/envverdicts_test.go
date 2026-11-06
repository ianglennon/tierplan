// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// EnvVerdicts lists the checkable workloads of an environment with their verdicts, leaves task
// containers and local builds out, and says how far the checks reach (plan 12-28).
func TestEnvVerdictsListsCheckableWorkloadsAndTheirCoverage(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	old := dg('1')
	agg := aggOf([]envs.ImageRow{
		img(dg('c'), "docker.io/library/redis@"+old),
		img(dg('d'), "docker.io/library/nginx@"+old),
		img(dg('e'), "docker.io/library/alpine@"+old),
		img(dg('f')),
	},
		ctr("a", "redis:7", dg('c'), nil),
		ctr("b", "nginx:1", dg('d'), nil),
		ctr("c", "alpine:3", dg('e'), nil),
		ctr("local", "build:1", dg('f'), nil),
		ctr("task", "redis:7@"+old, dg('c'), map[string]string{labelSvcID: "svc1"}),
	)
	r.heads.set("docker.io/library/redis:7", answer{digest: dg('9')})
	r.heads.set("docker.io/library/alpine:3", answer{err: unauthorized("denied")})
	r.svc.Aggregator = snap{agg}
	next := t0.Add(6 * time.Hour)
	r.svc.NextCheck = func(string) (time.Time, bool) { return next, true }

	// Only redis and alpine are asked about: nginx is never checked.
	var ws []updates.Workload
	for _, w := range updates.ContainerWorkloads(agg, nil) {
		if w.ID == "a" || w.ID == "c" {
			w.EnvID = "env-1"
			ws = append(ws, w)
		}
	}
	if _, err := r.chk.CheckEnv(ctx, "env-1", ws); err != nil {
		t.Fatal(err)
	}
	r.clk.advance(time.Minute)

	got, cov, err := r.svc.EnvVerdicts(ctx, "env-1")
	if err != nil {
		t.Fatalf("EnvVerdicts: %v", err)
	}
	state := map[string]updates.State{}
	for _, wv := range got {
		state[wv.Workload.ID] = wv.Verdict.State
	}
	want := map[string]updates.State{"a": updates.StateUpdateAvailable, "b": updates.StateNotChecked, "c": updates.StateCheckFailed}
	if len(state) != len(want) {
		t.Fatalf("workloads = %v, want %v: the task container and the local build are not listed", state, want)
	}
	for id, s := range want {
		if state[id] != s {
			t.Errorf("%s = %s, want %s", id, state[id], s)
		}
	}
	if len(got) == 3 && (got[0].Workload.ID != "a" || got[1].Workload.ID != "b" || got[2].Workload.ID != "c") {
		t.Errorf("order = %s %s %s, want by name", got[0].Workload.ID, got[1].Workload.ID, got[2].Workload.ID)
	}
	if cov.Checkable != 3 || cov.Checked != 2 || cov.Failed != 1 || cov.NeverChecked != 1 {
		t.Errorf("coverage = %+v, want 3 checkable, 2 checked, 1 failed, 1 never checked", cov)
	}
	if cov.LastCheckedAt.IsZero() {
		t.Errorf("coverage has no last check time")
	}
	if cov.NextCheckAt == nil || !cov.NextCheckAt.Equal(next) {
		t.Errorf("next check = %v, want %v", cov.NextCheckAt, next)
	}
}
