// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// goodRevisions takes n revisions of the stack's draft, each marked good, and returns the last.
func goodRevisions(t *testing.T, f repoFixture, stackID string, n int) stackops.Snapshot {
	t.Helper()
	var last stackops.Snapshot
	for range n {
		last = goodRevision(t, f, stackID, "ada")
	}
	return last
}

// An image update gets a revision of its own, copied from the newest good revision: it is "update images", it is not a
// roll back (restored_from stays 0, so History, the review and git sync read an update), it keeps the commit the good
// revision was made from, and it is not the newest good revision until its deploy ends good (D-06, D-08, T-12-112).
func TestNewUpdateRevisionCopiesContent(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	f.addGitRepo(t, gitRepoID, "acme")
	ctx := context.Background()
	st, _, err := f.repo.CreateGitStack(ctx, gitInput(f, "web"))
	if err != nil {
		t.Fatalf("CreateGitStack: %v", err)
	}
	good := goodRevisions(t, f, st.ID, 3)
	if good.Seq != 3 {
		t.Fatalf("good revision = %d, want 3", good.Seq)
	}

	from, ok, err := f.repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !ok || from.Seq != 3 {
		t.Fatalf("LatestGoodRevision = %+v, %v, %v", from, ok, err)
	}
	up, err := f.repo.NewUpdateRevision(ctx, from, "cyd")
	if err != nil {
		t.Fatalf("NewUpdateRevision: %v", err)
	}
	if up.Seq != 4 || up.ID == from.ID || up.Kind != stackops.KindRevision || up.Outcome != stackops.OutcomePending || up.CreatedBy != "cyd" ||
		up.Origin != stackops.OriginUpdate || up.RestoredFrom != 0 || up.BaseSeq != 3 {
		t.Errorf("update = %+v, want revision 4, pending, by cyd, origin update, restored_from 0, base_seq 3", up)
	}

	read, err := f.repo.Revision(ctx, st.ID, up.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if read.Origin != stackops.OriginUpdate || read.RestoredFrom != 0 || read.BaseSeq != 3 {
		t.Errorf("stored update = origin %q, restored_from %d, base_seq %d, want update, 0, 3", read.Origin, read.RestoredFrom, read.BaseSeq)
	}
	if !reflect.DeepEqual(read.Git, gitRevision()) || !reflect.DeepEqual(read.Git, from.Git) {
		t.Errorf("stored git = %+v, want the commit and source of revision 3: %+v", read.Git, gitRevision())
	}
	if !reflect.DeepEqual(read.Copy.Files, from.Copy.Files) || !reflect.DeepEqual(read.Copy.Vars, from.Copy.Vars) ||
		!reflect.DeepEqual(read.Copy.ComposeFiles, from.Copy.ComposeFiles) {
		t.Errorf("stored content = %+v, want %+v", read.Copy, from.Copy)
	}

	// Nothing deploys it yet: the newest good revision is still #3 until the update's deploy marks #4 good.
	still, ok, err := f.repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !ok || still.Seq != 3 {
		t.Errorf("LatestGoodRevision after the copy = %+v, %v, %v, want revision 3", still, ok, err)
	}
	if err := f.repo.SetOutcome(ctx, up.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	if now, _, err := f.repo.LatestGoodRevision(ctx, st.ID); err != nil || now.Seq != 4 {
		t.Errorf("LatestGoodRevision after a good deploy = %+v, %v, want revision 4", now, err)
	}
}

// The update is made from the good revision, never from the draft: an edit not yet deployed neither rides along nor is
// lost (T-12-112).
func TestNewUpdateRevisionLeavesDraft(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, first, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada",
		stacks.WorkingCopy{Files: []stacks.File{compose("services: {a: {image: x}}\n")}, Vars: []stacks.Var{{Name: "K", Value: "deployed"}}})
	if err != nil {
		t.Fatal(err)
	}
	goodRevision(t, f, st.ID, "ada")
	edited, _, err := f.repo.SaveDraft(ctx, st.ID, first.Version, "bob", stacks.WorkingCopy{
		Files: []stacks.File{compose("services: {a: {image: y}}\n")}, Vars: []stacks.Var{{Name: "K", Value: "edited"}},
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}

	from, ok, err := f.repo.LatestGoodRevision(ctx, st.ID)
	if err != nil || !ok {
		t.Fatalf("LatestGoodRevision = %v, %v", ok, err)
	}
	up, err := f.repo.NewUpdateRevision(ctx, from, "cyd")
	if err != nil {
		t.Fatalf("NewUpdateRevision: %v", err)
	}
	read, err := f.repo.Revision(ctx, st.ID, up.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Copy.Compose()) != "services: {a: {image: x}}\n" {
		t.Errorf("update compose = %q, want the good revision's", read.Copy.Compose())
	}
	if len(read.Copy.Vars) != 1 || read.Copy.Vars[0].Value != "deployed" {
		t.Errorf("update vars = %+v, want the good revision's", read.Copy.Vars)
	}

	after, err := f.repo.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.IsDraft || after.Version != edited.Version || after.CreatedBy != "bob" || string(after.Copy.Compose()) != "services: {a: {image: y}}\n" ||
		len(after.Copy.Vars) != 1 || after.Copy.Vars[0].Value != "edited" {
		t.Errorf("draft after the update = %+v, want the edit at version %d untouched", after, edited.Version)
	}
}

// An inline stack has no git record, and the update has none either.
func TestNewUpdateRevisionInline(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetStandalone, "ada",
		stacks.WorkingCopy{Files: []stacks.File{compose("services: {a: {image: x}}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	from := goodRevision(t, f, st.ID, "ada")
	up, err := f.repo.NewUpdateRevision(ctx, from, "cyd")
	if err != nil {
		t.Fatalf("NewUpdateRevision: %v", err)
	}
	read, err := f.repo.Revision(ctx, st.ID, up.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.Git, stackops.GitRevision{}) || !reflect.DeepEqual(up.Git, stackops.GitRevision{}) {
		t.Errorf("git = %+v / %+v, want none for an inline stack", read.Git, up.Git)
	}
	if read.Origin != stackops.OriginUpdate || read.RestoredFrom != 0 {
		t.Errorf("update = origin %q, restored_from %d", read.Origin, read.RestoredFrom)
	}
}

// The update carries the platforms and subnets its source recorded, and nothing when the source predates the record.
func TestNewUpdateRevisionOldPlatforms(t *testing.T) {
	t.Parallel()
	f := newRepo(t)
	ctx := context.Background()
	st, _, err := f.repo.CreateStack(ctx, f.envID, "web", stacks.TargetSwarm, "ada",
		stacks.WorkingCopy{Files: []stacks.File{compose("services: {a: {image: x}}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	old := goodRevision(t, f, st.ID, "ada")
	up, err := f.repo.NewUpdateRevision(ctx, old, "cyd")
	if err != nil {
		t.Fatalf("NewUpdateRevision: %v", err)
	}
	read, err := f.repo.Revision(ctx, st.ID, up.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Platforms) != 0 || len(read.Platforms) != 0 || len(up.Subnets) != 0 || len(read.Subnets) != 0 {
		t.Errorf("update of a revision with no record has platforms %v / %v and subnets %v / %v, want none", up.Platforms, read.Platforms, up.Subnets, read.Subnets)
	}

	// A revision that recorded them hands them on.
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	want := map[string][]swarm.Platform{"web_a": {amd}}
	recorded := goodRevision(t, f, st.ID, "ada")
	if err := f.repo.SetPlatforms(ctx, recorded.ID, want); err != nil {
		t.Fatal(err)
	}
	from, err := f.repo.Revision(ctx, st.ID, recorded.Seq)
	if err != nil {
		t.Fatal(err)
	}
	up2, err := f.repo.NewUpdateRevision(ctx, from, "cyd")
	if err != nil {
		t.Fatal(err)
	}
	read2, err := f.repo.Revision(ctx, st.ID, up2.Seq)
	if err != nil || !reflect.DeepEqual(read2.Platforms, want) || !reflect.DeepEqual(up2.Platforms, want) {
		t.Errorf("update platforms = %v / %v, %v, want %v", up2.Platforms, read2.Platforms, err, want)
	}
}
