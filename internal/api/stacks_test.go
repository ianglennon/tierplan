// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Compose stacks (plan 07-01, STK-01, STK-02, STK-05). Docker is a fake swarm manager behind the real
// monitor, aggregator and cluster reader; the router, policy, CSRF guard, registry resolver, job hub
// and audit writer are the real ones.

// stackEnv is actionsEnv for the stack tests: the services a deploy writes settle at once, as they do
// on a swarm that has the images and room. A test that wants one stuck moves it with Fake.SetTaskState.
func stackEnv(t *testing.T, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	return actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.TasksFollowWrites = true
		if tune != nil {
			tune(c)
		}
	}, services...)
}

// stackEnvTuned is stackEnv for a test that sets a field of the fixture before the router is built.
func stackEnvTuned(t *testing.T, tune func(*envFix)) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine()
	cfg.TasksFollowWrites = true
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	tune(f)
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

type stackAnswer struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Target         string `json:"target"`
	Source         string `json:"source"`
	State          string `json:"state"`
	LastDeployedAt string `json:"lastDeployedAt"`
	LastDeployedBy string `json:"lastDeployedBy"`
	HasDraft       bool   `json:"hasDraft"`
	CreatedBy      string `json:"createdBy"`
	// The services the stack runs of those it has; absent for a stack not deployed and one that cannot be read.
	ServicesRunning *int   `json:"servicesRunning"`
	ServicesTotal   *int   `json:"servicesTotal"`
	Stopped         bool   `json:"stopped"`
	DraftSavedAt    string `json:"draftSavedAt"`
	DraftSavedBy    string `json:"draftSavedBy"`
}

type draftAnswer struct {
	Version int64  `json:"version"`
	SavedBy string `json:"savedBy"`
	Files   []struct {
		Path, Content string
		Secret        bool
	} `json:"files"`
	Vars []struct {
		Name, Value string
		Secret      bool
	} `json:"vars"`
}

type saveAnswer struct {
	Changed bool  `json:"changed"`
	Version int64 `json:"version"`
}

type jobAnswer struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Items []struct {
		ID, State, Code string
	} `json:"items"`
}

func (c *roleCaller) createStack(body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "stacks", body)
}

func (c *roleCaller) saveDraft(stackID, body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPut, "stacks/"+stackID+"/draft", body)
}

func (c *roleCaller) deploy(stackID, body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "stacks/"+stackID+"/deploys", body)
}

func answerOf[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// newStack creates a stack as the env Admin and returns it, failing the test on any other answer.
func newStack(t *testing.T, admin *roleCaller, name string) stackAnswer {
	t.Helper()
	rec := admin.createStack(`{"name":"` + name + `"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create stack: %d %s", rec.Code, rec.Body)
	}
	return answerOf[stackAnswer](t, rec)
}

// saveBody is a save request holding the starter compose file changed by mut.
func saveBody(version int64, compose string, vars string) string {
	b, _ := json.Marshal(compose)
	return `{"version":` + itoaT(version) + `,"files":[{"path":"compose.yaml","content":` + string(b) + `,"secret":false}],"vars":` + vars + `}`
}

func itoaT(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (f *envFix) stackAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'stack.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *envFix) awaitDeploy(t *testing.T, jobID string) jobAnswer {
	t.Helper()
	job, err := f.jobHub.Wait(testsupport.Context(t), jobID)
	if err != nil {
		t.Fatalf("waiting for the deploy job: %v", err)
	}
	var out jobAnswer
	out.ID, out.State = job.ID, job.State
	for _, it := range job.Items {
		out.Items = append(out.Items, struct{ ID, State, Code string }{it.ID, it.State, it.Code})
	}
	return out
}

func TestDeployAOneServiceStackToALocalSwarmManagerEndToEnd(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")

	st := newStack(t, admin, "tracer")
	if st.Target != "swarm" || st.State != "not_deployed" || st.Source != "inline" || st.CreatedBy != "envadmin-1" {
		t.Fatalf("created stack = %+v", st)
	}
	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	draft := answerOf[draftAnswer](t, rec)
	if draft.Version != 1 || len(draft.Files) != 1 || draft.Files[0].Path != "compose.yaml" || draft.Files[0].Content != stacks.StarterCompose {
		t.Fatalf("draft = %+v", draft)
	}

	rec = operator.saveDraft(st.ID, saveBody(1, stacks.StarterCompose+"# tuned\n", `[{"name":"WEB_PORT","value":"9090","secret":false}]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	saved := answerOf[saveAnswer](t, rec)
	if !saved.Changed || saved.Version != 2 {
		t.Fatalf("save = %+v", saved)
	}

	rec = operator.deploy(st.ID, `{"draftVersion":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil || started.JobID == "" || started.Revision != 1 {
		t.Fatalf("deploy answer = %s (%v)", rec.Body, err)
	}
	job := f.awaitDeploy(t, started.JobID)
	if job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}

	nets := f.fake.NetworkCreates()
	if len(nets) != 1 || nets[0].Name != "tracer_default" || nets[0].Labels["bayplan.stack.id"] != st.ID || nets[0].Labels["com.docker.stack.namespace"] != "tracer" {
		t.Errorf("network creates = %+v", nets)
	}
	svcs := f.fake.ServiceCreates()
	if len(svcs) != 1 {
		t.Fatalf("service creates = %+v, want exactly one", svcs)
	}
	spec := svcs[0].Spec
	if spec.Name != "tracer_web" || spec.Labels["com.docker.stack.namespace"] != "tracer" || spec.Labels["bayplan.stack.id"] != st.ID || spec.Labels["com.docker.stack.image"] != "nginx:1.27-alpine" {
		t.Errorf("service spec labels = %+v", spec.Labels)
	}
	if ports := spec.EndpointSpec.Ports; len(ports) != 1 || ports[0].PublishedPort != 9090 || ports[0].TargetPort != 80 {
		t.Errorf("ports = %+v, want the variable from the stack's table (9090)", ports)
	}
	if got := writeVerbs(f.fake.Writes()); !slices.Equal(got, []string{"POST /networks", "POST /services"}) {
		t.Errorf("writes = %v", got)
	}

	rows := f.stackAudits(t)
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action)
	}
	if want := []string{audit.ActionStackCreated, audit.ActionStackDraftSaved, audit.ActionStackDeployed}; !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
	dep := rows[2]
	if dep.Result != string(audit.ResultSuccess) || dep.TargetID != st.ID || dep.EnvID != e.ID {
		t.Errorf("deploy audit row = %+v", dep)
	}
	for k, want := range map[string]string{"stack": "tracer", "mode": "deploy", "result": "done", "revision": "1", "job": started.JobID, "services": "1", "code": ""} {
		if dep.Details[k] != want {
			t.Errorf("deploy audit detail %s = %q, want %q (%+v)", k, dep.Details[k], want, dep.Details)
		}
	}

	// The list reads the stack as deployed, by the operator who deployed it, with no draft pending.
	rec = operator.do(http.MethodGet, "stacks", "")
	var list struct {
		Items []stackAnswer `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list = %s (%v)", rec.Body, err)
	}
	if got := list.Items[0]; got.State != "running" || got.LastDeployedBy != "operator-1" || got.LastDeployedAt == "" || got.HasDraft {
		t.Errorf("listed stack = %+v", got)
	}

	// The job can be read back while the hub keeps it, and only through its own stack.
	rec = operator.do(http.MethodGet, "stacks/"+st.ID+"/deploys/"+started.JobID, "")
	if rec.Code != http.StatusOK {
		t.Errorf("get job: %d %s", rec.Code, rec.Body)
	}
	other := newStack(t, admin, "other")
	rec = operator.do(http.MethodGet, "stacks/"+other.ID+"/deploys/"+started.JobID, "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_job_not_found" {
		t.Errorf("a job of another stack: %d %s, want 404 stack_job_not_found", rec.Code, rec.Body)
	}
}

func TestStackSaveSendsNoEngineRequest(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")

	st := newStack(t, admin, "quiet")
	if rec := operator.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", rec.Code, rec.Body)
	}
	if rec := operator.saveDraft(st.ID, saveBody(2, "services:\n  a:\n    image: busybox:2\n", `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("second save: %d %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("engine writes = %v, a create and a save must change nothing on an engine", w)
	}
	for _, p := range f.fake.Requests() {
		if strings.Contains(p, "/services/create") || strings.Contains(p, "/networks/create") {
			t.Errorf("request %q after a create and two saves", p)
		}
	}
}

func TestStackDraftConflictAndIdempotentSave(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	ada := f.caller(t, e, "ada", "operator")
	bob := f.caller(t, e, "bob", "operator")
	st := newStack(t, admin, "shared")

	body := saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`)
	if rec := ada.saveDraft(st.ID, body); rec.Code != http.StatusOK {
		t.Fatalf("ada's save: %d %s", rec.Code, rec.Body)
	}

	// Bob read version 1 too: his save is refused and names Ada.
	rec := bob.saveDraft(st.ID, saveBody(1, "services:\n  b:\n    image: busybox:9\n", `[]`))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "draft_conflict" {
		t.Fatalf("bob's stale save: %d %s, want 409 draft_conflict", rec.Code, rec.Body)
	}
	var problem struct {
		Errors []struct{ Location, Message string } `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	named := map[string]string{}
	for _, d := range problem.Errors {
		named[d.Location] = d.Message
	}
	if named["savedBy"] != "ada" || named["savedAt"] == "" || named["version"] != "2" {
		t.Errorf("conflict errors = %v, want ada, a time and version 2", named)
	}

	// Saving exactly what is saved changes nothing: no new version, no audit row.
	before := len(f.stackAudits(t))
	rec = ada.saveDraft(st.ID, saveBody(2, "services:\n  a:\n    image: busybox:1\n", `[]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent save: %d %s", rec.Code, rec.Body)
	}
	if got := answerOf[saveAnswer](t, rec); got.Changed || got.Version != 2 {
		t.Errorf("idempotent save = %+v, want changed false at version 2", got)
	}
	if after := len(f.stackAudits(t)); after != before {
		t.Errorf("audit rows %d -> %d: a save that changed nothing must write none", before, after)
	}
	rec = ada.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if d := answerOf[draftAnswer](t, rec); d.Version != 2 || d.SavedBy != "ada" || !strings.Contains(d.Files[0].Content, "busybox:1") {
		t.Errorf("draft = %+v: the refused save must not have changed it", d)
	}
}

func TestStackRefusals(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	viewer := f.caller(t, e, "viewer-1", "viewer")
	st := newStack(t, admin, "guarded")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a viewer saving":      viewer.saveDraft(st.ID, saveBody(1, "services: {}\n", `[]`)),
		"a viewer deploying":   viewer.deploy(st.ID, `{"draftVersion":1}`),
		"an operator creating": operator.createStack(`{"name":"nope"}`),
	} {
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if rec := viewer.do(http.MethodGet, "stacks", ""); rec.Code != http.StatusOK {
		t.Errorf("a viewer listing: %d %s, want 200", rec.Code, rec.Body)
	}

	for _, name := range []string{"Web", "-web", "my stack", "", strings.Repeat("a", 64), "a.b"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		rec := admin.createStack(string(body))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_name_invalid" {
			t.Errorf("name %q: %d %s, want 422 stack_name_invalid", name, rec.Code, rec.Body)
		}
	}
	rec := admin.createStack(`{"name":"guarded"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_name_taken" {
		t.Errorf("a taken name: %d %s, want 409 stack_name_taken", rec.Code, rec.Body)
	}

	// A stack of another environment is not found, whoever asks.
	other := newStack(t, admin, "elsewhere")
	rec = operator.do(http.MethodGet, "stacks/"+other.ID+"x", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("unknown stack: %d %s, want 404 stack_not_found", rec.Code, rec.Body)
	}
	rec = operator.saveDraft("not-a-stack", saveBody(1, "services: {}\n", `[]`))
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "stack_not_found" {
		t.Errorf("saving an unknown stack: %d %s, want 404", rec.Code, rec.Body)
	}
	rows := f.stackAudits(t)
	if len(rows) != 2 { // the two stacks the admin created; a refusal writes no stack row
		t.Errorf("stack audit rows = %d, want 2 (%+v)", len(rows), rows)
	}
}

func TestAStackOfAnotherEnvironmentIsNotFound(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "mine")
	// The same stack ID under a different environment ID resolves no stack, whoever asks.
	rec := f.req(t, http.MethodGet, "/api/env/no-such-env/stacks/"+st.ID, "")
	if rec.code != http.StatusNotFound {
		t.Errorf("another environment: %d %s, want 404", rec.code, rec.body)
	}
}

func TestStackDraftHidesSecretValuesAndKeepsThemWhenSavedAsThePlaceholder(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "vault")

	compose := "services:\n  a:\n    image: busybox:1\n    environment:\n      PW: ${DB_PASSWORD}\n"
	rec := operator.saveDraft(st.ID, saveBody(1, compose, `[{"name":"DB_PASSWORD","value":"s3cr3t-value","secret":true},{"name":"PLAIN","value":"visible","secret":false}]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	rec = operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Fatalf("the draft read holds a secret value: %s", rec.Body)
	}
	d := answerOf[draftAnswer](t, rec)
	vars := map[string]string{}
	for _, v := range d.Vars {
		vars[v.Name] = v.Value
	}
	if vars["DB_PASSWORD"] != "<hidden>" || vars["PLAIN"] != "visible" {
		t.Errorf("vars = %v", vars)
	}

	// Saving the placeholder back keeps the stored secret, and with nothing else changed writes nothing.
	rec = operator.saveDraft(st.ID, saveBody(2, compose, `[{"name":"DB_PASSWORD","value":"<hidden>","secret":true},{"name":"PLAIN","value":"visible","secret":false}]`))
	if rec.Code != http.StatusOK || answerOf[saveAnswer](t, rec).Changed {
		t.Fatalf("placeholder save: %d %s, want 200 and changed false", rec.Code, rec.Body)
	}
	// A placeholder for a secret that was never stored is refused.
	rec = operator.saveDraft(st.ID, saveBody(2, compose, `[{"name":"OTHER","value":"<hidden>","secret":true}]`))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "stack_placeholder_without_value" {
		t.Errorf("placeholder without a value: %d %s, want 422", rec.Code, rec.Body)
	}
	var sealed, plain string
	row := f.st.Read.QueryRow(`SELECT value, value_sealed FROM stack_snapshot_vars WHERE name = 'DB_PASSWORD'`)
	if err := row.Scan(&plain, &sealed); err != nil || plain != "" || sealed == "" || strings.Contains(sealed, "s3cr3t") {
		t.Errorf("stored secret = %q / %q (%v), want it sealed", plain, sealed, err)
	}
	for _, a := range f.stackAudits(t) {
		for k, v := range a.Details {
			if strings.Contains(v, "s3cr3t") || strings.Contains(k, "s3cr3t") {
				t.Errorf("audit row %s holds the secret: %v", a.Action, a.Details)
			}
		}
	}
}

func TestSavingADraftRefusesAPathOrNameThatCouldEscapeTheStack(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "paths")
	compose := `{"path":"compose.yaml","content":"services: {}\n","secret":false}`
	for name, tc := range map[string]struct{ files, vars, problem string }{
		"an absolute path":      {`[` + compose + `,{"path":"/etc/passwd","content":"x","secret":false}]`, `[]`, "file_path_invalid"},
		"a parent directory":    {`[` + compose + `,{"path":"../x","content":"x","secret":false}]`, `[]`, "file_path_invalid"},
		"a dot segment":         {`[` + compose + `,{"path":"a/./b","content":"x","secret":false}]`, `[]`, "file_path_invalid"},
		"a backslash":           {`[` + compose + `,{"path":"a\\b","content":"x","secret":false}]`, `[]`, "file_path_invalid"},
		"a repeated path":       {`[` + compose + `,` + compose + `]`, `[]`, "file_name_taken"},
		"no compose file":       {`[{"path":"app.env","content":"x","secret":false}]`, `[]`, "compose_missing"},
		"a variable with a dot": {`[` + compose + `]`, `[{"name":"A.B","value":"x","secret":false}]`, "variable_name_invalid"},
		"a repeated variable":   {`[` + compose + `]`, `[{"name":"A","value":"1","secret":false},{"name":"A","value":"2","secret":false}]`, "variable_name_taken"},
	} {
		rec := operator.saveDraft(st.ID, `{"version":1,"files":`+tc.files+`,"vars":`+tc.vars+`}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.problem {
			t.Errorf("%s: %d %s, want 422 %s", name, rec.Code, rec.Body, tc.problem)
		}
	}
	rec := operator.do(http.MethodGet, "stacks/"+st.ID+"/draft", "")
	if d := answerOf[draftAnswer](t, rec); d.Version != 1 {
		t.Errorf("a refused save moved the draft to version %d", d.Version)
	}
}

func TestDeployRefusesAComposeFileItCannotLoadOrConvertAndStoresNothing(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "broken")

	for name, tc := range map[string]struct{ compose, problem string }{
		"invalid yaml":           {"services: [", "compose_invalid"},
		"a schema error":         {"services:\n  a:\n    image: busybox\n    ports: 5\n", "compose_invalid"},
		"a service with a build": {"services:\n  a:\n    build: .\n", "compose_unsupported"},
		"a legacy memory limit":  {"services:\n  a:\n    image: busybox:1\n    mem_limit: 64m\n", "compose_unsupported"},
	} {
		rec := operator.saveDraft(st.ID, saveBody(draftVersion(t, operator, st.ID), tc.compose, `[]`))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: save %d %s", name, rec.Code, rec.Body)
		}
		rec = operator.deploy(st.ID, `{"draftVersion":`+itoaT(draftVersion(t, operator, st.ID))+`}`)
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != tc.problem {
			t.Errorf("%s: %d %s, want 422 %s", name, rec.Code, rec.Body, tc.problem)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	var revisions int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stack_snapshots WHERE kind = 'revision'`).Scan(&revisions); err != nil || revisions != 0 {
		t.Errorf("revisions = %d (%v), a refused deploy must leave none", revisions, err)
	}
	var deploys int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM stack_deploys`).Scan(&deploys); err != nil || deploys != 0 {
		t.Errorf("deploy rows = %d (%v)", deploys, err)
	}
}

func draftVersion(t *testing.T, c *roleCaller, stackID string) int64 {
	t.Helper()
	return answerOf[draftAnswer](t, c.do(http.MethodGet, "stacks/"+stackID+"/draft", "")).Version
}

func TestADeployFailureIsRecordedAndShownAsDeployFailed(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, func(c *dockertest.FakeConfig) { c.ServiceCreateError = "secret not found: hidden-name" })
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "failing")

	rec := operator.deploy(st.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	job := f.awaitDeploy(t, started.JobID)
	if job.State != "partial" {
		t.Fatalf("job = %+v, want partial (networks done, services failed)", job)
	}

	rows := f.stackAudits(t)
	dep := rows[len(rows)-1]
	if dep.Action != audit.ActionStackDeployed || dep.Result != string(audit.ResultFailure) || dep.Reason != "service_rejected" ||
		dep.Details["result"] != "failed" || dep.Details["code"] != "service_rejected" {
		t.Errorf("deploy audit row = %+v", dep)
	}
	var state, failedService, outcome string
	if err := f.st.Read.QueryRow(`SELECT state, failed_service FROM stack_deploys`).Scan(&state, &failedService); err != nil || state != "failed" || failedService != "failing_web" {
		t.Errorf("deploy row = %q %q (%v)", state, failedService, err)
	}
	if err := f.st.Read.QueryRow(`SELECT outcome FROM stack_snapshots WHERE kind = 'revision'`).Scan(&outcome); err != nil || outcome != "failed" {
		t.Errorf("revision outcome = %q (%v)", outcome, err)
	}
	rec = operator.do(http.MethodGet, "stacks/"+st.ID, "")
	if got := answerOf[stackAnswer](t, rec); got.State != "deploy_failed" || got.LastDeployedAt != "" {
		t.Errorf("stack = %+v, want deploy_failed with no successful deploy", got)
	}
	if strings.Contains(rec.Body.String(), "hidden-name") {
		t.Errorf("the stack answer holds the manager's own words: %s", rec.Body)
	}
}

// standaloneEnv is an environment that is the server's own standalone host, with the Compose SDK's up replaced
// by a stand-in that records the project it is given and plays the outcome err.
func standaloneEnv(t *testing.T, err error) (*envFix, envBody, *standaloneUp) {
	t.Helper()
	up := &standaloneUp{err: err}
	cfg := swarmManagerEngine()
	cfg.SwarmState, cfg.SwarmControl, cfg.NodeRows = "", false, nil
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.stackUp = up.run
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e, up
}

type standaloneUp struct {
	mu      sync.Mutex
	err     error
	project *types.Project
	calls   int
}

func (u *standaloneUp) run(_ context.Context, _ *dockerx.Client, p *types.Project, _ dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.project, u.calls = p, u.calls+1
	on(dockerx.ComposeEvent{Kind: dockerx.ComposeKindContainer, Name: "solo-web-1", Container: "solo-web-1", Service: "web", Status: "Running", Done: true})
	return u.err
}

func TestADeployToAStandaloneHostRunsTheComposeEngineAndIsAudited(t *testing.T) {
	t.Parallel()
	f, e, up := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	if st.Target != "standalone" {
		t.Fatalf("target = %q, want standalone", st.Target)
	}
	rec := operator.deploy(st.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s, want 202", rec.Code, rec.Body)
	}
	body := answerOf[struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}](t, rec)
	job := f.awaitDeploy(t, body.JobID)
	if job.State != "done" {
		t.Fatalf("job = %+v, want done", job)
	}
	if up.calls != 1 || up.project == nil || !strings.Contains(up.project.Services["web"].Image, "@sha256:") {
		t.Errorf("the Compose engine ran %d times with %+v, want once with a pinned image", up.calls, up.project)
	}
	// The only write the engine saw is Bayplan's own pull of the pinned image (plan 07-21); the Compose
	// engine's own writes are the stand-in's and none reached it.
	if w := f.fake.Writes(); len(w) != 1 || !strings.HasPrefix(w[0], "POST /images/create?") {
		t.Errorf("engine writes = %v, want the pull of the image alone", w)
	}
	rec = operator.do(http.MethodGet, "stacks/"+st.ID, "")
	// The Compose stand-in made no container, so nothing runs: the stack was deployed and is not running.
	if got := answerOf[stackAnswer](t, rec); got.State != "not_running" || got.LastDeployedBy == "" {
		t.Errorf("stack = %+v, want deployed and not running", got)
	}
	var deployed []auditRowFull
	for _, a := range f.stackAudits(t) {
		if a.Action == "stack.deployed" {
			deployed = append(deployed, a)
		}
	}
	if len(deployed) != 1 || deployed[0].Result != "success" || deployed[0].Details["mode"] != "deploy" {
		t.Errorf("audit = %+v, want one successful stack.deployed", deployed)
	}
}

func TestAFailedStandaloneDeployIsRecordedAsFailed(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, errors.New("Error response from daemon: port is already allocated"))
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	rec := operator.deploy(st.ID, `{"draftVersion":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body)
	}
	body := answerOf[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	job := f.awaitDeploy(t, body.JobID)
	if job.State == "done" {
		t.Fatalf("job = %+v, want it failed", job)
	}
	rec = operator.do(http.MethodGet, "stacks/"+st.ID, "")
	if got := answerOf[stackAnswer](t, rec); got.State != "deploy_failed" {
		t.Errorf("stack = %+v, want deploy_failed", got)
	}
}

func TestARollBackOfAStandaloneStackIsRefusedUntilItsEngineExists(t *testing.T) {
	t.Parallel()
	f, e, _ := standaloneEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "solo")
	rec := operator.do(http.MethodPost, "stacks/"+st.ID+"/rollback", "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "target_engine_missing" {
		t.Errorf("rollback: %d %s, want 409 target_engine_missing", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestTheStackListShowsADraftThatWasSavedAfterTheLastDeploy(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "drafty")
	hasDraft := func() bool {
		var list struct {
			Items []stackAnswer `json:"items"`
		}
		if err := json.Unmarshal(operator.do(http.MethodGet, "stacks", "").Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
			t.Fatalf("list: %v", err)
		}
		return list.Items[0].HasDraft
	}
	if hasDraft() {
		t.Error("a stack nobody has saved lists a draft")
	}
	if rec := operator.saveDraft(st.ID, saveBody(1, "services:\n  a:\n    image: busybox:1\n", `[]`)); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if !hasDraft() {
		t.Error("a saved, undeployed draft does not list as a draft")
	}
	rec := operator.deploy(st.ID, `{"draftVersion":2}`)
	var started struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	f.awaitDeploy(t, started.JobID)
	if hasDraft() {
		t.Error("a deployed draft still lists as a draft")
	}
}

// The state of a stack (plan 07-23, STK-10, UI-SPEC Defaults 11): derived from Bayplan's records of the stack's
// operations and from what the engine runs, the same way on the list and on the page.

func readStackAnswer(t *testing.T, c *roleCaller, id string) stackAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get stack: %d %s", rec.Code, rec.Body)
	}
	return answerOf[stackAnswer](t, rec)
}

func listedStack(t *testing.T, c *roleCaller, id string) stackAnswer {
	t.Helper()
	rec := c.do(http.MethodGet, "stacks", "")
	var list struct {
		Items []stackAnswer `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list = %s (%v)", rec.Body, err)
	}
	for _, it := range list.Items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("stack %s is not in %s", id, rec.Body)
	return stackAnswer{}
}

func wantCounts(t *testing.T, got stackAnswer, running, total int) {
	t.Helper()
	if got.ServicesRunning == nil || got.ServicesTotal == nil || *got.ServicesRunning != running || *got.ServicesTotal != total {
		t.Errorf("services = %v of %v, want %d of %d", deref(got.ServicesRunning), deref(got.ServicesTotal), running, total)
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestAStackReadsRunningWithItsServiceCountsOnTheListAndThePage(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	page, listed := readStackAnswer(t, operator, st.ID), listedStack(t, operator, st.ID)

	for name, got := range map[string]stackAnswer{"page": page, "list": listed} {
		if got.State != "running" {
			t.Errorf("%s state = %q, want running", name, got.State)
		}
		wantCounts(t, got, 1, 1)
	}
}

func TestAStackNeverDeployedHasNoServiceCounts(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	st := newStack(t, admin, "fresh")

	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, admin, st.ID), "list": listedStack(t, admin, st.ID)} {
		if got.State != "not_deployed" || got.ServicesRunning != nil || got.ServicesTotal != nil {
			t.Errorf("%s = %+v, want not_deployed with no counts", name, got)
		}
	}
}

// ownedService is a service the stack st owns on the fake swarm, with the task counts given.
func ownedService(st stackAnswer, short string, running, desired uint64) dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-" + short, Name: st.Name + "_" + short, Mode: "replicated", Running: running, Desired: desired,
		Image:  "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64),
		Labels: map[string]string{"com.docker.stack.namespace": st.Name, "bayplan.stack.id": st.ID, "bayplan.stack.image": "nginx:1.27-alpine"},
	}
}

// reconfigureSwarm sets the services the fake swarm holds and reads the cluster again.
func reconfigureSwarm(t *testing.T, f *envFix, e envBody, services ...dockertest.FakeService) {
	t.Helper()
	f.fake.Reconfigure(swarmManagerEngine(services...))
	if err := f.clusters.Refresh(testsupport.Context(t), e.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestADeployedStackReadsDegradedWhenSomeServicesRunAndNotRunningWhenNoneDo(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}

	reconfigureSwarm(t, f, e, ownedService(st, "a", 2, 2), ownedService(st, "b", 1, 3))
	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, operator, st.ID), "list": listedStack(t, operator, st.ID)} {
		if got.State != "degraded" {
			t.Errorf("%s state = %q, want degraded", name, got.State)
		}
		wantCounts(t, got, 1, 2)
	}

	reconfigureSwarm(t, f, e, ownedService(st, "a", 0, 2), ownedService(st, "b", 0, 3))
	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, operator, st.ID), "list": listedStack(t, operator, st.ID)} {
		if got.State != "not_running" {
			t.Errorf("%s state = %q, want not_running", name, got.State)
		}
		wantCounts(t, got, 0, 2)
	}
}

func TestAServiceOfTheSameNamespaceThatAnotherStackOwnsIsNotCountedForThisOne(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	foreign := ownedService(st, "foreign", 0, 1)
	foreign.Labels = map[string]string{"com.docker.stack.namespace": st.Name}

	reconfigureSwarm(t, f, e, ownedService(st, "a", 1, 1), foreign)

	got := readStackAnswer(t, operator, st.ID)
	if got.State != "running" {
		t.Errorf("state = %q, want running", got.State)
	}
	wantCounts(t, got, 1, 1)
}

func TestAStackWithADeployUnderWayReadsDeployingWhateverRuns(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	jobID := startStuckDeploy(t, f, operator, st)

	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, operator, st.ID), "list": listedStack(t, operator, st.ID)} {
		if got.State != "deploying" {
			t.Errorf("%s state = %q, want deploying", name, got.State)
		}
	}

	if rec := operator.do(http.MethodPost, cancelPath(st.ID, jobID), ""); rec.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	f.awaitDeploy(t, jobID)
	if got := readStackAnswer(t, operator, st.ID); got.State != "deploy_failed" {
		t.Errorf("state after a cancel = %q, want deploy_failed", got.State)
	}
}

func TestAStoppedStackReadsStoppedAndAFailedDeployDoesNotHideIt(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	if _, err := f.st.Write.Exec(`UPDATE stacks SET stopped = 1 WHERE id = ?`, st.ID); err != nil {
		t.Fatal(err)
	}
	reconfigureSwarm(t, f, e, ownedService(st, "web", 0, 0))

	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, operator, st.ID), "list": listedStack(t, operator, st.ID)} {
		if got.State != "stopped" {
			t.Errorf("%s state = %q, want stopped", name, got.State)
		}
	}
	if got := readStackAnswer(t, operator, st.ID); !got.Stopped {
		t.Errorf("stopped = false on the page of a stopped stack")
	}
}

func TestADeployedStackWhoseEngineCannotBeReadReadsUnknownNotRunning(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	// A cluster that was never read and cannot be: forget what the deploy's own read left.
	f.clusters.Forget(e.ID)
	f.fake.LoseQuorum()

	for name, got := range map[string]stackAnswer{"page": readStackAnswer(t, operator, st.ID), "list": listedStack(t, operator, st.ID)} {
		if got.State != "unknown" || got.ServicesRunning != nil || got.ServicesTotal != nil {
			t.Errorf("%s = %+v, want unknown with no counts: the stack is not claimed to run or not to run", name, got)
		}
	}
}

func TestADraftSavedAfterTheLastDeployIsNamedOnThePage(t *testing.T) {
	t.Parallel()
	f, e := stackEnv(t, nil)
	admin := f.caller(t, e, "envadmin-1", "admin")
	operator := f.caller(t, e, "operator-1", "operator")
	st := newStack(t, admin, "tracer")
	if got := readStackAnswer(t, operator, st.ID); got.DraftSavedAt != "" || got.DraftSavedBy != "" {
		t.Fatalf("a stack with no draft pending names a save: %+v", got)
	}
	if job := f.deployAndWait(t, operator, st.ID, `{"draftVersion":1}`); job.State != "done" {
		t.Fatalf("deploy: %+v", job)
	}
	if got := readStackAnswer(t, operator, st.ID); got.HasDraft || got.DraftSavedAt != "" {
		t.Fatalf("right after a deploy there is no draft: %+v", got)
	}

	saveCompose(t, operator, st.ID, stacks.StarterCompose+"# tuned\n", `[]`)

	got := readStackAnswer(t, operator, st.ID)
	if !got.HasDraft || got.DraftSavedBy != "operator-1" || got.DraftSavedAt == "" {
		t.Errorf("stack = %+v, want a draft saved by operator-1", got)
	}
}
