// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
	f, e := actionsEnv(t, nil)
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
		"an empty path":        {[]draftFile{composeFile, {Path: ""}}, "file_path_invalid", ""},
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
	f, e := actionsEnv(t, nil)
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
