// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
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
	editDB    = "postgres://user:hunter2@db/app"
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
	return spec["TaskTemplate"].(map[string]any)["ContainerSpec"].(map[string]any)
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
	for _, x := range v.([]any) {
		out = append(out, x.(string))
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
			m["TaskTemplate"].(map[string]any)["Networks"] = []any{map[string]any{"Target": "front"}}
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
