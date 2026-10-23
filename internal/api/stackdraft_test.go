// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The files of a stack's draft (plan 07-03, STK-01, D-05): the server holds every save to the path
// and size rules, whatever the browser checked.

type draftFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Secret  bool   `json:"secret"`
}

func filesBody(t *testing.T, version int64, files ...draftFile) string {
	t.Helper()
	b, err := json.Marshal(struct {
		Version int64       `json:"version"`
		Files   []draftFile `json:"files"`
		Vars    []struct{}  `json:"vars"`
	}{version, files, []struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var composeFile = draftFile{Path: "compose.yaml", Content: "services: {}\n"}

func TestSaveDraftRefusesBadFiles(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "files")
	auditBefore := len(f.stackAudits(t))

	mib := strings.Repeat("x", stacks.MaxFileBytes)
	fifty := []draftFile{composeFile}
	for i := 0; i < stacks.MaxFiles; i++ { // compose.yaml and 50 more: 51 files
		fifty = append(fifty, draftFile{Path: fmt.Sprintf("f%02d.txt", i)})
	}
	heavy := []draftFile{composeFile}
	for i := 0; i < 8; i++ { // 8 MiB of other files and compose.yaml: over the total
		heavy = append(heavy, draftFile{Path: fmt.Sprintf("d%d.bin", i), Content: mib})
	}

	for name, tc := range map[string]struct {
		files    []draftFile
		problem  string
		location string
	}{
		"an absolute path":     {[]draftFile{composeFile, {Path: "/etc/passwd"}}, "file_path_invalid", "/etc/passwd"},
		"a parent directory":   {[]draftFile{composeFile, {Path: "../x"}}, "file_path_invalid", "../x"},
		"a dot segment":        {[]draftFile{composeFile, {Path: "a/./b"}}, "file_path_invalid", "a/./b"},
		"a backslash":          {[]draftFile{composeFile, {Path: `a\b`}}, "file_path_invalid", `a\b`},
		"a drive path":         {[]draftFile{composeFile, {Path: "C:/x"}}, "file_path_invalid", "C:/x"},
		"a control character":  {[]draftFile{composeFile, {Path: "x\u0000y"}}, "file_path_invalid", "x\u0000y"},
		"an empty path":        {[]draftFile{composeFile, {Path: ""}}, "file_path_invalid", "file"},
		"a path of 256":        {[]draftFile{composeFile, {Path: strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 54)}}, "file_path_invalid", strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/" + strings.Repeat("c", 54)},
		"a repeated path":      {[]draftFile{composeFile, {Path: "a.env"}, {Path: "a.env"}}, "file_name_taken", "a.env"},
		"a repeated compose":   {[]draftFile{composeFile, composeFile}, "file_name_taken", "compose.yaml"},
		"a file over 1 MiB":    {[]draftFile{composeFile, {Path: "big.bin", Content: mib + "x"}}, "file_too_large", "big.bin"},
		"a compose over 1 MiB": {[]draftFile{{Path: "compose.yaml", Content: mib + "x"}}, "file_too_large", "compose.yaml"},
		"more than 50 files":   {fifty, "stack_too_large", "files"},
		"more than 8 MiB":      {heavy, "stack_too_large", "files"},
		"no compose file":      {[]draftFile{{Path: "app.env", Content: "A=1"}}, "compose_missing", "compose.yaml"},
		"no files at all":      {nil, "compose_missing", "compose.yaml"},
	} {
		rec := operator.saveDraft(st.ID, filesBody(t, 1, tc.files...))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.problem {
			t.Errorf("%s: %d %.300s, want 422 %s", name, rec.Code, rec.Body, tc.problem)
			continue
		}
		var problem struct {
			Errors []struct{ Location, Message string } `json:"errors"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(problem.Errors) == 0 || problem.Errors[0].Location != tc.location {
			t.Errorf("%s: errors = %+v, want the first at location %q", name, problem.Errors, tc.location)
		}
	}

	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if d := answerOf[draftAnswer](t, rec); d.Version != 1 || len(d.Files) != 1 {
		t.Errorf("draft = %+v: a refused save must not change it", d)
	}
	if after := len(f.stackAudits(t)); after != auditBefore {
		t.Errorf("audit rows %d -> %d: a refused save must write none", auditBefore, after)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a refused save must change nothing on an engine", w)
	}
}

func TestSaveDraftKeepsFiftyFilesInPathOrder(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "fifty")

	files := []draftFile{{Path: "config/nginx.conf", Content: "events {}"}, composeFile, {Path: ".env", Content: "A=1"}}
	for i := 3; i < stacks.MaxFiles; i++ {
		files = append(files, draftFile{Path: fmt.Sprintf("f%02d.txt", i)})
	}
	rec := operator.saveDraft(st.ID, filesBody(t, 1, files...))
	if rec.Code != http.StatusOK {
		t.Fatalf("save of %d files: %d %.300s", len(files), rec.Code, rec.Body)
	}
	rec = operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	d := answerOf[draftAnswer](t, rec)
	if len(d.Files) != stacks.MaxFiles || d.Files[0].Path != ".env" || d.Files[1].Path != "compose.yaml" || d.Files[2].Path != "config/nginx.conf" {
		t.Errorf("draft files = %d, first three %q %q %q", len(d.Files), d.Files[0].Path, d.Files[1].Path, d.Files[2].Path)
	}
}

// Discarding a draft (plan 07-03, STK-05, D-08).

func (c *roleCaller) discardDraft(stackID string) *httptest.ResponseRecorder {
	return c.do(http.MethodDelete, "stacks/"+stackID+"/draft", "")
}

type draftRead struct {
	Version int64  `json:"version"`
	IsDraft bool   `json:"isDraft"`
	SavedBy string `json:"savedBy"`
	BaseSeq int64  `json:"baseSeq"`
	Files   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

func readDraft(t *testing.T, c *roleCaller, stackID string) draftRead {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+stackID+"/draft", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET draft: %d %s", rec.Code, rec.Body)
	}
	return answerOf[draftRead](t, rec)
}

func discardAudits(t *testing.T, f *envFix) []auditRowFull {
	t.Helper()
	var out []auditRowFull
	for _, r := range f.stackAudits(t) {
		if r.Action == audit.ActionStackDraftDiscarded {
			out = append(out, r)
		}
	}
	return out
}

func TestDiscardDraftIsIdempotent(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "scratch")

	if rec := operator.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if d := readDraft(t, operator, st.ID); !d.IsDraft || d.Version != 2 {
		t.Fatalf("draft before the discard = %+v", d)
	}

	rec := operator.discardDraft(st.ID)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("discard: %d %s, want 204", rec.Code, rec.Body)
	}
	rows := discardAudits(t, f)
	if len(rows) != 1 {
		t.Fatalf("%d stack.draft_discarded rows after a real discard, want 1", len(rows))
	}
	if r := rows[0]; r.TargetID != st.ID || r.EnvID != e.ID || r.Result != string(audit.ResultSuccess) || r.Details["stack"] != "scratch" || r.Details["version"] != "2" {
		t.Errorf("audit row = %+v", r)
	}

	// The editor now has the starter for a stack that was never deployed, and it is not a draft.
	d := readDraft(t, operator, st.ID)
	if d.IsDraft || d.Version != 0 || len(d.Files) != 1 || d.Files[0].Path != "compose.yaml" || d.Files[0].Content != stacks.StarterCompose {
		t.Errorf("draft after the discard = %+v, want the starter, not a draft, at version 0", d)
	}

	// Discarding again is the same as discarding once: 204 and no audit row.
	before := len(f.stackAudits(t))
	if rec := operator.discardDraft(st.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("second discard: %d %s, want 204", rec.Code, rec.Body)
	}
	if after := len(f.stackAudits(t)); after != before {
		t.Errorf("audit rows %d -> %d: discarding nothing must write none", before, after)
	}
	if got := len(discardAudits(t, f)); got != 1 {
		t.Errorf("%d discard rows, want still 1", got)
	}
}

func TestDiscardSendsNoEngineRequest(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "quiet-discard")

	operator.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`))
	for i := 0; i < 2; i++ {
		if rec := operator.discardDraft(st.ID); rec.Code != http.StatusNoContent {
			t.Fatalf("discard %d: %d %s, want 204", i+1, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a discard must change nothing on an engine", w)
	}
	for _, p := range f.fake.Requests() {
		if strings.Contains(p, "/services/") || strings.Contains(p, "/networks/") {
			t.Errorf("engine request %q after a save and two discards", p)
		}
	}
}

func TestDiscardRefusals(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "guarded-discard")
	operator.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`))

	if rec := viewer.discardDraft(st.ID); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer discarding: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := operator.discardDraft("no-such-stack"); rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("an unknown stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
	if d := readDraft(t, operator, st.ID); !d.IsDraft || d.Version != 2 {
		t.Errorf("draft = %+v: a refused discard must not touch it", d)
	}
	if got := len(discardAudits(t, f)); got != 0 {
		t.Errorf("%d discard rows after refusals, want 0", got)
	}
}

func TestDiscardingADraftOfADeployedStackShowsTheDeployedDefinition(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "shipped")

	deployed := "services:\n  web:\n    image: nginx:1.27-alpine\n"
	if rec := operator.saveDraft(st.ID, saveBody(1, deployed, `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	rec := operator.deploy(st.ID, `{"draftVersion":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if job := f.awaitDeploy(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	if rec := operator.saveDraft(st.ID, saveBody(2, "services:\n  web:\n    image: nginx:1.28-alpine\n", `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}

	if rec := operator.discardDraft(st.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("discard: %d %s", rec.Code, rec.Body)
	}
	d := readDraft(t, operator, st.ID)
	if d.IsDraft || d.Version != 0 || d.BaseSeq != 1 || len(d.Files) != 1 || d.Files[0].Content != deployed {
		t.Errorf("draft after the discard = %+v, want the deployed definition, not a draft, based on revision 1", d)
	}
	rec = operator.do(http.MethodGet, "stacks", "")
	var list struct {
		Items []stackAnswer `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 || list.Items[0].HasDraft {
		t.Errorf("list = %s (%v), want the stack with no draft pending", rec.Body, err)
	}
}

func TestSavingAfterADiscardStartsAFreshDraftAndAStaleVersionConflicts(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	ada := f.caller(t, e, "ada", "operator")
	bob := f.caller(t, e, "bob", "operator")
	st := newStack(t, admin, "again")

	ada.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`))
	ada.discardDraft(st.ID)

	// Bob still holds the version he read before the discard: he is told, not allowed to recreate it.
	rec := bob.saveDraft(st.ID, saveBody(2, "services:\n  b:\n    image: busybox:2\n", `[]`))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "draft_conflict" {
		t.Fatalf("a save at the discarded version: %d %s, want 409 draft_conflict", rec.Code, rec.Body)
	}
	if d := readDraft(t, bob, st.ID); d.IsDraft {
		t.Errorf("draft = %+v: a refused save must not create one", d)
	}

	// Version 0 is the editor that read the fallback.
	rec = bob.saveDraft(st.ID, saveBody(0, "services:\n  b:\n    image: busybox:2\n", `[]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("a save from the fallback: %d %s", rec.Code, rec.Body)
	}
	if got := answerOf[saveAnswer](t, rec); !got.Changed || got.Version != 2 {
		t.Errorf("save = %+v, want a new draft at version 2", got)
	}
	if d := readDraft(t, ada, st.ID); !d.IsDraft || d.SavedBy != "bob" {
		t.Errorf("draft = %+v, want bob's", d)
	}
}
