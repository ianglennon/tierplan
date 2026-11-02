// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package gitsync_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ianglennon/tierplan/internal/gitconn"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/gitx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// Pointing an inline stack at a repository (plan 09-30, GIT-02, D-24): the draft is written and nothing is applied; a good
// deploy of it makes the stack a git stack and keeps its revisions.

// inlineStack makes an inline stack with a plain file, a secret file and two variables, and gives it two good revisions.
func (f *checkFixture) inlineStack(t *testing.T) stackops.Stack {
	t.Helper()
	ctx := context.Background()
	st, _, err := f.rr.CreateStack(ctx, "env-1", "legacy", stacks.TargetStandalone, "ada", stacks.WorkingCopy{
		Files: []stacks.File{
			{Path: "compose.yaml", Content: []byte("services:\n  web:\n    image: nginx:1.25\n")},
			{Path: "notes.txt", Content: []byte("hello\n")},
			{Path: "web.env", Content: []byte("OLD=1\n")},
			{Path: "secrets/db.txt", Content: []byte("sealed"), Secret: true},
		},
		Vars: []stacks.Var{{Name: "TAG", Value: "9"}, {Name: "TOKEN", Value: "s3cret-token", Secret: true}},
	})
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	for range 2 {
		rev, err := f.rr.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
		if err != nil {
			t.Fatalf("NewRevision: %v", err)
		}
		if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
			t.Fatalf("SetOutcome: %v", err)
		}
	}
	return st
}

func (f *checkFixture) convertRequest(version int64, files ...gitsync.FileChoice) gitsync.ConvertRequest {
	return gitsync.ConvertRequest{
		SourceChange: gitsync.SourceChange{RepoID: f.id, RefKind: gitx.RefBranch, Ref: "main", Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}},
		Version:      version, Files: files,
	}
}

func TestConvertWritesTheDraftAndAGoodDeployMakesTheStackGit(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	st := f.inlineStack(t)
	draft, err := f.rr.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Convert(ctx, "env-1", st.ID, "bob", f.convertRequest(draft.Version,
		gitsync.FileChoice{Path: "notes.txt"}, gitsync.FileChoice{Path: "secrets/db.txt", Keep: true}))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if res.DraftVersion != draft.Version+1 || res.Commit.SHA != f.sha || res.Ref != "main" || res.Kept != 1 || res.Dropped != 3 {
		t.Errorf("result = %+v, want version %d at %s on main, 1 kept and 3 dropped", res, draft.Version+1, f.sha)
	}

	// The draft holds the repository's files, the kept file as a sealed overlay and the variables; the rest is dropped.
	got, err := f.rr.Draft(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for _, file := range got.Copy.Files {
		seen = append(seen, file.Path+":"+string(file.Origin))
		if file.Path == "secrets/db.txt" && (!file.Secret || string(file.Content) != "sealed") {
			t.Errorf("kept file = %q secret %v, want its content, sealed", file.Content, file.Secret)
		}
	}
	slices.Sort(seen)
	if want := []string{".env:repo", "compose.yaml:repo", "secrets/db.txt:overlay", "web.env:repo"}; !slices.Equal(seen, want) {
		t.Errorf("draft files = %v, want %v", seen, want)
	}
	if v := varText(got.Copy.Vars, "TOKEN"); v != "s3cret-token" || varText(got.Copy.Vars, "TAG") != "9" {
		t.Errorf("draft vars = %+v, want both kept", got.Copy.Vars)
	}
	if got.Git.Commit != f.sha || got.Git.Source.Folder != "stacks/web" {
		t.Errorf("draft git = %+v, want the commit and source", got.Git)
	}

	// Nothing is applied: still inline, no git settings, no sync state, two revisions.
	if _, err := f.rr.StackGit(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit = %v, want ErrNoGit until a deploy", err)
	}
	if revs, _, err := f.rr.ListRevisions(ctx, st.ID); err != nil || len(revs) != 2 {
		t.Errorf("revisions = %d, %v, want 2", len(revs), err)
	}

	// A deploy that fails changes nothing; a good one makes the stack a git stack.
	failed, err := f.rr.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rr.SetOutcome(ctx, failed.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rr.StackGit(ctx, st.ID); !errors.Is(err, stackops.ErrNoGit) {
		t.Errorf("StackGit after a failed deploy = %v, want ErrNoGit", err)
	}
	rev, err := f.rr.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rr.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatal(err)
	}
	f.svc.Deployed(ctx, st.ID, rev)

	sg, err := f.rr.StackGit(ctx, st.ID)
	if err != nil || sg.RepoID != f.id || sg.Ref != "main" || sg.Folder != "stacks/web" || sg.PendingConvert {
		t.Fatalf("StackGit after the deploy = %+v, %v, want the source", sg, err)
	}
	state, err := f.rr.GitState(ctx, st.ID)
	if err != nil || state.Status != stackops.GitStatusInSync || state.DeployedCommit != f.sha || state.HeadCommit != f.sha || state.Behind != 0 {
		t.Errorf("state = %+v, %v, want in sync at %s", state, err, f.sha)
	}
	read, err := f.rr.Stack(ctx, "env-1", st.ID)
	if err != nil || read.Source.Kind != stacks.SourceGit || read.Source.Ref != f.id {
		t.Errorf("stack = %+v, %v, want a git stack", read, err)
	}
	if revs, _, err := f.rr.ListRevisions(ctx, st.ID); err != nil || len(revs) != 4 {
		t.Errorf("revisions = %d, %v, want the 2 inline ones, the failed attempt and the new one", len(revs), err)
	}
	// A later deploy of the same stack is an ordinary one.
	f.svc.Deployed(ctx, st.ID, rev)
	if state, _ := f.rr.GitState(ctx, st.ID); state.Status != stackops.GitStatusInSync {
		t.Errorf("state after a second report = %+v, want in sync", state)
	}
}

func TestConvertAgainReplacesThePendingSource(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	st := f.inlineStack(t)
	draft, _ := f.rr.Draft(ctx, st.ID)
	first, err := f.svc.Convert(ctx, "env-1", st.ID, "bob", f.convertRequest(draft.Version))
	if err != nil {
		t.Fatal(err)
	}
	dev := f.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=dev\n"}, "dev")
	f.repo.Branch(t, "dev", dev)
	f.repo.Branch(t, "main", f.sha)
	again := f.convertRequest(first.DraftVersion)
	again.Ref = "dev"
	if _, err := f.svc.Convert(ctx, "env-1", st.ID, "bob", again); err != nil {
		t.Fatalf("second Convert: %v", err)
	}
	pending, ok, err := f.rr.PendingGit(ctx, st.ID)
	if err != nil || !ok || pending.Ref != "dev" {
		t.Errorf("PendingGit = %+v, %v, %v, want dev", pending, ok, err)
	}
	if d, _ := f.rr.Draft(ctx, st.ID); d.Git.Commit != dev {
		t.Errorf("draft commit = %q, want dev's %s", d.Git.Commit, dev)
	}
}

func TestConvertRefusals(t *testing.T) {
	t.Parallel()
	f := newCheckFixture(t, nil, nil)
	ctx := context.Background()
	st := f.inlineStack(t)
	draft, _ := f.rr.Draft(ctx, st.ID)

	var conflict *stackops.DraftConflictError
	var clash *gitsync.OverlayClashError
	var fe *stacks.FileError
	other := "env-2"
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES ('env-2', 'agent', 'two', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	foreign := f.addRepo(t, &other, "theirs")
	tests := []struct {
		name string
		req  gitsync.ConvertRequest
		stk  string
		is   func(error) bool
	}{
		{"a git stack", f.convertRequest(1), f.stackID, func(err error) bool { return errors.Is(err, stackops.ErrAlreadyGit) }},
		{"a draft that moved", f.convertRequest(draft.Version + 3), st.ID, func(err error) bool { return errors.As(err, &conflict) }},
		{"a kept file the repository holds", f.convertRequest(draft.Version, gitsync.FileChoice{Path: "web.env", Keep: true}), st.ID,
			func(err error) bool {
				return errors.As(err, &clash) && clash.Path == "web.env" && errors.Is(err, gitsync.ErrOverlayClash)
			}},
		{"a file that is not the stack's", f.convertRequest(draft.Version, gitsync.FileChoice{Path: "nope.txt", Keep: true}), st.ID,
			func(err error) bool { return errors.As(err, &fe) && errors.Is(err, stacks.ErrPathInvalid) }},
		{"a file named twice", f.convertRequest(draft.Version, gitsync.FileChoice{Path: "notes.txt"}, gitsync.FileChoice{Path: "notes.txt", Keep: true}), st.ID,
			func(err error) bool { return errors.Is(err, stacks.ErrPathTaken) }},
		{"another environment's repository", func() gitsync.ConvertRequest {
			r := f.convertRequest(draft.Version)
			r.RepoID = foreign
			return r
		}(), st.ID, func(err error) bool { return errors.Is(err, gitconn.ErrNotFound) }},
		{"a compose file the folder lacks", func() gitsync.ConvertRequest {
			r := f.convertRequest(draft.Version)
			r.ComposeFiles = []string{"nope.yaml"}
			return r
		}(), st.ID, func(err error) bool { var pe *gitsync.PathError; return errors.As(err, &pe) }},
	}
	for _, tc := range tests {
		if _, err := f.svc.Convert(ctx, "env-1", tc.stk, "bob", tc.req); err == nil || !tc.is(err) {
			t.Errorf("%s: err = %v, want the refusal", tc.name, err)
		}
	}
	// Every refusal left the stack as it was.
	if d, _ := f.rr.Draft(ctx, st.ID); d.Version != draft.Version || d.Git.Commit != "" {
		t.Errorf("draft after the refusals = %+v, want it untouched", d)
	}
	if _, ok, _ := f.rr.PendingGit(ctx, st.ID); ok {
		t.Error("a refused conversion left a pending source")
	}
}

func varText(vars []stacks.Var, name string) string {
	for _, v := range vars {
		if v.Name == name {
			return v.Value
		}
	}
	return ""
}
