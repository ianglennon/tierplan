// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Remembered failed updates (plan 12-43, IMG-07, D-14): one failed digest per workload, per environment.

func failureStore(r *rig) updates.FailureStore {
	return updates.FailureStore{Q: r.st.Q, W: r.st.Write, Now: r.clk.Now}
}

func sampleFailure(env, kind, workload, digest string) updates.Failure {
	return updates.Failure{EnvID: env, Kind: kind, Workload: workload, Digest: digest, Reason: "unhealthy", RunID: "run-1"}
}

func TestFailureRecordedAndRead(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := failureStore(r)

	if err := s.Record(ctx, sampleFailure("env-a", "service", "web", digestD)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ForEnv(ctx, "env-a")
	if err != nil || len(got) != 1 {
		t.Fatalf("ForEnv = %+v, %v, want the one failure", got, err)
	}
	f, ok := got[updates.FailureKey("service", "web")]
	if !ok || f.Digest != digestD || f.Reason != "unhealthy" || f.RunID != "run-1" || f.RollbackFailed || f.FailedAt.IsZero() ||
		f.EnvID != "env-a" || f.Kind != "service" || f.Workload != "web" {
		t.Errorf("ForEnv[service/web] = %+v, ok=%v, want the failure as recorded", f, ok)
	}

	cleared, err := s.Clear(ctx, "env-a", "service", "web")
	if err != nil || !cleared {
		t.Fatalf("Clear = %v, %v, want true", cleared, err)
	}
	if got, err = s.ForEnv(ctx, "env-a"); err != nil || len(got) != 0 {
		t.Errorf("ForEnv after Clear = %+v, %v, want none", got, err)
	}
}
