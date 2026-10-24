// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The History tab of a stack (plan 08-01, STK-08, D-04, D-05): its revisions newest first with how each came to
// be, the result and the image changes. Docker is a fake swarm behind the real monitor, aggregator and cluster
// reader.

type stackRevisionsAnswer struct {
	Items []struct {
		Seq           int64  `json:"seq"`
		Origin        string `json:"origin"`
		RestoredFrom  int64  `json:"restoredFrom"`
		Outcome       string `json:"outcome"`
		CreatedBy     string `json:"createdBy"`
		CreatedAt     string `json:"createdAt"`
		FailedService string `json:"failedService"`
		Code          string `json:"code"`
		Reason        string `json:"reason"`
		Deployed      bool   `json:"deployed"`
		Images        []struct {
			Service, From, To string
		} `json:"images"`
		Pinned      int    `json:"pinned"`
		AdoptedFrom string `json:"adoptedFrom"`
	} `json:"items"`
	Capped bool `json:"capped"`
}

func TestStackRevisionsList(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "hist")

	// A stack with no revision answers an empty list, not an error.
	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("revisions of a new stack: %d %s, want 200 and no items", rec.Code, rec.Body)
	}

	// Two good deploys, then a roll back of the first: three revisions, the last one a copy of 1.
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	rb := operator.do(http.MethodPost, rollbackPath(st.ID), "")
	if rb.Code != http.StatusAccepted {
		t.Fatalf("roll back: %d %s", rb.Code, rb.Body)
	}
	if job := f.awaitDeploy(t, answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rb).JobID); job.State != "done" {
		t.Fatalf("roll back job = %+v", job)
	}

	// A Viewer may read the list.
	rec = viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	got := answerOf[stackRevisionsAnswer](t, rec)
	if len(got.Items) != 3 || got.Capped {
		t.Fatalf("revisions = %+v, want 3, not capped", got)
	}
	for i, want := range []struct {
		seq          int64
		origin       string
		restoredFrom int64
		deployed     bool
	}{{3, "rollback", 1, true}, {2, "deploy", 0, false}, {1, "deploy", 0, false}} {
		it := got.Items[i]
		if it.Seq != want.seq || it.Origin != want.origin || it.RestoredFrom != want.restoredFrom || it.Deployed != want.deployed || it.Outcome != "good" || it.CreatedBy != "operator-1" || it.CreatedAt == "" {
			t.Errorf("item %d = %+v, want seq %d %s of %d, deployed %v, good, by operator-1", i, it, want.seq, want.origin, want.restoredFrom, want.deployed)
		}
	}
	// Revision 2 moved the image from 1.27 to 1.28; the first revision pinned one image and has nothing to compare with.
	if im := got.Items[1].Images; len(im) != 1 || im[0].Service != "web" || !strings.HasPrefix(im[0].From, "nginx:1.27") || !strings.HasPrefix(im[0].To, "nginx:1.28") {
		t.Errorf("revision 2 images = %+v, want web from 1.27 to 1.28", im)
	}
	if got.Items[2].Pinned != 1 || len(got.Items[2].Images) != 0 {
		t.Errorf("revision 1 = %+v, want 1 image pinned and no change", got.Items[2])
	}

	// A stack of another environment is not found, whoever asks.
	ctx := testsupport.Context(t)
	if _, err := f.st.Write.ExecContext(ctx, `INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at) VALUES ('env-elsewhere', 'agent', 'elsewhere', '', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	foreign, _, err := repo.CreateStack(ctx, "env-elsewhere", "foreign", stacks.TargetSwarm, "ada", stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte("services: {}\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{foreign.ID, st.ID + "x", "not-a-stack"} {
		rec = viewer.do(http.MethodGet, "stacks/"+id+"/revisions", "")
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
			t.Errorf("revisions of %s: %d %s, want 404 stack_not_found", id, rec.Code, rec.Body)
		}
	}
}

func TestStackRevisionsListAFailedRevisionWithItsReason(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "failing")
	ctx := testsupport.Context(t)
	repo := stackops.Repo{Store: f.st, Vault: f.v}
	rev, err := repo.NewRevision(ctx, st.ID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeFailed, "", ""); err != nil {
		t.Fatal(err)
	}
	d, err := repo.StartDeploy(ctx, st.ID, e.ID, stackops.DeployKindDeploy, rev.Seq, "job-1", "ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.FinishDeploy(ctx, d.ID, stackops.DeployFailed, "web", stackops.CodeServiceRejected); err != nil {
		t.Fatal(err)
	}

	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body)
	}
	got := answerOf[stackRevisionsAnswer](t, rec)
	if len(got.Items) != 1 {
		t.Fatalf("revisions = %+v", got)
	}
	it := got.Items[0]
	if it.Outcome != "failed" || it.FailedService != "web" || it.Code != stackops.CodeServiceRejected || it.Reason != stackops.CodeMessage(stackops.CodeServiceRejected) || it.Reason == "" || it.Deployed {
		t.Errorf("item = %+v, want failed on web with the sentence of service_rejected, not deployed", it)
	}
}
