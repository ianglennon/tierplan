// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Review and apply a full service spec (plan 06-18, RES-03, D-03, D-08, D-09, D-13). Docker is a
// fake daemon behind the real monitor, aggregator and cluster reader; the router, policy, CSRF
// guard, registry resolver and audit writer are the real ones.

const (
	editDB    = "postgres://user:hunter2@db/app" //nolint:gosec // G101: a fixture secret the tests look for in every answer
	editToken = "s3cr3t-token"
	editImage = "localhost:5000/app/api:1"
)

// editableSpec is a standalone service with a secret in each place a spec can hold one.
func editableSpec() swarm.ServiceSpec {
	replicas := uint64(2)
	return swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "api", Labels: map[string]string{"team": "core"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:       editImage,
				Env:         []string{"DB=" + editDB, "LEVEL=info"},
				Command:     []string{"api", "--admin-token=" + editToken},
				Healthcheck: &container.HealthConfig{Test: []string{"CMD-SHELL", "probe --key=hc-secret-key"}},
			},
			LogDriver: &swarm.Driver{Name: "gelf", Options: map[string]string{"gelf-address": "udp://logtoken-123@h:12201"}},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
	}
}

func editableService(tune func(*dockertest.FakeService)) dockertest.FakeService {
	spec := editableSpec()
	fs := dockertest.FakeService{ID: "svc-api", Name: "api", Mode: "replicated", Running: 2, Desired: 2, Version: 5, Spec: &spec}
	if tune != nil {
		tune(&fs)
	}
	return fs
}

var editSecrets = []string{"hunter2", editToken, "hc-secret-key", "logtoken-123"}

// roleCaller signs a user with that role on the environment in.
type roleCaller struct {
	f            *envFix
	t            *testing.T
	e            envBody
	cookie, csrf string
}

func (f *envFix) caller(t *testing.T, e envBody, user, role string) *roleCaller {
	t.Helper()
	f.addUser(t, user, false)
	f.addEnvRole(t, user, e.ID, role)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State != "" })
	cookie, csrf := f.signIn(t, user, false)
	return &roleCaller{f: f, t: t, e: e, cookie: cookie, csrf: csrf}
}

// revealCaller signs an Operator who also holds secrets.reveal: the permission a caller needs to add
// a secret the service does not use (D-13 as amended, plan 06-48).
func (f *envFix) revealCaller(t *testing.T, e envBody, user string) *roleCaller {
	t.Helper()
	id := f.addUser(t, user, false)
	if rec := f.putGrant(t, f.cookie, f.csrf, "users", id, `{"scope":"env","envId":"`+e.ID+`","role":"operator","extras":["secrets.reveal"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State != "" })
	cookie, csrf := f.signIn(t, user, false)
	return &roleCaller{f: f, t: t, e: e, cookie: cookie, csrf: csrf}
}

func (c *roleCaller) do(method, path, body string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.f.call(c.t, authReq{method: method, path: "/api/env/" + c.e.ID + "/" + path, body: body, cookie: c.cookie, csrf: c.csrf})
}

func (c *roleCaller) review(service, body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "services/"+service+"/review", body)
}

func (c *roleCaller) apply(service, body string) *httptest.ResponseRecorder {
	return c.do(http.MethodPost, "services/"+service+"/apply", body)
}

// maskedSpec is what the Spec tab of this caller holds: GET .../spec, edited by mut.
func (c *roleCaller) maskedSpec(service string, mut func(spec map[string]any)) (version uint64, body string) {
	c.t.Helper()
	rec := c.do(http.MethodGet, "services/"+service+"/spec", "")
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET spec: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Version uint64         `json:"version"`
		Spec    map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		c.t.Fatal(err)
	}
	if mut != nil {
		mut(got.Spec)
	}
	b, err := json.Marshal(map[string]any{"version": got.Version, "spec": got.Spec})
	if err != nil {
		c.t.Fatal(err)
	}
	return got.Version, string(b)
}

func containerSpecOf(spec map[string]any) map[string]any {
	tt, _ := spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	return cs
}

func noSecrets(t *testing.T, what string, b []byte) {
	t.Helper()
	for _, s := range editSecrets {
		if strings.Contains(string(b), s) {
			t.Errorf("%s holds %q: %s", what, s, b)
		}
	}
}

type reviewAnswer struct {
	Spec    map[string]any `json:"spec"`
	Version uint64         `json:"version"`
	Changed bool           `json:"changed"`
	Image   struct {
		Ref     string `json:"ref"`
		Digest  string `json:"digest"`
		Changed bool   `json:"changed"`
	} `json:"image"`
	Findings []struct {
		Path  string `json:"path"`
		Value string `json:"value"`
		Rule  string `json:"rule"`
	} `json:"findings"`
	Allowed    bool   `json:"allowed"`
	Refusal    string `json:"refusal"`
	Missing    struct{ Networks []string }
	HostPolicy struct {
		ManagerAllowsPrivileged bool `json:"managerAllowsPrivileged"`
	} `json:"hostPolicy"`
}

func TestServiceEditReviewAndApplyNeedServicesControlAViewerIsForbidden(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	viewer := f.caller(t, e, "viewer-1", "viewer")
	body := `{"version":5,"spec":{"Name":"api"}}`
	for name, rec := range map[string]*httptest.ResponseRecorder{"review": viewer.review("svc-api", body), "apply": viewer.apply("svc-api", body)} {
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s by a viewer: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestServiceEditReviewMasksEverySecretAndChangesNothing(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.caller(t, e, "operator-1", "operator")
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
		cs := containerSpecOf(spec)
		cs["Image"] = "localhost:5000/app/api:2"
		cs["Env"] = []any{"DB=<hidden>", "LEVEL=debug"}
	})
	rec := op.review("svc-api", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	noSecrets(t, "the review answer", rec.Body.Bytes())
	var got reviewAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	digest := dockertest.FakeDigestOf("localhost:5000/app/api:2")
	if !got.Changed || !got.Allowed || got.Version != 5 || got.Image.Ref != "localhost:5000/app/api:2@"+digest || !got.Image.Changed || !got.HostPolicy.ManagerAllowsPrivileged {
		t.Fatalf("answer = %+v", got)
	}
	if env := containerSpecOf(got.Spec)["Env"]; !slices.Equal(toStrings(env), []string{"DB=<hidden>", "LEVEL=<hidden>"}) {
		t.Errorf("env = %v, want the values hidden", env)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a review changes nothing", w)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, a review is not audited", rows)
	}
}

func toStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, x := range list {
		str, _ := x.(string)
		out = append(out, str)
	}
	return out
}

func TestServiceEditApplyRoundTripsASecretFixtureForAnOperatorWhoCannotRevealIt(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.caller(t, e, "operator-1", "operator")
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
		cs := containerSpecOf(spec)
		cs["Image"] = "localhost:5000/app/api:2"
		cs["Env"] = []any{"DB=<hidden>", "LEVEL=debug"}
	})
	rec := op.apply("svc-api", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	noSecrets(t, "the apply answer", rec.Body.Bytes())

	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].ServiceID != "svc-api" || ups[0].Version != "5" {
		t.Fatalf("updates = %+v, want one at version 5", ups)
	}
	cs := ups[0].Spec.TaskTemplate.ContainerSpec
	digest := dockertest.FakeDigestOf("localhost:5000/app/api:2")
	if cs.Image != "localhost:5000/app/api:2@"+digest || !slices.Equal(cs.Env, []string{"DB=" + editDB, "LEVEL=debug"}) ||
		!slices.Equal(cs.Command, []string{"api", "--admin-token=" + editToken}) || cs.Healthcheck.Test[1] != "probe --key=hc-secret-key" ||
		ups[0].Spec.TaskTemplate.LogDriver.Options["gelf-address"] != "udp://logtoken-123@h:12201" {
		t.Errorf("the daemon received %+v, want the real values merged back and the image pinned", cs)
	}

	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionServiceUpdated || rows[0].Result != string(audit.ResultSuccess) || rows[0].TargetID != "svc-api" || rows[0].EnvID != e.ID {
		t.Fatalf("audit rows = %+v, want one service.updated success", rows)
	}
	for k, want := range map[string]string{"name": "api", "version": "5", "image": "localhost:5000/app/api:2@" + digest, "digest": digest, "changed": "true", "findings": "0", "code": "done"} {
		if rows[0].Details[k] != want {
			t.Errorf("audit detail %s = %q, want %q (%+v)", k, rows[0].Details[k], want, rows[0].Details)
		}
	}
	b, _ := json.Marshal(rows)
	noSecrets(t, "the audit rows", b)
}

func TestServiceEditAnOperatorIsRefusedHostAccessAndAnEnvAdminMayHaveIt(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.caller(t, e, "operator-1", "operator")
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
		containerSpecOf(spec)["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/sock"}}
	})

	rev := op.review("svc-api", body)
	var ra reviewAnswer
	if rev.Code != http.StatusOK || json.Unmarshal(rev.Body.Bytes(), &ra) != nil || ra.Allowed || len(ra.Findings) != 1 ||
		ra.Findings[0].Path != "TaskTemplate.ContainerSpec.Mounts[0].Source" || ra.Findings[0].Rule != "bind_path" ||
		ra.Refusal != "TaskTemplate.ContainerSpec.Mounts[0].Source: /var/run/docker.sock" {
		t.Fatalf("review = %d %s", rev.Code, rev.Body)
	}

	rec := op.apply("svc-api", body)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_service" {
		t.Fatalf("apply by an operator: %d %s, want 403 privileged_service", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if prob.at("TaskTemplate.ContainerSpec.Mounts[0].Source") != "/var/run/docker.sock" || !strings.Contains(prob.Detail, "TaskTemplate.ContainerSpec.Mounts[0].Source: /var/run/docker.sock") {
		t.Errorf("problem = %+v, want each finding listed", prob)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, a refused spec is never sent", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "privileged_service" || rows[0].Details["findings"] != "1" || rows[0].Details["code"] != "privileged_service" {
		t.Fatalf("audit rows = %+v, want one privileged_service failure with one finding", rows)
	}

	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body = admin.maskedSpec("svc-api", func(spec map[string]any) {
		containerSpecOf(spec)["Mounts"] = []any{map[string]any{"Type": "bind", "Source": "/var/run/docker.sock", "Target": "/sock"}}
	})
	if rec := admin.apply("svc-api", body); rec.Code != http.StatusAccepted {
		t.Fatalf("apply by an env admin: %d %s", rec.Code, rec.Body)
	}
	if ups := f.fake.ServiceUpdates(); len(ups) != 1 {
		t.Errorf("updates = %d, want the admin's one", len(ups))
	}
}

func TestServiceEditEachRefusalIsItsOwnProblemAndNoneSendsAnythingOrLeaksAValue(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil), webService())
	op := f.caller(t, e, "operator-1", "operator")
	type tc struct {
		name    string
		service string
		mut     func(map[string]any)
		version func(uint64) uint64
		raw     string
		status  int
		typ     string
	}
	cases := []tc{
		{name: "a stale version", mut: func(m map[string]any) { m["Labels"] = map[string]any{"a": "b"} }, version: func(v uint64) uint64 { return v - 1 }, status: 409, typ: "service_changed"},
		{name: "an untouched spec", mut: nil, status: 422, typ: "nothing_to_apply"},
		{name: "a rename", mut: func(m map[string]any) { m["Name"] = "other" }, status: 422, typ: "name_immutable"},
		{name: "a mode change", mut: func(m map[string]any) { m["Mode"] = map[string]any{"Global": map[string]any{}} }, status: 422, typ: "mode_immutable"},
		{name: "a stack label", mut: func(m map[string]any) { m["Labels"] = map[string]any{"com.docker.stack.namespace": "x"} }, status: 422, typ: "stack_label_reserved"},
		{name: "a hidden value with nothing behind it", mut: func(m map[string]any) {
			containerSpecOf(m)["Env"] = []any{"GHOST=<hidden>"}
		}, status: 422, typ: "placeholder_unmatched"},
		{name: "a missing network", mut: func(m map[string]any) {
			tt, _ := m["TaskTemplate"].(map[string]any)
			tt["Networks"] = []any{map[string]any{"Target": "front"}}
		}, status: 422, typ: "objects_missing"},
		{name: "an unknown field", raw: `{"version":5,"spec":{"Name":"api","Bogus":1}}`, status: 422, typ: "spec_invalid"},
		{name: "a stack-owned service", service: "svc-web", raw: `{"version":4,"spec":{"Name":"web"}}`, status: 409, typ: "stack_owned"},
		{name: "a service that does not exist", service: "ghost", raw: `{"version":1,"spec":{"Name":"ghost"}}`, status: 404, typ: "service_not_found"},
	}
	for _, c := range cases {
		service := c.service
		if service == "" {
			service = "svc-api"
		}
		body := c.raw
		if body == "" {
			version, b := op.maskedSpec("svc-api", c.mut)
			if c.version != nil {
				var m map[string]any
				_ = json.Unmarshal([]byte(b), &m)
				m["version"] = c.version(version)
				bb, _ := json.Marshal(m)
				b = string(bb)
			}
			body = b
		}
		for route, do := range map[string]func(string, string) *httptest.ResponseRecorder{"review": op.review, "apply": op.apply} {
			if route == "review" && (c.typ == "nothing_to_apply" || c.typ == "objects_missing") {
				continue // a review reports these in its answer
			}
			rec := do(service, body)
			if rec.Code != c.status || problemType(t, rec) != c.typ {
				t.Errorf("%s of %s: %d %s (%s), want %d %s", route, c.name, rec.Code, rec.Body, problemType(t, rec), c.status, c.typ)
			}
			noSecrets(t, route+" of "+c.name, rec.Body.Bytes())
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, none of these is ever sent", w)
	}
	for _, r := range f.serviceAudits(t) {
		b, _ := json.Marshal(r)
		noSecrets(t, "an audit row", b)
		if r.Result != string(audit.ResultFailure) {
			t.Errorf("audit row %+v, want only failures", r)
		}
	}
}

func TestServiceEditDockersRefusalIsSpecRejectedWithItsWordsAndNoValue(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(func(fs *dockertest.FakeService) {
		fs.UpdateError = "secret not found: db_password (env " + editDB + ", --admin-token=" + editToken + ")"
	}))
	op := f.caller(t, e, "operator-1", "operator")
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) { containerSpecOf(spec)["Env"] = []any{"DB=<hidden>", "LEVEL=debug"} })
	rec := op.apply("svc-api", body)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "spec_rejected" {
		t.Fatalf("apply: %d %s, want 422 spec_rejected", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Docker rejected these settings: ") || !strings.Contains(rec.Body.String(), "secret not found: db_password") {
		t.Errorf("body = %s, want Docker's words", rec.Body)
	}
	noSecrets(t, "the refusal", rec.Body.Bytes())
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Reason != "spec_rejected" {
		t.Fatalf("audit rows = %+v", rows)
	}
	b, _ := json.Marshal(rows)
	noSecrets(t, "the audit rows", b)
}

func TestServiceEditTheSameVersionAppliedTwiceMakesTheSecondServiceChanged(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.caller(t, e, "operator-1", "operator")
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) { containerSpecOf(spec)["Env"] = []any{"DB=<hidden>", "LEVEL=debug"} })
	if rec := op.apply("svc-api", body); rec.Code != http.StatusAccepted {
		t.Fatalf("first apply: %d %s", rec.Code, rec.Body)
	}
	rec := op.apply("svc-api", body)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "service_changed" {
		t.Fatalf("second apply: %d %s, want 409 service_changed", rec.Code, rec.Body)
	}
	if got := len(f.fake.ServiceUpdates()); got != 1 {
		t.Errorf("updates = %d, want one", got)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 2 || rows[1].Reason != "service_changed" {
		t.Errorf("audit rows = %+v, want the success and the service_changed failure", rows)
	}
}

func TestServiceEditOfAStandaloneHostIsNotASwarm(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	body := `{"version":1,"spec":{"Name":"api"}}`
	for _, route := range []string{"review", "apply"} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/api/"+route, body)
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("%s = %d %s (%s), want 409 not_a_swarm", route, rec.code, rec.body, rec.ptyp)
		}
	}
}

// Staged objects (plan 06-26, RES-03, D-06, D-07, D-13).

const stagedValue = "staged-value-9f2c" //nolint:gosec // G101: a fixture value the tests look for in every answer and audit row

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// refsStaged makes the masked spec name a network, a secret and a config by name only.
func refsStaged(spec map[string]any) {
	tt, _ := spec["TaskTemplate"].(map[string]any)
	tt["Networks"] = []any{map[string]any{"Target": "front", "Aliases": []any{"web"}}}
	cs := containerSpecOf(spec)
	cs["Secrets"] = []any{map[string]any{"SecretName": "db-pass", "File": map[string]any{"Name": "db-pass", "UID": "0", "GID": "0", "Mode": 292}}}
	cs["Configs"] = []any{map[string]any{"ConfigName": "app.conf", "File": map[string]any{"Name": "/app.conf", "UID": "0", "GID": "0", "Mode": 292}}}
}

func stagedObjects() []map[string]any {
	return []map[string]any{
		{"kind": "config", "name": "app.conf", "data": b64("level=" + stagedValue), "templating": "golang"},
		{"kind": "secret", "name": "db-pass", "data": b64(stagedValue), "labels": map[string]string{"team": "core"}},
		{"kind": "network", "name": "front", "subnet": "10.77.0.0/24"},
	}
}

// withObjects adds objects to an apply body.
func withObjects(t *testing.T, body string, objects any) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["objects"] = objects
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f *envFix) auditLike(t *testing.T, like string) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE ? ORDER BY seq`, like)
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
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type applyAnswer struct {
	Image string `json:"image"`
	Steps []struct {
		Step  string `json:"step"`
		Kind  string `json:"kind"`
		Name  string `json:"name"`
		State string `json:"state"`
		Code  string `json:"code"`
	} `json:"steps"`
	Cleanup struct {
		Removed    []struct{ Kind, Name, ID string } `json:"removed"`
		NotRemoved []struct{ Kind, Name, ID string } `json:"notRemoved"`
	} `json:"cleanup"`
}

func noStagedValue(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), stagedValue) || strings.Contains(string(b), b64(stagedValue)) {
		t.Errorf("%s holds the staged value: %s", what, b)
	}
}

func writeVerbs(writes []string) []string {
	out := make([]string, 0, len(writes))
	for _, w := range writes {
		method, rest, _ := strings.Cut(w, " ")
		out = append(out, method+" /"+strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 2)[0])
	}
	return out
}

func TestApplyWithStagedObjects(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body := admin.maskedSpec("svc-api", refsStaged)
	rec := admin.apply("svc-api", withObjects(t, body, stagedObjects()))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	noStagedValue(t, "the answer", rec.Body.String())
	var got applyAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, s := range got.Steps {
		steps = append(steps, fmt.Sprintf("%s/%s/%s/%s", s.Step, s.Kind, s.Name, s.State))
	}
	want := []string{"image//" + "/skipped", "object/network/front/done", "object/secret/db-pass/done", "object/config/app.conf/done", "service//api/done"}
	if !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if !strings.Contains(rec.Body.String(), `"removed":[]`) || !strings.Contains(rec.Body.String(), `"notRemoved":[]`) {
		t.Errorf("body = %s, want empty cleanup lists, not null", rec.Body)
	}
	if got, want := writeVerbs(f.fake.Writes()), []string{"POST /networks", "POST /secrets", "POST /configs", "POST /services"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	ups := f.fake.ServiceUpdates()
	cs := ups[0].Spec.TaskTemplate.ContainerSpec
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID == "" || len(cs.Configs) != 1 || cs.Configs[0].ConfigID == "" {
		t.Errorf("references = %+v %+v, want the new IDs filled in", cs.Secrets, cs.Configs)
	}
	if n := f.fake.NetworkCreates(); len(n) != 1 || !n[0].Attachable || n[0].Driver != "overlay" {
		t.Errorf("network creates = %+v", n)
	}
	if c := f.fake.ConfigCreates(); len(c) != 1 || c[0].Templating != "golang" {
		t.Errorf("config creates = %+v", c)
	}

	for action, targetType := range map[string]string{"network.created": "network", "secret.created": "secret", "config.created": "config"} {
		rows := f.auditLike(t, action)
		if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) || rows[0].TargetType != targetType || rows[0].EnvID != e.ID ||
			rows[0].Details["source"] != "service_apply" || rows[0].Details["code"] != "created" {
			t.Errorf("%s rows = %+v, want one success with source service_apply", action, rows)
		}
		noStagedValue(t, action+" rows", rows)
	}
	if rows := f.auditLike(t, "secret.created"); rows[0].Details["size"] != fmt.Sprint(len(stagedValue)) {
		t.Errorf("secret row = %+v, want the size and no value", rows[0].Details)
	}
	if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) {
		t.Errorf("service rows = %+v", rows)
	}
}

func TestApplyCleanupIsReported(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(func(fs *dockertest.FakeService) { fs.UpdateError = "the manager refuses this spec" }))
	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body := admin.maskedSpec("svc-api", refsStaged)
	rec := admin.apply("svc-api", withObjects(t, body, stagedObjects()))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "spec_rejected" {
		t.Fatalf("apply: %d %s, want 422 spec_rejected", rec.Code, rec.Body)
	}
	noStagedValue(t, "the problem", rec.Body.String())
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prob.Detail, "Couldn't update api. Docker rejected these settings: ") || !strings.Contains(prob.Detail, "the manager refuses this spec") ||
		!strings.HasSuffix(prob.Detail, " The 3 objects created for this change were removed.") {
		t.Errorf("detail = %q, want the reason followed by the 3 objects were removed", prob.Detail)
	}
	if reason := prob.at("reason"); !strings.HasPrefix(reason, "Docker rejected these settings: ") || strings.Contains(reason, "Couldn't update") {
		t.Errorf("reason = %q, want Docker's refusal on its own", reason)
	}
	var removed []string
	for _, er := range prob.Errors {
		if er.Location == "removed" {
			removed = append(removed, er.Message)
		}
	}
	if want := []string{"config:app.conf", "secret:db-pass", "network:front"}; !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	if got, want := writeVerbs(f.fake.Writes()), []string{"POST /networks", "POST /secrets", "POST /configs", "POST /services", "DELETE /configs", "DELETE /secrets", "DELETE /networks"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
	for _, action := range []string{"network.removed", "secret.removed", "config.removed"} {
		rows := f.auditLike(t, action)
		if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) || rows[0].Details["source"] != "service_apply_cleanup" {
			t.Errorf("%s rows = %+v, want one success with source service_apply_cleanup", action, rows)
		}
	}
	if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Reason != "spec_rejected" {
		t.Errorf("service rows = %+v, want the spec_rejected failure", rows)
	}
}

func TestApplyCleanupThatCouldNotFinishNamesWhatIsLeft(t *testing.T) {
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) { c.RemoveErrors = map[string]string{"db-pass": "the manager is busy"} },
		editableService(func(fs *dockertest.FakeService) { fs.UpdateError = "the manager refuses this spec" }))
	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body := admin.maskedSpec("svc-api", refsStaged)
	rec := admin.apply("svc-api", withObjects(t, body, stagedObjects()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(prob.Detail, "Couldn't update api. Docker rejected these settings: ") ||
		!strings.HasSuffix(prob.Detail, " Bayplan couldn't remove secret db-pass. Remove them under Networks, Secrets or Configs.") {
		t.Errorf("detail = %q, want the reason followed by what could not be removed", prob.Detail)
	}
	if !strings.HasPrefix(prob.at("notRemoved"), "secret:db-pass") {
		t.Errorf("notRemoved = %q, want the secret", prob.at("notRemoved"))
	}
	rows := f.auditLike(t, "secret.removed")
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "remove_failed" || rows[0].Details["source"] != "service_apply_cleanup" {
		t.Errorf("secret.removed rows = %+v, want a remove_failed failure", rows)
	}
}

func TestApplyNameTakenMidwayRemovesWhatWasMade(t *testing.T) {
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-elsewhere", Name: "db-pass"}}
	}, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body := admin.maskedSpec("svc-api", refsStaged)
	rec := admin.apply("svc-api", withObjects(t, body, stagedObjects()))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "name_taken" {
		t.Fatalf("apply: %d %s, want 409 name_taken", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if want := "Couldn't update api. A secret named db-pass already exists. The 1 objects created for this change were removed."; prob.Detail != want {
		t.Errorf("detail = %q, want %q", prob.Detail, want)
	}
	if got, want := writeVerbs(f.fake.Writes()), []string{"POST /networks", "POST /secrets", "DELETE /networks"}; !slices.Equal(got, want) {
		t.Errorf("writes = %v, want %v", got, want)
	}
}

func TestApplyObjectPermissionForAnOperator(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.revealCaller(t, e, "operator-1")
	_, body := op.maskedSpec("svc-api", refsStaged)
	rec := op.apply("svc-api", withObjects(t, body, stagedObjects()))
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "object_permission" {
		t.Fatalf("apply: %d %s, want 403 object_permission", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if want := "Needs Admin to create networks, secrets and configs."; prob.Detail != want {
		t.Errorf("detail = %q, want %q", prob.Detail, want)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v: nothing is created before the permission is judged", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "object_permission" {
		t.Errorf("service rows = %+v, want one object_permission failure", rows)
	}
	noStagedValue(t, "the problem", rec.Body.String())
}

func TestApplyOnlyAsksForThePermissionsTheNewObjectsNeed(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.caller(t, e, "operator-1", "operator")
	// The secret and config exist already; only the network is new, and creating it needs Admin.
	_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
		tt, _ := spec["TaskTemplate"].(map[string]any)
		tt["Networks"] = []any{map[string]any{"Target": "front"}}
	})
	rec := op.apply("svc-api", withObjects(t, body, []map[string]any{{"kind": "network", "name": "front"}}))
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil || rec.Code != http.StatusForbidden || prob.Detail != "Needs Admin to create networks." {
		t.Fatalf("apply: %d %s, want 403 Needs Admin to create networks.", rec.Code, rec.Body)
	}
}

func TestApplyRefusesBadObjectsBeforeAnythingIsSent(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	_, body := admin.maskedSpec("svc-api", refsStaged)
	many := make([]map[string]any, 17)
	for i := range many {
		many[i] = map[string]any{"kind": "network", "name": fmt.Sprintf("net%d", i)}
	}
	cases := []struct {
		name    string
		objects any
		typ     string
	}{
		{"seventeen objects", many, "too_many_objects"},
		{"a bad name", []map[string]any{{"kind": "secret", "name": "-bad", "data": b64("v")}}, "name_invalid"},
		{"a secret without a value", []map[string]any{{"kind": "secret", "name": "db-pass"}}, "value_required"},
		{"a value that is not base64", []map[string]any{{"kind": "config", "name": "app.conf", "data": "***"}}, "value_invalid"},
		{"a secret that is too large", []map[string]any{{"kind": "secret", "name": "db-pass", "data": b64(strings.Repeat("x", 512000))}}, "value_too_large"},
		{"templating that is not known", []map[string]any{{"kind": "config", "name": "app.conf", "data": b64("v"), "templating": "jinja"}}, "templating_invalid"},
		{"a subnet that is not a subnet", []map[string]any{{"kind": "network", "name": "front", "subnet": "10.0.0.1"}}, "subnet_invalid"},
		{"the same object twice", []map[string]any{{"kind": "network", "name": "front"}, {"kind": "network", "name": "front"}}, "object_duplicate"},
		{"a label that is too long", []map[string]any{{"kind": "secret", "name": "db-pass", "data": b64("v"), "labels": map[string]string{strings.Repeat("k", 300): "v"}}}, "labels_invalid"},
	}
	for _, c := range cases {
		rec := admin.apply("svc-api", withObjects(t, body, c.objects))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != c.typ {
			t.Errorf("%s: %d %s, want 422 %s", c.name, rec.Code, rec.Body, c.typ)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a bad request sends nothing", w)
	}
}

func TestReviewListsMissingSecretsAndConfigs(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	op := f.revealCaller(t, e, "operator-1")
	_, body := op.maskedSpec("svc-api", refsStaged)
	rec := op.review("svc-api", body)
	var got struct {
		Missing struct{ Networks, Secrets, Configs []string } `json:"missing"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		!slices.Equal(got.Missing.Networks, []string{"front"}) || !slices.Equal(got.Missing.Secrets, []string{"db-pass"}) || !slices.Equal(got.Missing.Configs, []string{"app.conf"}) {
		t.Fatalf("review = %d %s, want all three kinds missing", rec.Code, rec.Body)
	}
}

func TestApplyBodyOverEightMiBIsRefusedBeforeAnythingIsSent(t *testing.T) {
	f, e := actionsEnv(t, nil, editableService(nil))
	admin := f.caller(t, e, "envadmin-1", "admin")
	body := `{"version":5,"spec":{"Name":"api"},"objects":[{"kind":"config","name":"big","data":"` + strings.Repeat("A", 9<<20) + `"}]}`
	rec := admin.apply("svc-api", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("apply: %d, want 413 for a body over 8 MiB", rec.Code)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestServiceWriteObjectsMissingNamesEveryKindOfMissingObject(t *testing.T) {
	type entry struct{ location, message string }
	var (
		secretOnly = func(spec map[string]any) {
			containerSpecOf(spec)["Secrets"] = []any{map[string]any{"SecretName": "db-pass", "File": map[string]any{"Name": "db-pass", "UID": "0", "GID": "0", "Mode": 292}}}
		}
		oneOfEach = []entry{{"networks", "front"}, {"secrets", "db-pass"}, {"configs", "app.conf"}}
	)
	check := func(t *testing.T, f *envFix, rec *httptest.ResponseRecorder, wantDetail string, want []entry) {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "objects_missing" {
			t.Fatalf("answer = %d %s (%s), want 422 objects_missing", rec.Code, rec.Body, problemType(t, rec))
		}
		var p actionProblem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Detail != wantDetail {
			t.Errorf("detail = %q, want %q", p.Detail, wantDetail)
		}
		var got []entry
		for _, e := range p.Errors {
			got = append(got, entry{e.Location, e.Message})
		}
		if !slices.Equal(got, want) {
			t.Errorf("errors = %v, want %v", got, want)
		}
		noSecrets(t, "the answer", rec.Body.Bytes())
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("writes = %v, nothing is sent for a spec naming objects that don't exist", w)
		}
		if rows := f.serviceAudits(t); len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "objects_missing" {
			t.Errorf("service rows = %+v, want the one objects_missing failure", rows)
		}
	}
	t.Run("an apply missing only a secret", func(t *testing.T) {
		f, e := actionsEnv(t, nil, editableService(nil))
		op := f.revealCaller(t, e, "operator-1")
		_, body := op.maskedSpec("svc-api", secretOnly)
		check(t, f, op.apply("svc-api", body),
			"The spec names objects that don't exist: secrets db-pass. Create them first.",
			[]entry{{"secrets", "db-pass"}})
	})
	t.Run("a create missing one of each", func(t *testing.T) {
		f, e := actionsEnv(t, nil, editableService(nil))
		admin := f.caller(t, e, "envadmin-1", "admin")
		check(t, f, admin.create(chatBody(t, refsStaged, nil)),
			"The spec names objects that don't exist: networks front; secrets db-pass; configs app.conf. Create them first.",
			oneOfEach)
	})
}

// addSecretRef appends a reference to the secret db-pass, by name, to the container spec of a spec
// as the Spec tab holds it.
func addSecretRef(spec map[string]any) {
	cs := containerSpecOf(spec)
	refs, _ := cs["Secrets"].([]any)
	cs["Secrets"] = append(refs, map[string]any{
		"SecretName": "db-pass",
		"File":       map[string]any{"Name": "/run/secrets/db-pass", "UID": "0", "GID": "0", "Mode": 0o444},
	})
}

func TestServiceApplyASecretTheServiceDoesNotUseNeedsTheRevealPermission(t *testing.T) {
	withDBPass := func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = []dockertest.FakeSecret{{ID: "sec-db", Name: "db-pass"}}
	}

	t.Run("an operator adds a secret", func(t *testing.T) {
		f, e := actionsEnv(t, withDBPass, editableService(nil))
		op := f.caller(t, e, "operator-1", "operator")
		_, body := op.maskedSpec("svc-api", addSecretRef)

		for _, route := range []struct {
			name string
			do   func(service, body string) *httptest.ResponseRecorder
		}{{"review", op.review}, {"apply", op.apply}} {
			rec := route.do("svc-api", body)
			if rec.Code != http.StatusForbidden || problemType(t, rec) != "secret_reveal_required" {
				t.Fatalf("%s by an operator: %d %s, want 403 secret_reveal_required", route.name, rec.Code, rec.Body)
			}
			var prob actionProblem
			if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
				t.Fatal(err)
			}
			if len(prob.Errors) != 1 || prob.at("TaskTemplate.ContainerSpec.Secrets[0]") != "db-pass" {
				t.Errorf("%s errors = %+v, want exactly Secrets[0]: db-pass", route.name, prob.Errors)
			}
			if !strings.Contains(prob.Detail, "db-pass") || !strings.Contains(prob.Detail, "needs the reveal secrets permission") {
				t.Errorf("%s detail = %q, want the secret named and the permission said", route.name, prob.Detail)
			}
			noSecrets(t, route.name+" answer", rec.Body.Bytes())
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Fatalf("writes = %v, a refused spec is never sent", w)
		}
		rows := f.serviceAudits(t)
		if len(rows) != 1 || rows[0].Action != audit.ActionServiceUpdated || rows[0].Result != string(audit.ResultFailure) ||
			rows[0].Reason != "secret_reveal_required" || rows[0].Details["code"] != "secret_reveal_required" {
			t.Fatalf("audit rows = %+v, want one service.updated failure with reason secret_reveal_required (a review is not audited)", rows)
		}
		b, _ := json.Marshal(rows)
		noSecrets(t, "the audit rows", b)
	})

	usesDBPass := func(fs *dockertest.FakeService) {
		fs.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
			SecretID: "sec-db", SecretName: "db-pass",
			File: &swarm.SecretReferenceFileTarget{Name: "/run/secrets/db-pass", UID: "0", GID: "0", Mode: 0o444},
		}}
	}
	secretRefOf := func(spec map[string]any) map[string]any {
		refs, _ := containerSpecOf(spec)["Secrets"].([]any)
		ref, _ := refs[0].(map[string]any)
		return ref
	}

	t.Run("an operator keeps the service's secret and changes the replicas", func(t *testing.T) {
		f, e := actionsEnv(t, withDBPass, editableService(usesDBPass))
		op := f.caller(t, e, "operator-1", "operator")
		_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
			mode, _ := spec["Mode"].(map[string]any)
			rep, _ := mode["Replicated"].(map[string]any)
			rep["Replicas"] = 3
		})
		if rec := op.apply("svc-api", body); rec.Code != http.StatusAccepted {
			t.Fatalf("apply by an operator: %d %s, want 202", rec.Code, rec.Body)
		}
		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 || ups[0].Spec.Mode.Replicated == nil || *ups[0].Spec.Mode.Replicated.Replicas != 3 {
			t.Fatalf("updates = %+v, want one with 3 replicas", ups)
		}
	})

	t.Run("an operator changes the mode of the secret the service uses", func(t *testing.T) {
		f, e := actionsEnv(t, withDBPass, editableService(usesDBPass))
		op := f.caller(t, e, "operator-1", "operator")
		_, body := op.maskedSpec("svc-api", func(spec map[string]any) {
			secretRefOf(spec)["File"].(map[string]any)["Mode"] = 0o400
		})
		if rec := op.apply("svc-api", body); rec.Code != http.StatusAccepted {
			t.Fatalf("apply by an operator: %d %s, want 202", rec.Code, rec.Body)
		}
		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("updates = %+v, want one", ups)
		}
		refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
		if len(refs) != 1 || refs[0].SecretID != "sec-db" || refs[0].File.Mode != 0o400 {
			t.Errorf("secrets sent = %+v, want sec-db at mode 0400", refs)
		}
	})

	t.Run("an env admin adds the secret", func(t *testing.T) {
		f, e := actionsEnv(t, withDBPass, editableService(nil))
		admin := f.caller(t, e, "envadmin-1", "admin")
		_, body := admin.maskedSpec("svc-api", addSecretRef)
		if rec := admin.apply("svc-api", body); rec.Code != http.StatusAccepted {
			t.Fatalf("apply by an env admin: %d %s, want 202", rec.Code, rec.Body)
		}
		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("updates = %+v, want the admin's one", ups)
		}
		refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
		if len(refs) != 1 || refs[0].SecretID != "sec-db" {
			t.Errorf("secrets sent = %+v, want one carrying SecretID sec-db", refs)
		}
	})
}

// TestServiceApplyCannotAttachASecretByGivingAUsedReferenceAnotherSecretsName is the two-step rename of
// review finding CR-01: keep a used SecretID under another secret's name, then name that secret alone.
// A secret reference is identified by its engine ID, so the first step is refused for everyone and the
// second still needs secrets.reveal.
func TestServiceApplyCannotAttachASecretByGivingAUsedReferenceAnotherSecretsName(t *testing.T) {
	twoSecrets := func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = []dockertest.FakeSecret{{ID: "sec-db", Name: "db-pass"}, {ID: "sec-victim", Name: "victim-secret"}}
	}
	file := func() map[string]any {
		return map[string]any{"Name": "/run/secrets/db-pass", "UID": "0", "GID": "0", "Mode": 0o444}
	}
	usesDBPass := func(fs *dockertest.FakeService) {
		fs.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
			SecretID: "sec-db", SecretName: "db-pass",
			File: &swarm.SecretReferenceFileTarget{Name: "/run/secrets/db-pass", UID: "0", GID: "0", Mode: 0o444},
		}}
	}
	// storedMismatch is a service whose stored spec carries the misleading name, as one stored before
	// this release may.
	storedMismatch := func(fs *dockertest.FakeService) {
		usesDBPass(fs)
		fs.Spec.TaskTemplate.ContainerSpec.Secrets[0].SecretName = "victim-secret"
	}
	setRef := func(ref map[string]any) func(spec map[string]any) {
		return func(spec map[string]any) { containerSpecOf(spec)["Secrets"] = []any{ref} }
	}
	setReplicas := func(spec map[string]any) {
		mode, _ := spec["Mode"].(map[string]any)
		rep, _ := mode["Replicated"].(map[string]any)
		rep["Replicas"] = 3
	}
	specInvalid := func(t *testing.T, what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "spec_invalid" {
			t.Fatalf("%s: %d %s, want 422 spec_invalid", what, rec.Code, rec.Body)
		}
		var prob actionProblem
		if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
			t.Fatal(err)
		}
		msg := prob.at("spec")
		if !strings.Contains(msg, "TaskTemplate.ContainerSpec.Secrets[0]") || !strings.Contains(msg, "victim-secret") {
			t.Errorf("%s: the spec entry = %q (errors %+v), want the path and victim-secret named", what, msg, prob.Errors)
		}
	}

	t.Run("step 1: a used ID under another secret's name is refused for an operator and an env admin", func(t *testing.T) {
		for _, role := range []string{"operator", "admin"} {
			f, e := actionsEnv(t, twoSecrets, editableService(usesDBPass))
			c := f.caller(t, e, role+"-1", role)
			_, body := c.maskedSpec("svc-api", setRef(map[string]any{"SecretID": "sec-db", "SecretName": "victim-secret", "File": file()}))
			specInvalid(t, role+" review", c.review("svc-api", body))
			specInvalid(t, role+" apply", c.apply("svc-api", body))
			if w := f.fake.Writes(); len(w) != 0 {
				t.Fatalf("%s: writes = %v, a refused spec is never sent", role, w)
			}
			rows := f.serviceAudits(t)
			if len(rows) != 1 || rows[0].Action != audit.ActionServiceUpdated || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "spec_invalid" {
				t.Fatalf("%s: audit rows = %+v, want one service.updated failure with reason spec_invalid (a review is not audited)", role, rows)
			}
		}
	})

	t.Run("step 2: naming that secret alone, from a stored mismatch, needs secrets.reveal", func(t *testing.T) {
		alone := setRef(map[string]any{"SecretName": "victim-secret", "File": file()})

		f, e := actionsEnv(t, twoSecrets, editableService(storedMismatch))
		op := f.caller(t, e, "operator-1", "operator")
		_, body := op.maskedSpec("svc-api", alone)
		for name, do := range map[string]func(service, body string) *httptest.ResponseRecorder{"review": op.review, "apply": op.apply} {
			rec := do("svc-api", body)
			if rec.Code != http.StatusForbidden || problemType(t, rec) != "secret_reveal_required" {
				t.Fatalf("%s by an operator: %d %s, want 403 secret_reveal_required", name, rec.Code, rec.Body)
			}
			var prob actionProblem
			if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
				t.Fatal(err)
			}
			if len(prob.Errors) != 1 || prob.at("TaskTemplate.ContainerSpec.Secrets[0]") != "victim-secret" {
				t.Errorf("%s errors = %+v, want exactly Secrets[0]: victim-secret", name, prob.Errors)
			}
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Fatalf("writes = %v, a refused spec is never sent", w)
		}

		f, e = actionsEnv(t, twoSecrets, editableService(storedMismatch))
		admin := f.caller(t, e, "envadmin-1", "admin")
		_, body = admin.maskedSpec("svc-api", alone)
		if rec := admin.apply("svc-api", body); rec.Code != http.StatusAccepted {
			t.Fatalf("apply by an env admin: %d %s, want 202", rec.Code, rec.Body)
		}
		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("updates = %+v, want the admin's one", ups)
		}
		refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
		if len(refs) != 1 || refs[0].SecretID != "sec-victim" {
			t.Errorf("secrets sent = %+v, want one carrying victim-secret's own ID sec-victim", refs)
		}
	})

	t.Run("a spec stored with the mismatch is sent again once its name is corrected", func(t *testing.T) {
		f, e := actionsEnv(t, twoSecrets, editableService(storedMismatch))
		op := f.caller(t, e, "operator-1", "operator")
		_, body := op.maskedSpec("svc-api", setReplicas)
		specInvalid(t, "an unchanged mismatching reference with new replicas", op.apply("svc-api", body))
		if w := f.fake.Writes(); len(w) != 0 {
			t.Fatalf("writes = %v, a refused spec is never sent", w)
		}

		_, body = op.maskedSpec("svc-api", func(spec map[string]any) {
			setReplicas(spec)
			setRef(map[string]any{"SecretID": "sec-db", "SecretName": "db-pass", "File": file()})(spec)
		})
		if rec := op.apply("svc-api", body); rec.Code != http.StatusAccepted {
			t.Fatalf("apply with the name corrected: %d %s, want 202", rec.Code, rec.Body)
		}
		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("updates = %+v, want one", ups)
		}
		refs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets
		if len(refs) != 1 || refs[0].SecretID != "sec-db" || refs[0].SecretName != "db-pass" {
			t.Errorf("secrets sent = %+v, want sec-db named db-pass", refs)
		}
	})
}
