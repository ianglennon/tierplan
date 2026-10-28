// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Retention (plan 08-06, STK-08, D-01): the newest N revisions, the deployed one and a pending one stay.

// prunable is a stack whose revisions carry a secret variable and a secret file, so a pruned revision
// leaves sealed rows behind unless the delete takes them.
func prunable(t *testing.T, f repoFixture, name string) stackops.Stack {
	t.Helper()
	wc := stacks.WorkingCopy{
		Files: []stacks.File{compose("services: {}\n")},
		Vars:  []stacks.Var{{Name: "TOKEN", Value: "hunter2", Secret: true}},
	}
	st, _, err := f.repo.CreateStack(context.Background(), f.envID, name, stacks.TargetSwarm, "ada", wc)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// revisions takes count revisions of the stack in order and ends each with outcomes[i] (or good when the
// slice is shorter); a pending outcome is left as it is.
func revisions(t *testing.T, f repoFixture, stackID string, outcomes ...string) []stackops.Snapshot {
	t.Helper()
	ctx := context.Background()
	var out []stackops.Snapshot
	for _, outcome := range outcomes {
		rev, err := f.repo.NewRevision(ctx, stackID, "ada", stackops.OriginDeploy)
		if err != nil {
			t.Fatal(err)
		}
		if outcome != stackops.OutcomePending {
			if err := f.repo.SetOutcome(ctx, rev.ID, outcome, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, rev)
	}
	return out
}

func repeated(outcome string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = outcome
	}
	return out
}

// seqsOf is the seq of each revision the stack still has, oldest first.
func seqsOf(t *testing.T, f repoFixture, stackID string) []int64 {
	t.Helper()
	rows, _, err := f.repo.ListRevisions(context.Background(), stackID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int64, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, rows[i].Seq)
	}
	return out
}

func rangeOf(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func sameSeqs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPruneKeepsTheNewestAndTheDeployed(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st := prunable(t, f, "web")
	revisions(t, f, st.ID, repeated(stackops.OutcomeGood, 22)...)

	n, err := f.repo.Prune(ctx, st.ID, 20)
	if err != nil || n != 2 {
		t.Fatalf("Prune(20) = %d, %v, want 2 deleted", n, err)
	}
	if got := seqsOf(t, f, st.ID); !sameSeqs(got, rangeOf(3, 22)) {
		t.Fatalf("revisions left = %v, want 3 to 22", got)
	}

	// The deployed revision is the newest good one, however old: with ten revisions of which only #3 is good,
	// a limit of 5 keeps #6 to #10 and #3.
	old := prunable(t, f, "old")
	outcomes := repeated(stackops.OutcomeFailed, 10)
	outcomes[2] = stackops.OutcomeGood
	revisions(t, f, old.ID, outcomes...)
	n, err = f.repo.Prune(ctx, old.ID, 5)
	if err != nil || n != 4 {
		t.Fatalf("Prune(5) = %d, %v, want 4 deleted (#1, #2, #4, #5)", n, err)
	}
	if got := seqsOf(t, f, old.ID); !sameSeqs(got, []int64{3, 6, 7, 8, 9, 10}) {
		t.Fatalf("revisions left = %v, want 3 and 6 to 10", got)
	}
	if dep, found, err := f.repo.LatestGoodRevision(ctx, old.ID); err != nil || !found || dep.Seq != 3 {
		t.Fatalf("deployed revision = %+v, %v, %v, want #3 to survive", dep, found, err)
	}
}

func TestPruneKeepsPending(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st := prunable(t, f, "web")
	// #1 is a deploy still running, then twelve that ended: #1 is outside the window and neither good nor failed.
	outcomes := append([]string{stackops.OutcomePending}, repeated(stackops.OutcomeGood, 12)...)
	revisions(t, f, st.ID, outcomes...)

	n, err := f.repo.Prune(ctx, st.ID, 5)
	if err != nil || n != 7 {
		t.Fatalf("Prune(5) = %d, %v, want 7 deleted (#2 to #8)", n, err)
	}
	got := seqsOf(t, f, st.ID)
	if !sameSeqs(got, []int64{1, 9, 10, 11, 12, 13}) {
		t.Fatalf("revisions left = %v, want the pending #1 and #9 to #13", got)
	}
	rev, err := f.repo.Revision(ctx, st.ID, 1)
	if err != nil || rev.Outcome != stackops.OutcomePending {
		t.Fatalf("revision 1 = %+v, %v, want it pending and intact", rev, err)
	}
}

func TestPruneTakesSealedRows(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st := prunable(t, f, "web")
	revs := revisions(t, f, st.ID, repeated(stackops.OutcomeGood, 8)...)
	for _, r := range revs {
		if fileSnapshotCount(t, f, r.ID) == 0 {
			t.Fatalf("revision %d has no file or variable row to prune", r.Seq)
		}
	}

	if n, err := f.repo.Prune(ctx, st.ID, 5); err != nil || n != 3 {
		t.Fatalf("Prune(5) = %d, %v, want 3", n, err)
	}
	for _, r := range revs {
		var snaps int
		row := f.repo.Store.Read.QueryRowContext(ctx, `SELECT COUNT(*) FROM stack_snapshots WHERE id = ?`, r.ID)
		if err := row.Scan(&snaps); err != nil {
			t.Fatal(err)
		}
		left := fileSnapshotCount(t, f, r.ID)
		if r.Seq <= 3 && (snaps != 0 || left != 0) {
			t.Errorf("pruned revision %d left %d snapshot and %d file or variable rows, want none", r.Seq, snaps, left)
		}
		if r.Seq > 3 && (snaps != 1 || left == 0) {
			t.Errorf("kept revision %d has %d snapshot and %d file or variable rows, want its content intact", r.Seq, snaps, left)
		}
	}
	// Nothing sealed is left that no revision or draft owns.
	var orphans int
	row := f.repo.Store.Read.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM stack_snapshot_vars WHERE snapshot_id NOT IN (SELECT id FROM stack_snapshots))
		+ (SELECT COUNT(*) FROM stack_snapshot_files WHERE snapshot_id NOT IN (SELECT id FROM stack_snapshots))`)
	if err := row.Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("%d sealed rows belong to no snapshot (%v)", orphans, err)
	}
	// The draft is not a revision: it stays, with its secret.
	if d, err := f.repo.Draft(ctx, st.ID); err != nil || len(d.Copy.Vars) != 1 || d.Copy.Vars[0].Value != "hunter2" {
		t.Fatalf("draft = %+v, %v, want it untouched", d, err)
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st := prunable(t, f, "web")
	revisions(t, f, st.ID, repeated(stackops.OutcomeGood, 9)...)

	if n, err := f.repo.Prune(ctx, st.ID, 5); err != nil || n != 4 {
		t.Fatalf("first Prune(5) = %d, %v, want 4", n, err)
	}
	if n, err := f.repo.Prune(ctx, st.ID, 5); err != nil || n != 0 {
		t.Fatalf("second Prune(5) = %d, %v, want 0", n, err)
	}
	if n, err := f.repo.PruneAll(ctx, 5); err != nil || n != 0 {
		t.Fatalf("PruneAll(5) afterwards = %d, %v, want 0", n, err)
	}
	if got := seqsOf(t, f, st.ID); !sameSeqs(got, rangeOf(5, 9)) {
		t.Fatalf("revisions left = %v, want 5 to 9", got)
	}
}

func TestPruneAtTheBoundary(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	// Twelve failed revisions, then one good (#13): the deployed revision is the newest, so the rule is the
	// plain window. With 5 kept the 5th newest (#9) stays and the 6th (#8) goes.
	st := prunable(t, f, "web")
	outcomes := append(repeated(stackops.OutcomeFailed, 12), stackops.OutcomeGood)
	revisions(t, f, st.ID, outcomes...)
	if n, err := f.repo.Prune(ctx, st.ID, 5); err != nil || n != 8 {
		t.Fatalf("Prune(5) = %d, %v, want 8 deleted (#1 to #8)", n, err)
	}
	if got := seqsOf(t, f, st.ID); !sameSeqs(got, rangeOf(9, 13)) {
		t.Fatalf("revisions left = %v, want 9 to 13: the 5th newest kept and the 6th gone", got)
	}

	// The 6th newest is kept when it is the newest good revision, and only then.
	g := prunable(t, f, "good-at-six")
	revisions(t, f, g.ID, append([]string{stackops.OutcomeGood}, repeated(stackops.OutcomeFailed, 5)...)...)
	revisions(t, f, g.ID, stackops.OutcomeFailed)
	// #1 good, #2 to #7 failed: with 5 kept the window is #3 to #7, and #1 stays as the deployed revision, #2 goes.
	if n, err := f.repo.Prune(ctx, g.ID, 5); err != nil || n != 1 {
		t.Fatalf("Prune(5) = %d, %v, want 1 deleted (#2)", n, err)
	}
	if got := seqsOf(t, f, g.ID); !sameSeqs(got, []int64{1, 3, 4, 5, 6, 7}) {
		t.Fatalf("revisions left = %v, want 1 (deployed) and 3 to 7", got)
	}

	// A stack with fewer revisions than the limit loses nothing, and one with none is fine.
	few := prunable(t, f, "few")
	revisions(t, f, few.ID, stackops.OutcomeGood, stackops.OutcomeFailed)
	none := prunable(t, f, "none")
	for _, id := range []string{few.ID, none.ID} {
		if n, err := f.repo.Prune(ctx, id, 5); err != nil || n != 0 {
			t.Fatalf("Prune(5) = %d, %v, want 0", n, err)
		}
	}
}

func TestPruneAllAndThePreviewAgree(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	a, b, c := prunable(t, f, "a"), prunable(t, f, "b"), prunable(t, f, "c")
	revisions(t, f, a.ID, repeated(stackops.OutcomeGood, 9)...) // 4 beyond 5
	revisions(t, f, b.ID, repeated(stackops.OutcomeGood, 7)...) // 2 beyond 5
	revisions(t, f, c.ID, repeated(stackops.OutcomeGood, 5)...) // none
	pending := prunable(t, f, "p")                              // a pending revision older than the window stays
	revisions(t, f, pending.ID, append([]string{stackops.OutcomePending}, repeated(stackops.OutcomeGood, 6)...)...)

	revs, stks, err := f.repo.PrunePreview(ctx, 5)
	if err != nil || revs != 7 || stks != 3 {
		t.Fatalf("PrunePreview(5) = %d revisions in %d stacks, %v, want 7 in 3: a 4, b 2, p 1", revs, stks, err)
	}
	if got := seqsOf(t, f, a.ID); len(got) != 9 {
		t.Fatalf("the preview deleted something: a has %v", got)
	}
	n, err := f.repo.PruneAll(ctx, 5)
	if err != nil || n != revs {
		t.Fatalf("PruneAll(5) = %d, %v, want %d as previewed", n, err, revs)
	}
	if revs, stks, err = f.repo.PrunePreview(ctx, 5); err != nil || revs != 0 || stks != 0 {
		t.Fatalf("PrunePreview after = %d, %d, %v, want nothing left to prune", revs, stks, err)
	}
}

func TestPruneRefusesALimitThatKeepsNothing(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st := prunable(t, f, "web")
	revisions(t, f, st.ID, repeated(stackops.OutcomeGood, 3)...)
	if _, err := f.repo.Prune(ctx, st.ID, 0); !errors.Is(err, stackops.ErrKeepTooLow) {
		t.Errorf("Prune(0) = %v, want ErrKeepTooLow", err)
	}
	if _, err := f.repo.PruneAll(ctx, 0); !errors.Is(err, stackops.ErrKeepTooLow) {
		t.Errorf("PruneAll(0) = %v, want ErrKeepTooLow", err)
	}
	if _, _, err := f.repo.PrunePreview(ctx, -1); !errors.Is(err, stackops.ErrKeepTooLow) {
		t.Errorf("PrunePreview(-1) = %v, want ErrKeepTooLow", err)
	}
	if got := seqsOf(t, f, st.ID); len(got) != 3 {
		t.Fatalf("revisions left = %v, want all 3", got)
	}
}
