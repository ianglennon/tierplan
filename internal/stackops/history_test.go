// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

func digest(c string) string { return "web:1@sha256:" + strings.Repeat(c, 64) }

func TestListRevisionsNewestFirstWithReasonsAndImageChanges(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	// 1: good, pins web and db.
	r1 := goodRevision(t, f, st.ID, "ada")
	if err := f.repo.SetPins(ctx, r1.ID, map[string]string{"web": digest("a"), "db": digest("d")}); err != nil {
		t.Fatal(err)
	}
	// 2: failed on service web with service_rejected.
	r2, err := f.repo.NewRevision(ctx, st.ID, "bob", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, r2.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}
	d2, err := f.repo.StartDeploy(ctx, st.ID, f.envID, stackops.DeployKindDeploy, r2.Seq, "job-2", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.FinishDeploy(ctx, d2.ID, stackops.DeployFailed, "web", stackops.CodeServiceRejected); err != nil {
		t.Fatal(err)
	}
	// 3: good, the web digest changed.
	r3, err := f.repo.NewRevision(ctx, st.ID, "cyd", stackops.OriginUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetPins(ctx, r3.ID, map[string]string{"web": digest("b"), "db": digest("d")}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, r3.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	// 4: a roll back of 1, good.
	r4, err := f.repo.CopyRevision(ctx, r1, "ada", stackops.OriginRollback)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.repo.SetOutcome(ctx, r4.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}

	rows, capped, err := f.repo.ListRevisions(ctx, st.ID)
	if err != nil || capped {
		t.Fatalf("ListRevisions = %d rows, capped %v, %v", len(rows), capped, err)
	}
	var order []int64
	for _, r := range rows {
		order = append(order, r.Seq)
	}
	if len(order) != 4 || order[0] != 4 || order[1] != 3 || order[2] != 2 || order[3] != 1 {
		t.Fatalf("order = %v, want 4 3 2 1", order)
	}
	bySeq := map[int64]stackops.RevisionRow{}
	for _, r := range rows {
		bySeq[r.Seq] = r
	}
	for seq, want := range map[int64]bool{4: true, 3: false, 2: false, 1: false} {
		if bySeq[seq].Deployed != want {
			t.Errorf("revision %d Deployed = %v, want %v", seq, bySeq[seq].Deployed, want)
		}
	}
	if got := bySeq[3]; len(got.Images) != 1 || got.Images[0] != (stackops.ImageChange{Service: "web", From: digest("a"), To: digest("b")}) || got.Origin != stackops.OriginUpdate || got.Pinned != 0 {
		t.Errorf("revision 3 = %+v, want one change of web from a to b", got)
	}
	if got := bySeq[2]; got.Outcome != stackops.OutcomeFailed || got.Code != stackops.CodeServiceRejected || got.FailedService != "web" {
		t.Errorf("revision 2 = %+v, want failed on web with service_rejected", got)
	}
	if got := bySeq[4]; got.Origin != stackops.OriginRollback || got.RestoredFrom != 1 || got.CreatedBy != "ada" {
		t.Errorf("revision 4 = %+v, want a roll back of 1 by ada", got)
	}
	// Revision 4 copied 1's pins, so against good revision 3 it changes web back.
	if got := bySeq[4]; len(got.Images) != 1 || got.Images[0].Service != "web" || got.Images[0].From != digest("b") || got.Images[0].To != digest("a") {
		t.Errorf("revision 4 images = %+v, want web from b back to a", got.Images)
	}
	// The first revision has nothing older to compare with: it reports how many images it pinned.
	if got := bySeq[1]; len(got.Images) != 0 || got.Pinned != 2 {
		t.Errorf("revision 1 = %+v, want no changes and 2 pinned", got)
	}
}

func TestListRevisionsOfAStackWithNoneIsEmpty(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	rows, capped, err := f.repo.ListRevisions(ctx, st.ID)
	if err != nil || capped || len(rows) != 0 {
		t.Errorf("ListRevisions = %v, capped %v, %v, want none", rows, capped, err)
	}
}

func TestListRevisionsCapsAtTheMostItListsAndNeverRewrites(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{compose("services: {}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < stackops.MaxRevisionsListed+3; i++ {
		goodRevision(t, f, st.ID, "ada")
	}
	rows, capped, err := f.repo.ListRevisions(ctx, st.ID)
	if err != nil || !capped || len(rows) != stackops.MaxRevisionsListed {
		t.Fatalf("ListRevisions = %d rows, capped %v, %v, want %d capped", len(rows), capped, err, stackops.MaxRevisionsListed)
	}
	if rows[0].Seq != int64(stackops.MaxRevisionsListed+3) || !rows[0].Deployed {
		t.Errorf("newest = %+v, want the last revision, deployed", rows[0])
	}
	again, _, err := f.repo.ListRevisions(ctx, st.ID)
	if err != nil || len(again) != len(rows) || again[0].Seq != rows[0].Seq || again[len(again)-1].Seq != rows[len(rows)-1].Seq {
		t.Errorf("a second read differs from the first (%v)", err)
	}
}
