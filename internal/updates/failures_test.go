// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"testing"
	"time"

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

func TestFailureReplacedByNewer(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := failureStore(r)

	first := sampleFailure("env-a", "service", "web", digestD)
	first.FailedAt = time.Unix(1000, 0)
	if err := s.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := sampleFailure("env-a", "service", "web", digestE)
	second.Reason = "task failed to start"
	second.RunID = "run-2"
	second.FailedAt = time.Unix(2000, 0)
	if err := s.Record(ctx, second); err != nil {
		t.Fatal(err)
	}
	got, err := s.ForEnv(ctx, "env-a")
	if err != nil || len(got) != 1 {
		t.Fatalf("ForEnv = %+v, %v, want one row", got, err)
	}
	f := got[updates.FailureKey("service", "web")]
	if f.Digest != digestE || f.Reason != "task failed to start" || f.RunID != "run-2" || !f.FailedAt.Equal(time.Unix(2000, 0)) {
		t.Errorf("failure = %+v, want the newer one", f)
	}
}

func TestRollbackFailedRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := failureStore(r)

	f := sampleFailure("env-a", "container", "db", digestD)
	f.RollbackFailed = true
	if err := s.Record(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := s.ForEnv(ctx, "env-a")
	if err != nil {
		t.Fatal(err)
	}
	if !got[updates.FailureKey("container", "db")].RollbackFailed {
		t.Errorf("RollbackFailed = false, want true")
	}
	f.RollbackFailed = false
	if err := s.Record(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ForEnv(ctx, "env-a"); err != nil || got[updates.FailureKey("container", "db")].RollbackFailed {
		t.Errorf("RollbackFailed after replace = %+v, %v, want false", got, err)
	}
}

func TestFailureKeyedByKind(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := failureStore(r)

	if err := s.Record(ctx, sampleFailure("env-a", "service", "web", digestD)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, sampleFailure("env-a", "container", "web", digestE)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ForEnv(ctx, "env-a")
	if err != nil || len(got) != 2 {
		t.Fatalf("ForEnv = %+v, %v, want two rows", got, err)
	}
	if got[updates.FailureKey("service", "web")].Digest != digestD || got[updates.FailureKey("container", "web")].Digest != digestE {
		t.Errorf("ForEnv = %+v, want each kind to keep its own digest", got)
	}
	if cleared, err := s.Clear(ctx, "env-a", "service", "web"); err != nil || !cleared {
		t.Fatalf("Clear(service) = %v, %v", cleared, err)
	}
	if got, err = s.ForEnv(ctx, "env-a"); err != nil || len(got) != 1 || got[updates.FailureKey("container", "web")].Digest != digestE {
		t.Errorf("ForEnv after clearing the service = %+v, %v, want the container's failure kept", got, err)
	}
}

func TestFailuresPerEnvironment(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	s := failureStore(r)

	if err := s.Record(ctx, sampleFailure("env-a", "service", "web", digestD)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, sampleFailure("env-b", "service", "web", digestE)); err != nil {
		t.Fatal(err)
	}
	a, err := s.ForEnv(ctx, "env-a")
	if err != nil || len(a) != 1 || a[updates.FailureKey("service", "web")].Digest != digestD {
		t.Errorf("ForEnv(a) = %+v, %v, want only a's failure", a, err)
	}
	b, err := s.ForEnv(ctx, "env-b")
	if err != nil || len(b) != 1 || b[updates.FailureKey("service", "web")].Digest != digestE {
		t.Errorf("ForEnv(b) = %+v, %v, want only b's failure", b, err)
	}
	if cleared, err := s.Clear(ctx, "env-a", "service", "web"); err != nil || !cleared {
		t.Fatalf("Clear(a) = %v, %v", cleared, err)
	}
	if b, err = s.ForEnv(ctx, "env-b"); err != nil || len(b) != 1 {
		t.Errorf("ForEnv(b) after clearing a = %+v, %v, want b's failure kept", b, err)
	}
}

func TestClearNothing(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	s := failureStore(r)

	cleared, err := s.Clear(ctx, "env-a", "service", "web")
	if err != nil || cleared {
		t.Errorf("Clear with no failure = %v, %v, want false and no error", cleared, err)
	}
}

func TestFailuresCascadeWithEnvironment(t *testing.T) {
	t.Parallel()
	ctx := testsupport.Context(t)
	r := newRig(t)
	r.addEnvironment(t, "env-a")
	r.addEnvironment(t, "env-b")
	s := failureStore(r)

	if err := s.Record(ctx, sampleFailure("env-a", "service", "web", digestD)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, sampleFailure("env-b", "service", "web", digestD)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.Write.ExecContext(ctx, `DELETE FROM environments WHERE id = ?`, "env-a"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ForEnv(ctx, "env-a"); err != nil || len(got) != 0 {
		t.Errorf("ForEnv(a) after deleting it = %+v, %v, want none", got, err)
	}
	if got, err := s.ForEnv(ctx, "env-b"); err != nil || len(got) != 1 {
		t.Errorf("ForEnv(b) = %+v, %v, want b's failure kept", got, err)
	}
}
