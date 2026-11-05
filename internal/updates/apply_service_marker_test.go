// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/updates"
)

// The version a service update writes at is on the run before the write, so a restart can tell a service the run wrote from one it
// never touched (CR-02). The run keeps it after the update ends.

// The run of an update carries the version of the service as it was read for the write, which is the version the write was sent at.
func TestAServiceUpdateRecordsTheVersionItWritesAt(t *testing.T) {
	t.Parallel()
	svc := swarmService("web", 'a')
	svc.Version = 7
	r := newApplyRig(t, nil, svc)
	r.offer("web", dg('b'))

	job := r.run(t, r.request("web", dg('b')))
	run := r.runRow(t, job)
	if run.State != updates.RunDone {
		t.Fatalf("run = %s %q, want done", run.State, run.Reason)
	}
	sent := r.fake.ServiceUpdates()
	if len(sent) != 1 {
		t.Fatalf("service updates = %d, want 1", len(sent))
	}
	if len(run.Items) != 1 || run.Items[0].WriteVersion == nil {
		t.Fatalf("run items = %+v, want one with the version of the write", run.Items)
	}
	if got := strconv.FormatUint(*run.Items[0].WriteVersion, 10); got != "7" || got != sent[0].Version {
		t.Errorf("recorded version = %s, write sent at %s, want both 7", got, sent[0].Version)
	}
}

// A write whose version could not be put on the run is not made: after a restart nothing could say whether it had happened. The run
// ends failed, as one does before any change: no rollback is claimed or audited, nothing is remembered, and the service is as it was.
func TestAServiceUpdateWhoseVersionCannotBeRecordedWritesNothing(t *testing.T) {
	t.Parallel()
	r := newApplyRig(t, nil, swarmService("web", 'a'))
	r.offer("web", dg('b'))
	if _, err := r.st.Write.Exec(`CREATE TRIGGER no_run_items BEFORE UPDATE OF items ON image_update_runs BEGIN SELECT RAISE(ABORT, 'database is locked'); END`); err != nil {
		t.Fatal(err)
	}

	job := r.run(t, r.request("web", dg('b')))

	run := r.runRow(t, job)
	if run.State != updates.RunFailed || run.Reason != updates.ReasonEngine || run.FinishedAt.IsZero() {
		t.Fatalf("run = %s %q, want failed with engine_error", run.State, run.Reason)
	}
	if run.Items[0].WriteVersion != nil {
		t.Errorf("run items = %+v, want no version recorded", run.Items)
	}
	if n := len(r.fake.ServiceUpdates()); n != 0 {
		t.Errorf("service updates = %d, want none: the version of the write was not on record", n)
	}
	states := map[string]string{}
	codes := map[string]string{}
	for _, it := range job.Items {
		states[it.ID], codes[it.ID] = it.State, it.Code
	}
	if states[updates.ItemUpdate] != jobs.ItemFailed || codes[updates.ItemUpdate] != updates.CodeNotRecorded || states[updates.ItemWait] != jobs.ItemSkipped {
		t.Errorf("steps = %v codes %v, want the update failed as not recorded and the wait skipped", states, codes)
	}
	for _, it := range job.Items {
		if it.ID == updates.ItemRollback || it.ID == updates.ItemRollbackWait {
			t.Errorf("job has a %s step, want no rollback", it.ID)
		}
	}
	if rows := r.audits(t, "image.update_rolled_back"); len(rows) != 0 {
		t.Errorf("rollback audit rows = %+v, want none: nothing was changed", rows)
	}
	if rows := r.audits(t, "image.update_failed"); len(rows) != 1 || rows[0]["reason"] != updates.ReasonEngine {
		t.Errorf("failure audit rows = %+v, want one", rows)
	}
	if f, ok := r.failureOf(t, "web"); ok {
		t.Errorf("failure = %+v, want nothing remembered: the new digest was never tried", f)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is still held after the job ended")
	}
}

// Each item of a bulk update goes through the same update, so its run carries the version its write was made at too.
func TestEachItemOfABulkUpdateRecordsTheVersionItWritesAt(t *testing.T) {
	t.Parallel()
	web, api := swarmService("web", 'a'), swarmService("api", 'a')
	web.Version, api.Version = 7, 11
	r := newApplyRig(t, nil, web, api)
	r.offer("web", dg('b'))
	r.offer("api", dg('b'))

	job, _ := r.runBulk(t, r.bulkRequest(dg('b'), "web", "api"))
	if job.State != jobs.JobDone {
		t.Fatalf("job = %s, want done", job.State)
	}
	rows, err := r.st.Read.Query(`SELECT id FROM image_update_runs WHERE bulk_id = 'bulk-1'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]uint64{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		run, err := r.runs.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Items) != 1 || run.Items[0].WriteVersion == nil {
			t.Errorf("run of %s items = %+v, want one with the version of the write", run.Workload, run.Items)
			continue
		}
		got[run.Workload] = *run.Items[0].WriteVersion
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["web"] != 7 || got["api"] != 11 {
		t.Errorf("versions recorded = %v, want web at 7 and api at 11", got)
	}
}
