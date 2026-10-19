// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Checking a stack's working copy (plan 07-06, STK-01): the editor sends what it holds, the server
// answers every problem with its file and line, and the check writes and sends nothing.

type checkProblem struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"endLine"`
	EndCol   int    `json:"endCol"`
}

type checkAnswer struct {
	Problems    []checkProblem `json:"problems"`
	UnusedFiles []string       `json:"unusedFiles"`
}

func (c *roleCaller) check(stackID, body string) (checkAnswer, int, string) {
	c.t.Helper()
	rec := c.do(http.MethodPost, "stacks/"+stackID+"/check", body)
	var a checkAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
			c.t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return a, rec.Code, rec.Body.String()
}

func checkBody(compose string, extra ...draftFile) string {
	files := append([]draftFile{{Path: "compose.yaml", Content: compose}}, extra...)
	b, _ := json.Marshal(struct {
		Files []draftFile `json:"files"`
		Vars  []struct{}  `json:"vars"`
	}{files, []struct{}{}})
	return string(b)
}

// engineStateReads counts the requests that read the resources a stack touches. The monitor probes
// /_ping and /info; nothing but a request that reads these goes to the lists.
func engineStateReads(requests []string) int {
	n := 0
	for _, p := range requests {
		for _, tail := range []string{"/services", "/networks", "/secrets", "/configs", "/volumes", "/containers/json", "/nodes", "/tasks"} {
			if strings.HasSuffix(p, tail) {
				n++
			}
		}
	}
	return n
}

func TestCheckIsPureAndPositioned(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "lint")

	draftBefore := readDraft(t, viewer, st.ID)
	auditBefore := len(f.stackAudits(t))
	readsBefore := engineStateReads(f.fake.Requests())

	// A tab as the indentation of line 2: the problem is the file, the line and the sentence.
	answer, code, raw := viewer.check(st.ID, checkBody("services:\n\tweb:\n    image: nginx\n"))
	if code != http.StatusOK {
		t.Fatalf("check by a viewer: %d %s, want 200 (stacks.read is enough)", code, raw)
	}
	if len(answer.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", answer.Problems)
	}
	p := answer.Problems[0]
	if p.Severity != "error" || p.Code != "yaml_invalid" || p.File != "compose.yaml" || p.Line != 2 ||
		p.Message != "Line 2: found character that cannot start any token" {
		t.Errorf("problem = %+v", p)
	}

	// A schema error is at the line of its key.
	answer, code, raw = viewer.check(st.ID, checkBody("services:\n  web:\n    image: nginx\n    imag: x\n"))
	if code != http.StatusOK || len(answer.Problems) != 1 || answer.Problems[0].Line != 4 || answer.Problems[0].Code != "schema_invalid" {
		t.Errorf("schema check: %d %s, want one schema_invalid at line 4", code, raw)
	}

	// A good file has no problems, and the answer always carries both lists.
	answer, code, raw = viewer.check(st.ID, checkBody("services:\n  web:\n    image: nginx:1.27\n"))
	if code != http.StatusOK || len(answer.Problems) != 0 || answer.UnusedFiles == nil {
		t.Errorf("clean check: %d %s", code, raw)
	}
	if !strings.Contains(raw, `"problems":[]`) || !strings.Contains(raw, `"unusedFiles":[]`) {
		t.Errorf("a clean answer must carry empty lists, not null: %s", raw)
	}

	// Pure: nothing written, nothing sent to an engine, no audit row, the draft where it was.
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a check must change nothing", w)
	}
	if got := engineStateReads(f.fake.Requests()); got != readsBefore {
		t.Errorf("the check read the engine: %d state reads, was %d", got, readsBefore)
	}
	if after := len(f.stackAudits(t)); after != auditBefore {
		t.Errorf("audit rows %d -> %d: a check must write none", auditBefore, after)
	}
	if d := readDraft(t, viewer, st.ID); d.Version != draftBefore.Version {
		t.Errorf("draft version %d -> %d: a check must not save", draftBefore.Version, d.Version)
	}
}

func TestCheckRefusesWhatSaveRefuses(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "rules")

	_, code, raw := admin.check(st.ID, checkBody("services: {}\n", draftFile{Path: "../x"}))
	if code != http.StatusUnprocessableEntity || !strings.Contains(raw, "file_path_invalid") {
		t.Errorf("a bad path: %d %s, want 422 file_path_invalid", code, raw)
	}
	_, code, _ = admin.check("no-such-stack", checkBody("services: {}\n"))
	if code != http.StatusNotFound {
		t.Errorf("a stack that is not there: %d, want 404", code)
	}
}
