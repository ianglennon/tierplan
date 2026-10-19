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

	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
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

type checkVariable struct {
	Name       string `json:"name"`
	Value      string `json:"value"`
	Secret     bool   `json:"secret"`
	Source     string `json:"source"`
	Default    string `json:"default"`
	HasDefault bool   `json:"hasDefault"`
	Required   bool   `json:"required"`
	Unused     bool   `json:"unused"`
	Places     []struct {
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"places"`
}

type checkAnswer struct {
	Problems    []checkProblem  `json:"problems"`
	UnusedFiles []string        `json:"unusedFiles"`
	Variables   []checkVariable `json:"variables"`
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

func TestCheckRefusesWhatNamesAFileOfTheServerAndListsUnusedFiles(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "files")

	// include, an env_file outside the stack and a secret file of the server's disk are each a
	// blocking problem at their line, and nothing is read.
	const hostile = "include:\n  - /etc/hostname\nservices:\n  web:\n    image: nginx\n    env_file: /proc/self/environ\nsecrets:\n  s:\n    file: /etc/shadow\n"
	answer, code, raw := admin.check(st.ID, checkBody(hostile))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	type at struct {
		code string
		line int
	}
	var got []at
	for _, p := range answer.Problems {
		got = append(got, at{p.Code, p.Line})
		if p.Severity != "error" || p.File != "compose.yaml" {
			t.Errorf("problem = %+v, want an error in compose.yaml", p)
		}
	}
	if want := []at{{"include_unsupported", 1}, {"path_illegal", 6}, {"path_illegal", 9}}; !slices.Equal(got, want) {
		t.Errorf("problems = %v, want %v", got, want)
	}

	// A file the compose file refers to is used; one it never mentions is listed.
	answer, code, raw = admin.check(st.ID, checkBody("services:\n  web:\n    image: nginx:1.27\n    env_file: app.env\n",
		draftFile{Path: "app.env", Content: "A=1\n"}, draftFile{Path: "notes.txt"}))
	if code != http.StatusOK || len(answer.Problems) != 0 {
		t.Fatalf("check: %d %s", code, raw)
	}
	if len(answer.UnusedFiles) != 1 || answer.UnusedFiles[0] != "notes.txt" {
		t.Errorf("unusedFiles = %v, want [notes.txt]", answer.UnusedFiles)
	}
}

func TestCheckAnswersWhatTheTargetIgnoresAsWarningsAtTheirLines(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "target-lint")

	// build with an image is a warning; a service with no tag is another; both are 200 answers
	// that block nothing, and the error for a build with no image is the only one that blocks.
	answer, code, raw := admin.check(st.ID, checkBody("services:\n  web:\n    image: nginx\n    build: .\n  job:\n    build: ./job\n"))
	if code != http.StatusOK {
		t.Fatalf("check: %d %s", code, raw)
	}
	type at struct {
		severity, code string
		line           int
	}
	var got []at
	for _, p := range answer.Problems {
		got = append(got, at{p.Severity, p.Code, p.Line})
	}
	want := []at{{"warning", "moving_tag", 3}, {"warning", "ignored_on_target", 4}, {"error", "build_without_image", 6}}
	if !slices.Equal(got, want) {
		t.Errorf("problems = %v, want %v", got, want)
	}
}

// checkEnvTuned is actionsEnv with the stack check tuned before the routes are built.
func checkEnvTuned(t *testing.T, tune func(*envFix)) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, swarmManagerEngine())
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	tune(f)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func TestCheckIsLimitedPerUser(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	f, e := checkEnvTuned(t, func(f *envFix) {
		// The first two checks are held in flight; the others pass through.
		f.stackOnCheck = func(context.Context) {
			if calls.Add(1) <= 2 {
				entered <- struct{}{}
				<-release
			}
		}
	})
	admin := f.caller(t, e, "envadmin-1", "admin")
	holder := f.caller(t, e, "holder-1", "operator")
	other := f.caller(t, e, "other-1", "operator")
	st := newStack(t, admin, "limited")

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code, _ := holder.check(st.ID, checkBody(stacksStarter))
			codes <- code
		}()
	}
	testsupport.Await(t, "the first check to be in flight", entered)
	testsupport.Await(t, "the second check to be in flight", entered)

	// Two are in flight for this user: the third is refused, and says why.
	_, code, raw := holder.check(st.ID, checkBody(stacksStarter))
	if code != http.StatusTooManyRequests || !strings.Contains(raw, "check_busy") {
		t.Errorf("a third check: %d %s, want 429 check_busy", code, raw)
	}
	// Another user is unaffected.
	if _, code, raw := other.check(st.ID, checkBody(stacksStarter)); code != http.StatusOK {
		t.Errorf("another user's check: %d %s, want 200", code, raw)
	}

	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a held check ended with %d, want 200", code)
		}
	}
	// The slots are given back.
	if _, code, raw := holder.check(st.ID, checkBody(stacksStarter)); code != http.StatusOK {
		t.Errorf("a check after the others finished: %d %s, want 200", code, raw)
	}
}

func TestCheckPastItsDeadlineAnswersAProblemNotAnError(t *testing.T) {
	t.Parallel()
	f, e := checkEnvTuned(t, func(f *envFix) { f.stackCheckDeadline = 1 }) // 1 ns: over before it starts
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "slow")

	answer, code, raw := admin.check(st.ID, checkBody(stacksStarter))
	if code != http.StatusOK || len(answer.Problems) != 1 {
		t.Fatalf("check: %d %s, want 200 with one problem", code, raw)
	}
	if p := answer.Problems[0]; p.Code != "check_timeout" || p.Severity != "error" || p.File != "compose.yaml" {
		t.Errorf("problem = %+v, want an error check_timeout in compose.yaml", p)
	}
}

const stacksStarter = "services:\n  web:\n    image: nginx:1.27-alpine\n"

// checkBodyWithVars is checkBody with the variables of the table; each is {name, value, secret}.
func checkBodyWithVars(compose, vars string) string {
	b, _ := json.Marshal(compose)
	return `{"files":[{"path":"compose.yaml","content":` + string(b) + `,"secret":false}],"vars":` + vars + `}`
}

func TestCheckIsGivenTheVariablesOfTheTableAndAnswersThemResolved(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "vars")

	answer, code, raw := viewer.check(st.ID, checkBodyWithVars(stacks.StarterCompose, `[]`))
	if code != http.StatusOK || len(answer.Variables) != 1 {
		t.Fatalf("check: %d %s, want one variable", code, raw)
	}
	v := answer.Variables[0]
	if v.Name != "WEB_PORT" || v.Value != "8080" || v.Source != "default" || !v.HasDefault || v.Default != "8080" || v.Required || v.Unused {
		t.Errorf("variable = %+v, want WEB_PORT 8080 from the default", v)
	}
	if len(v.Places) != 1 || v.Places[0].File != "compose.yaml" || v.Places[0].Line != 12 {
		t.Errorf("places = %+v, want compose.yaml line 12", v.Places)
	}

	answer, code, raw = viewer.check(st.ID, checkBodyWithVars(stacks.StarterCompose, `[{"name":"WEB_PORT","value":"9090","secret":false}]`))
	if code != http.StatusOK || len(answer.Variables) != 1 || answer.Variables[0].Value != "9090" || answer.Variables[0].Source != "table" {
		t.Errorf("with WEB_PORT=9090: %d %s, want 9090 from the table", code, raw)
	}

	// The list is never null.
	_, code, raw = viewer.check(st.ID, checkBodyWithVars("services: {}\n", `[]`))
	if code != http.StatusOK || !strings.Contains(raw, `"variables":[]`) {
		t.Errorf("a file with no variables: %d %s, want an empty list", code, raw)
	}
}
