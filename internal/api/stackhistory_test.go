// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// A revision's card (plan 08-02, STK-08, D-05): its stored rendering, files, variables, pinned images and the
// difference from the deployed revision, with no secret in any of it.

type stackRevisionAnswer struct {
	Item struct {
		Seq      int64 `json:"seq"`
		Deployed bool  `json:"deployed"`
	} `json:"item"`
	DeployedSeq int64  `json:"deployedSeq"`
	Rendered    string `json:"rendered"`
	Files       []struct {
		Path   string `json:"path"`
		Secret bool   `json:"secret"`
	} `json:"files"`
	Vars []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Secret bool   `json:"secret"`
	} `json:"vars"`
	Pins []struct {
		Service string `json:"service"`
		Image   string `json:"image"`
		Digest  string `json:"digest"`
	} `json:"pins"`
	YAML struct {
		ChangedLines int `json:"changedLines"`
		Hunks        []struct {
			Lines []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"lines"`
		} `json:"hunks"`
	} `json:"yaml"`
}

func TestStackRevisionDetail(t *testing.T) {
	t.Parallel()
	const (
		secretValue = "tok-9f8e7d6c5b4a"    //nolint:gosec // a made-up value the test must never find in an answer
		secretFile  = "file-1a2b3c4d5e6f7a" //nolint:gosec // a made-up value the test must never find in an answer
	)
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "card")

	// Two deploys of a stack with a secret variable, a plain one and a secret file.
	compose := func(image string) string {
		return "services:\n  web:\n    image: " + image + "\n    environment:\n      TOKEN: ${TOKEN}\n      PLAIN: ${PLAIN}\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"
	}
	deploy := func(image string) {
		t.Helper()
		cb, _ := json.Marshal(compose(image))
		body := `{"version":` + itoaT(draftVersion(t, operator, st.ID)) + `,"files":[{"path":"compose.yaml","content":` + string(cb) + `,"secret":false},` +
			`{"path":"tok.txt","content":"` + secretFile + `","secret":true}],` +
			`"vars":[{"name":"TOKEN","value":"` + secretValue + `","secret":true},{"name":"PLAIN","value":"visible","secret":false}]}`
		if rec := operator.saveDraft(st.ID, body); rec.Code != http.StatusOK {
			t.Fatalf("save: %d %s", rec.Code, rec.Body)
		}
		if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":`+itoaT(draftVersion(t, operator, st.ID))+`}`); job.State != "done" {
			t.Fatalf("deploy = %+v, want it done", job)
		}
	}
	deploy("nginx:1.27-alpine")
	deploy("nginx:1.28-alpine")

	// A Viewer reads the older revision.
	rec := viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revision 1: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	// No secret reaches the answer, whoever asks.
	for _, c := range []*roleCaller{viewer, operator, admin} {
		body := c.do(http.MethodGet, "stacks/"+st.ID+"/revisions/1", "").Body.String()
		for _, secret := range []string{secretValue, secretFile} {
			if strings.Contains(body, secret) {
				t.Fatalf("a secret reached the answer: %s", body)
			}
		}
	}
	got := answerOf[stackRevisionAnswer](t, rec)
	if got.Item.Seq != 1 || got.Item.Deployed || got.DeployedSeq != 2 {
		t.Errorf("revision = seq %d deployed %v, deployedSeq %d, want 1, not deployed, 2 deployed", got.Item.Seq, got.Item.Deployed, got.DeployedSeq)
	}
	// The files: paths and whether secret, nothing else (the answer has no field for a file's content).
	files := map[string]bool{}
	for _, fl := range got.Files {
		files[fl.Path] = fl.Secret
	}
	if len(files) != 2 || files["compose.yaml"] || !files["tok.txt"] {
		t.Errorf("files = %+v, want compose.yaml plain and tok.txt secret", got.Files)
	}
	if strings.Contains(rec.Body.String(), `"content"`) {
		t.Errorf("the answer has a content field: %s", rec.Body)
	}
	// The variables: a secret value reads <hidden>, a plain one is as stored.
	vars := map[string]string{}
	for _, v := range got.Vars {
		vars[v.Name] = v.Value
		if v.Secret != (v.Name == "TOKEN") {
			t.Errorf("variable %+v: secret = %v", v, v.Secret)
		}
	}
	if vars["TOKEN"] != stacks.Hidden || vars["PLAIN"] != "visible" {
		t.Errorf("variables = %v, want TOKEN <hidden> and PLAIN visible", vars)
	}
	// The pins are named as the compose file names the service, with their digest.
	if len(got.Pins) != 1 || got.Pins[0].Service != "web" || !strings.HasPrefix(got.Pins[0].Image, "nginx:1.27") || !strings.HasPrefix(got.Pins[0].Digest, "sha256:") {
		t.Errorf("pins = %+v, want web pinned to nginx:1.27 with a sha256 digest", got.Pins)
	}
	// The rendering is the stored one: the older image, the secret as <hidden>.
	if !strings.Contains(got.Rendered, "nginx:1.27") || strings.Contains(got.Rendered, "nginx:1.28") || !strings.Contains(got.Rendered, stacks.Hidden) {
		t.Errorf("rendered = %q, want the 1.27 rendering with the secret hidden", got.Rendered)
	}
	// The diff runs from the deployed rendering (1.28) to this one (1.27).
	var added, removed int
	for _, h := range got.YAML.Hunks {
		for _, l := range h.Lines {
			switch {
			case l.Kind == "added" && strings.Contains(l.Text, "nginx:1.27"):
				added++
			case l.Kind == "removed" && strings.Contains(l.Text, "nginx:1.28"):
				removed++
			}
		}
	}
	if added != 1 || removed != 1 || got.YAML.ChangedLines < 2 {
		t.Errorf("yaml diff = %+v, want 1.28 removed and 1.27 added", got.YAML)
	}

	// The deployed revision has no difference from itself.
	deployed := answerOf[stackRevisionAnswer](t, viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions/2", ""))
	if !deployed.Item.Deployed || len(deployed.YAML.Hunks) != 0 || deployed.YAML.ChangedLines != 0 {
		t.Errorf("deployed revision = %+v, want deployed with no diff", deployed)
	}

	// A number the stack does not hold is gone; so is one that is not a number at all.
	rec = viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions/99", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "revision_not_found" || !strings.Contains(rec.Body.String(), "That revision is gone.") {
		t.Errorf("revision 99: %d %s, want 404 revision_not_found", rec.Code, rec.Body)
	}
	for _, seq := range []string{"0", "-1", "x"} {
		if rec = viewer.do(http.MethodGet, "stacks/"+st.ID+"/revisions/"+seq, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("revision %s: %d %s, want 422", seq, rec.Code, rec.Body)
		}
	}

	// A stack of another environment is not found, whoever asks, and so is a revision read through it.
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
		rec = viewer.do(http.MethodGet, "stacks/"+id+"/revisions/1", "")
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
			t.Errorf("revision 1 of %s: %d %s, want 404 stack_not_found", id, rec.Code, rec.Body)
		}
	}
}

// The Changes tab of a revision (plan 08-04, STK-08, D-05, D-47, AUTH-07): what rolling back to a revision would
// change against what runs, read by anyone who may read stacks, with the live values a reader may not reveal
// withheld.

type revisionChangesAnswer struct {
	DeployedSeq int64 `json:"deployedSeq"`
	Services    []struct {
		Name, Short, Change string
		Current, Next       map[string]any
		HiddenChanges       []string `json:"hiddenChanges"`
		Settings            int
	} `json:"services"`
	Summary   struct{ Added, Changed, Removed int } `json:"summary"`
	Identical bool                                  `json:"identical"`
}

func revisionChangesPath(stackID string, seq int) string {
	return "stacks/" + stackID + "/revisions/" + itoaT(int64(seq)) + "/changes"
}

// liveValues are the made-up values of the stack the tests run: they must reach an Admin and never a Viewer.
var liveValues = []string{"key-one-8c1d", "key-two-3f6a", "pw-one-9e2f", "pw-two-5d0b", "u:pw-one-4b7a", "u:pw-two-7c3e"}

func liveValuesCompose(image, n string) string {
	keys := map[string]string{"one": "key-one-8c1d", "two": "key-two-3f6a"}
	pws := map[string]string{"one": "pw-one-9e2f", "two": "pw-two-5d0b"}
	logs := map[string]string{"one": "tcp://u:pw-one-4b7a@h", "two": "tcp://u:pw-two-7c3e@h"}
	return "services:\n  web:\n    image: " + image + "\n    environment:\n      API_KEY: " + keys[n] + "\n      MODE: prod\n" +
		"    command: [\"nginx\", \"--pw=" + pws[n] + "\"]\n    logging:\n      driver: syslog\n      options:\n        syslog-address: " + logs[n] + "\n"
}

func TestRevisionChangesRedactsForAViewer(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "chg")
	f.deployGood(t, operator, st.ID, liveValuesCompose("nginx:1.27-alpine", "one"))
	f.deployGood(t, operator, st.ID, liveValuesCompose("nginx:1.28-alpine", "two"))

	// A Viewer reads what rolling back to revision 1 would change, and sees no live value.
	rec := viewer.do(http.MethodGet, revisionChangesPath(st.ID, 1), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("changes as a viewer: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	for _, v := range liveValues {
		if strings.Contains(rec.Body.String(), v) {
			t.Fatalf("a live value %q reached a viewer: %s", v, rec.Body)
		}
	}
	got := answerOf[revisionChangesAnswer](t, rec)
	if got.DeployedSeq != 2 || got.Identical || len(got.Services) != 1 || got.Services[0].Change != "changed" || got.Summary.Changed != 1 {
		t.Fatalf("changes = %+v, want revision 2 deployed and the one service changed", got)
	}
	svc := got.Services[0]
	if !strings.Contains(rec.Body.String(), "API_KEY=\\u003chidden\\u003e") && !strings.Contains(rec.Body.String(), "API_KEY=<hidden>") {
		t.Errorf("the environment entry is not withheld by name: %s", rec.Body)
	}
	for _, want := range []string{"TaskTemplate.ContainerSpec.Env[API_KEY]", "TaskTemplate.ContainerSpec.Command", "TaskTemplate.LogDriver.Options.syslog-address"} {
		if !slices.Contains(svc.HiddenChanges, want) {
			t.Errorf("hidden changes = %v, want %s listed", svc.HiddenChanges, want)
		}
	}
	// The image change is still shown: it is not a value to withhold.
	if !strings.Contains(rec.Body.String(), "nginx:1.28-alpine") || !strings.Contains(rec.Body.String(), "nginx:1.27-alpine") {
		t.Errorf("the image change is missing: %s", rec.Body)
	}

	// An Admin may reveal, and sees both sides.
	for _, c := range []*roleCaller{admin} {
		rec = c.do(http.MethodGet, revisionChangesPath(st.ID, 1), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("changes as an admin: %d %s", rec.Code, rec.Body)
		}
		for _, v := range []string{"key-one-8c1d", "key-two-3f6a", "pw-one-9e2f", "pw-two-5d0b"} {
			if !strings.Contains(rec.Body.String(), v) {
				t.Errorf("an admin's answer lacks %q: %s", v, rec.Body)
			}
		}
	}
	// An Operator holds no secrets.reveal and is redacted like a Viewer.
	rec = operator.do(http.MethodGet, revisionChangesPath(st.ID, 1), "")
	for _, v := range liveValues {
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), v) {
			t.Fatalf("changes as an operator: %d, a live value %q may have reached them: %s", rec.Code, v, rec.Body)
		}
	}
}

func TestRevisionChangesOfTheDeployedRevisionAndOfOneThatMatchesItAreIdentical(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "same")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)
	f.deployGood(t, operator, st.ID, composeWeb128)

	// The newest is the deployed one; revision 2 ran the same content, so rolling back to either changes nothing.
	for _, seq := range []int{3, 2} {
		rec := viewer.do(http.MethodGet, revisionChangesPath(st.ID, seq), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("changes of %d: %d %s", seq, rec.Code, rec.Body)
		}
		got := answerOf[revisionChangesAnswer](t, rec)
		if !got.Identical || len(got.Services) != 0 || got.DeployedSeq != 3 {
			t.Errorf("changes of %d = %+v, want none against deployed revision 3", seq, got)
		}
	}
	// Revision 1 differs: the tag moved.
	if got := answerOf[revisionChangesAnswer](t, viewer.do(http.MethodGet, revisionChangesPath(st.ID, 1), "")); got.Identical || len(got.Services) != 1 {
		t.Errorf("changes of 1 = %+v, want the one service changed", got)
	}
}

func TestRevisionChangesRefusesAFailedRevision(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "failed")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployThenCancel(t, operator, st, composeWeb128)
	writes := len(f.fake.Writes())

	rec := viewer.do(http.MethodGet, revisionChangesPath(st.ID, 2), "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "revision_not_good" {
		t.Errorf("changes of a failed revision: %d %s, want 409 revision_not_good", rec.Code, rec.Body)
	}
	rec = viewer.do(http.MethodGet, revisionChangesPath(st.ID, 99), "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "revision_not_found" {
		t.Errorf("changes of a revision the stack lacks: %d %s, want 404 revision_not_found", rec.Code, rec.Body)
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
	rec = viewer.do(http.MethodGet, revisionChangesPath(foreign.ID, 1), "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("changes of another environment's stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
	if n := len(f.fake.Writes()); n != writes {
		t.Errorf("writes went from %d to %d, reading the changes writes nothing", writes, n)
	}
}

func TestRevisionChangesIsLimitedPerUser(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	f, e := stackEnvTuned(t, func(f *envFix) {
		// The first two reads are held in flight; the others pass through.
		f.stackOnChanges = func(context.Context) {
			if calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	holder := f.caller(t, e, "holder-1", "viewer")
	other := f.caller(t, e, "other-1", "viewer")
	st := newStack(t, admin, "limited")
	f.deployGood(t, operator, st.ID, composeWeb127)
	f.deployGood(t, operator, st.ID, composeWeb128)

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- holder.do(http.MethodGet, revisionChangesPath(st.ID, 1), "").Code
		}()
	}
	testsupport.Await(t, "the first read to be in flight", entered)
	testsupport.Await(t, "the second read to be in flight", entered)

	// Two are in flight for this user: the third is refused, and says why.
	rec := holder.do(http.MethodGet, revisionChangesPath(st.ID, 1), "")
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "check_busy" {
		t.Errorf("a third read: %d %s, want 429 check_busy", rec.Code, rec.Body)
	}
	// Another user is unaffected.
	if rec := other.do(http.MethodGet, revisionChangesPath(st.ID, 1), ""); rec.Code != http.StatusOK {
		t.Errorf("another user's read: %d %s, want 200", rec.Code, rec.Body)
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a held read ended with %d, want 200", code)
		}
	}
	// The slots are given back.
	if rec := holder.do(http.MethodGet, revisionChangesPath(st.ID, 1), ""); rec.Code != http.StatusOK {
		t.Errorf("a read after the others finished: %d %s, want 200", rec.Code, rec.Body)
	}
}
