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

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// envRecOf is a recorded answer in the shape the default caller's helpers return.
func envRecOf(t *testing.T, rec *httptest.ResponseRecorder) *envRec {
	t.Helper()
	return &envRec{code: rec.Code, body: rec.Body.Bytes(), ptyp: problemType(t, rec)}
}

// Force update, rollback and the spec route (plan 06-08, RES-02, D-08, D-11, E23). Docker is a
// fake daemon behind the real monitor, aggregator and cluster reader; the router, policy, CSRF
// guard, registry resolver and audit writer are the real ones.

type forceUpdateAnswer struct {
	Image         string `json:"image"`
	Digest        string `json:"digest"`
	Pinned        bool   `json:"pinned"`
	PreviousImage string `json:"previousImage"`
	Reason        string `json:"reason"`
}

type actionProblem struct {
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func (p actionProblem) at(location string) string {
	for _, e := range p.Errors {
		if e.Location == location {
			return e.Message
		}
	}
	return ""
}

// actionsEnv is a local swarm manager environment whose daemon is configured by tune.
func actionsEnv(t *testing.T, tune func(*dockertest.FakeConfig), services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	cfg := swarmManagerEngine(services...)
	if tune != nil {
		tune(&cfg)
	}
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f, e
}

func (f *envFix) forceUpdate(t *testing.T, e envBody, service, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/"+service+"/force-update", body)
}

func (f *envFix) rollback(t *testing.T, e envBody, service, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/services/"+service+"/rollback", body)
}

// webImageRef is the tag webService runs, as the registry is asked for it.
const webImageRef = "localhost:5000/private/web:1"

func TestForceUpdateRepinsTheTag(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	f.addPullCredential(t, "local-bot", "")

	rec := f.forceUpdate(t, e, "svc-web", `{}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[forceUpdateAnswer](t, rec)
	digest := dockertest.FakeDigestOf(webImageRef)
	if !got.Pinned || got.Digest != digest || got.Image != webImageRef+"@"+digest || got.PreviousImage != scaleImage || got.Reason != "" {
		t.Fatalf("answer = %+v, want %s@%s pinned", got, webImageRef, digest)
	}

	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "svc-web" || u.Version != "4" || u.Query.Get("rollback") != "" {
		t.Errorf("update = %+v, want svc-web at version 4", u)
	}
	if cs := u.Spec.TaskTemplate.ContainerSpec; cs == nil || cs.Image != got.Image || u.Spec.TaskTemplate.ForceUpdate != 1 {
		t.Errorf("spec = %+v, want the pinned image and ForceUpdate 1", u.Spec.TaskTemplate)
	}
	if u.Spec.Labels["com.docker.stack.namespace"] != "shop" {
		t.Errorf("the stack label is gone: %+v", u.Spec.Labels)
	}
	if want := []swarm.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}; u.Spec.TaskTemplate.Placement == nil || !slices.Equal(u.Spec.TaskTemplate.Placement.Platforms, want) {
		t.Errorf("placement = %+v, want the image's platforms", u.Spec.TaskTemplate.Placement)
	}
	user, password, server := decodedPullAuth(t, u.Auth)
	if user != "bot" || password != pullCredSecret || server != "localhost:5000" {
		t.Errorf("the update carried the credential %q/%q at %q", user, password, server)
	}
	if refs := f.fake.DistributionRefs(); !slices.Equal(refs, []string{webImageRef}) {
		t.Errorf("the registry was asked for %q, want the tag", refs)
	}
	if auths := f.fake.DistributionAuth(); len(auths) != 1 || auths[0] != u.Auth {
		t.Errorf("the lookup carried %q, want the credential the update carried", auths)
	}
	if strings.Contains(string(rec.body), pullCredSecret) {
		t.Error("the answer holds the credential")
	}

	rows := f.serviceAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Action != audit.ActionServiceForceUpdated || row.Result != string(audit.ResultSuccess) || row.TargetType != "service" || row.TargetID != "svc-web" || row.EnvID != e.ID {
		t.Errorf("audit row = %+v", row)
	}
	for k, want := range map[string]string{"image": got.Image, "digest": digest, "retry": "false", "pinned": "true", "code": "done", "name": "web"} {
		if row.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, row.Details[k], want, row.Details)
		}
	}
	for k, v := range row.Details {
		if strings.Contains(v, pullCredSecret) || strings.Contains(k, "auth") {
			t.Errorf("audit detail %s=%q holds a credential", k, v)
		}
	}
}

func TestForceUpdateRepeatedPicksUpAMovedTagAndIsItsOwnRollout(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	first := decode[forceUpdateAnswer](t, f.forceUpdate(t, e, "web", `{}`))

	moved := "sha256:" + strings.Repeat("9", 64)
	f.fake.SetDistributionDigest(webImageRef, moved)
	// The second call strips the digest it wrote the first time and resolves the tag again.
	rec := f.forceUpdate(t, e, "web", `{"retry":true}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	second := decode[forceUpdateAnswer](t, rec)
	if second.Digest != moved || second.Image != webImageRef+"@"+moved || second.PreviousImage != first.Image {
		t.Fatalf("second = %+v, want the moved digest and the first pin as the previous image (first %+v)", second, first)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 2 || ups[0].Spec.TaskTemplate.ForceUpdate != 1 || ups[1].Spec.TaskTemplate.ForceUpdate != 2 || ups[1].Version != "5" {
		t.Fatalf("updates = %+v, want ForceUpdate 1 then 2 at versions 4 then 5", ups)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 2 || rows[0].Details["retry"] != "false" || rows[1].Details["retry"] != "true" || rows[1].Details["digest"] != moved {
		t.Errorf("audit rows = %+v, want one row per call, the second a retry", rows)
	}
}

func TestForceUpdateFailsWhenTheDigestCannotBeResolved(t *testing.T) {
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.PullStatus = dockertest.FakePullStatus{Code: http.StatusUnauthorized, Message: "unauthorized: authentication required"}
	}, webService())
	f.addPullCredential(t, "local-bot", "")

	rec := f.forceUpdate(t, e, "svc-web", `{}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "image_unresolved" {
		t.Fatalf("status = %d %s (%s), want 422 image_unresolved", rec.code, rec.body, rec.ptyp)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.body, &prob); err != nil {
		t.Fatal(err)
	}
	if prob.at("image.ref") != scaleImage || prob.at("image.host") != "localhost:5000" || prob.at("reason") != dockerx.PullCodeUnauthorized {
		t.Errorf("errors = %+v, want the ref, the registry host and the reason", prob.Errors)
	}
	if ups := f.fake.ServiceUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none: a failed lookup never falls back to the unpinned tag", ups)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if strings.Contains(string(rec.body), pullCredSecret) {
		t.Error("the problem holds the credential")
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "image_unresolved" || rows[0].Details["code"] != "image_unresolved" {
		t.Fatalf("audit rows = %+v, want one failure image_unresolved", rows)
	}
}

func TestForceUpdateDigestOnly(t *testing.T) {
	pinned := "localhost:5000/private/web@sha256:" + strings.Repeat("7", 64)
	svc := webService()
	svc.Image = pinned
	f, e := actionsEnv(t, nil, svc)

	rec := f.forceUpdate(t, e, "svc-web", `{}`)
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[forceUpdateAnswer](t, rec)
	if got.Pinned || got.Reason != "digest_only" || got.Image != pinned || got.PreviousImage != pinned || got.Digest != "sha256:"+strings.Repeat("7", 64) {
		t.Fatalf("answer = %+v, want pinned false, reason digest_only and the image as it was", got)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Spec.TaskTemplate.ForceUpdate != 1 || ups[0].Spec.TaskTemplate.ContainerSpec.Image != pinned {
		t.Fatalf("updates = %+v, want one with ForceUpdate 1 and the same image", ups)
	}
	if refs := f.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("the registry was asked %q, want nothing", refs)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Details["pinned"] != "false" || rows[0].Result != string(audit.ResultSuccess) {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestForceUpdateWithAStaleVersionChangesNothing(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	rec := f.forceUpdate(t, e, "svc-web", `{"version":3}`)
	if rec.code != http.StatusConflict || rec.ptyp != "service_changed" {
		t.Fatalf("status = %d %s (%s), want 409 service_changed", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "service_changed" {
		t.Errorf("audit rows = %+v, want one failure service_changed", rows)
	}
	// The version it was shown is accepted.
	if rec := f.forceUpdate(t, e, "svc-web", `{"version":4}`); rec.code != http.StatusOK {
		t.Errorf("at the current version: %d %s", rec.code, rec.body)
	}
}

func TestForceUpdateOfAnUnknownServiceIsNotFoundAndNotAudited(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	rec := f.forceUpdate(t, e, "ghost", `{}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Fatalf("status = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a service that does not exist", rows)
	}
}

func TestForceUpdateAndRollbackOfAStandaloneHostAreNotASwarm(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, rec := range map[string]*envRec{
		"force update": f.forceUpdate(t, e, "web", `{}`),
		"rollback":     f.rollback(t, e, "web", `{"version":1}`),
		"spec":         f.get(t, e, "services/web/spec"),
	} {
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("%s = %d %s (%s), want 409 not_a_swarm", name, rec.code, rec.body, rec.ptyp)
		}
	}
}

func TestForceUpdateAndRollbackNeedServicesControl(t *testing.T) {
	f, e := actionsEnv(t, nil, rollbackService())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "viewer", false)
	for path, body := range map[string]string{"force-update": `{}`, "rollback": `{"version":9}`} {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/services/svc-web/" + path, body: body, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Errorf("a viewer's %s = %d: %s, want 403", path, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	// A viewer may still read the spec.
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/services/svc-web/spec", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Errorf("a viewer's spec = %d: %s", rec.Code, rec.Body)
	}
}

// rollbackService is a service updated once: web:2 now, web:1 before, at version 9.
func rollbackService() dockertest.FakeService {
	spec := func(image string) *swarm.ServiceSpec {
		replicas := uint64(2)
		return &swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "web", Labels: map[string]string{"com.docker.stack.namespace": "shop"}},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		}
	}
	return dockertest.FakeService{
		ID: "svc-web", Name: "web", Mode: "replicated", Running: 2, Desired: 2, Version: 9,
		Spec: spec("localhost:5000/private/web:2"), PreviousSpec: spec("localhost:5000/private/web:1"),
	}
}

func TestRollbackSendsTheCurrentSpecAndSwapsThemAndRepeatingTogglesBack(t *testing.T) {
	f, e := actionsEnv(t, nil, rollbackService())
	f.addPullCredential(t, "local-bot", "")

	rec := f.rollback(t, e, "svc-web", `{"version":9}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got := decode[struct {
		Image string `json:"image"`
	}](t, rec); got.Image != "localhost:5000/private/web:1" {
		t.Errorf("answer = %s, want the image rolled back to", rec.body)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.ServiceID != "svc-web" || u.Version != "9" || u.Query.Get("rollback") != "previous" {
		t.Errorf("update = %+v, want version 9 with rollback=previous", u)
	}
	if cs := u.Spec.TaskTemplate.ContainerSpec; cs == nil || cs.Image != "localhost:5000/private/web:2" {
		t.Errorf("body spec = %+v, want the current spec", u.Spec.TaskTemplate)
	}
	user, password, server := decodedPullAuth(t, u.Auth)
	if user != "bot" || password != pullCredSecret || server != "localhost:5000" {
		t.Errorf("the rollback carried the credential %q/%q at %q", user, password, server)
	}
	if refs := f.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("the registry was asked %q, want nothing: a rollback pins nothing", refs)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionServiceRolledBack || rows[0].Result != string(audit.ResultSuccess) ||
		rows[0].Details["version"] != "9" || rows[0].Details["code"] != "done" || rows[0].Details["name"] != "web" {
		t.Fatalf("audit rows = %+v", rows)
	}

	// The spec route now shows the two swapped, at the next version.
	spec := decode[sdSpecAnswer](t, f.get(t, e, "services/svc-web/spec"))
	if spec.Version != 10 || spec.imageOf(spec.Spec) != "localhost:5000/private/web:1" || spec.imageOf(spec.PreviousSpec) != "localhost:5000/private/web:2" {
		t.Fatalf("spec after = %+v", spec)
	}
	// Repeating goes back to the spec just replaced.
	if rec := f.rollback(t, e, "svc-web", `{"version":10}`); rec.code != http.StatusAccepted {
		t.Fatalf("second rollback = %d: %s", rec.code, rec.body)
	}
	spec = decode[sdSpecAnswer](t, f.get(t, e, "services/svc-web/spec"))
	if spec.imageOf(spec.Spec) != "localhost:5000/private/web:2" {
		t.Errorf("spec after two rollbacks = %+v, want web:2 again", spec)
	}
}

func TestRollbackWithoutPreviousSpec(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	rec := f.rollback(t, e, "svc-web", `{"version":4}`)
	if rec.code != http.StatusConflict || rec.ptyp != "no_previous_spec" {
		t.Fatalf("status = %d %s (%s), want 409 no_previous_spec", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "no_previous_spec" {
		t.Errorf("audit rows = %+v, want one failure no_previous_spec", rows)
	}
}

func TestRollbackObjectMissing(t *testing.T) {
	svc := rollbackService()
	svc.RollbackError = "secret not found: db_password"
	f, e := actionsEnv(t, nil, svc)
	rec := f.rollback(t, e, "svc-web", `{"version":9}`)
	if rec.code != http.StatusConflict || rec.ptyp != "rollback_object_missing" {
		t.Fatalf("status = %d %s (%s), want 409 rollback_object_missing", rec.code, rec.body, rec.ptyp)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.body, &prob); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prob.Detail, "secret not found: db_password") {
		t.Errorf("detail = %q, want the manager's words", prob.Detail)
	}
	if prob.at("object.kind") != "secret" || prob.at("object.name") != "db_password" {
		t.Errorf("errors = %+v, want the kind and the name of what is gone", prob.Errors)
	}
	if got := len(f.fake.Writes()); got != 1 {
		t.Errorf("writes = %d, want exactly one: never retried", got)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "rollback_object_missing" {
		t.Errorf("audit rows = %+v", rows)
	}
}

func TestRollbackNamesANetworkThatIsGone(t *testing.T) {
	svc := rollbackService()
	svc.RollbackError = "network front_net not found"
	svc.PreviousSpec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "front_net"}}
	f, e := actionsEnv(t, nil, svc)
	op := f.caller(t, e, "operator-1", "operator")
	for who, rec := range map[string]*envRec{
		"the default caller": f.rollback(t, e, "svc-web", `{"version":9}`),
		"an operator":        envRecOf(t, op.do(http.MethodPost, "services/svc-web/rollback", `{"version":9}`)),
	} {
		if rec.code != http.StatusConflict || rec.ptyp != "rollback_object_missing" {
			t.Fatalf("%s: status = %d %s (%s), want 409 rollback_object_missing", who, rec.code, rec.body, rec.ptyp)
		}
		var prob actionProblem
		if err := json.Unmarshal(rec.body, &prob); err != nil {
			t.Fatal(err)
		}
		if prob.at("object.kind") != "network" || prob.at("object.name") != "front_net" {
			t.Errorf("%s: errors = %+v, want network front_net", who, prob.Errors)
		}
	}
	// An unresolved network is not a finding: both calls reached the manager, which refused them.
	if got := len(f.fake.Writes()); got != 2 {
		t.Errorf("writes = %d, want one per caller", got)
	}
}

// privilegedRollback is a standalone service whose current spec is clean and whose previous spec
// is prev, at version 9.
func privilegedRollback(prev func(*swarm.ServiceSpec)) dockertest.FakeService {
	spec := func() *swarm.ServiceSpec {
		replicas := uint64(1)
		return &swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "api"},
			TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "localhost:5000/private/api:2"}},
			Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		}
	}
	before := spec()
	before.TaskTemplate.ContainerSpec.Image = "localhost:5000/private/api:1"
	if prev != nil {
		prev(before)
	}
	return dockertest.FakeService{
		ID: "svc-priv", Name: "api", Mode: "replicated", Running: 1, Desired: 1, Version: 9,
		Spec: spec(), PreviousSpec: before,
	}
}

func TestRollbackToAPrivilegedPreviousSpecIsRefusedForAnOperatorAndAppliedForAnEnvAdmin(t *testing.T) {
	f, e := actionsEnv(t, nil, privilegedRollback(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/sock"}}
	}))
	op := f.caller(t, e, "operator-1", "operator")

	rec := op.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_service" {
		t.Fatalf("rollback by an operator: %d %s, want 403 privileged_service", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if prob.at("TaskTemplate.ContainerSpec.Mounts[0].Source") != "/var/run/docker.sock" {
		t.Errorf("problem = %+v, want each finding listed", prob)
	}
	if ups, w := f.fake.ServiceUpdates(), f.fake.Writes(); len(ups) != 0 || len(w) != 0 {
		t.Fatalf("updates = %v, writes = %v, a refused rollback sends nothing", ups, w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionServiceRolledBack || rows[0].Result != string(audit.ResultFailure) ||
		rows[0].Reason != "privileged_service" || rows[0].Details["code"] != "privileged_service" || rows[0].Details["findings"] != "1" ||
		rows[0].Details["name"] != "api" || rows[0].Details["version"] != "9" {
		t.Fatalf("audit rows = %+v, want one privileged_service failure with one finding", rows)
	}

	admin := f.caller(t, e, "envadmin-1", "admin")
	if rec := admin.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`); rec.Code != http.StatusAccepted {
		t.Fatalf("rollback by an env admin: %d %s", rec.Code, rec.Body)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 1 || ups[0].Query.Get("rollback") != "previous" || ups[0].Version != "9" {
		t.Errorf("updates = %+v, want the admin's one at version 9 with rollback=previous", ups)
	}
}

func TestRollbackToASpecWithASecretTheServiceDoesNotUseNeedsTheRevealPermission(t *testing.T) {
	withDBPass := func(cfg *dockertest.FakeConfig) {
		cfg.Secrets = []dockertest.FakeSecret{{ID: "sec-db", Name: "db-pass"}}
	}
	f, e := actionsEnv(t, withDBPass, privilegedRollback(func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
			SecretID: "sec-db", SecretName: "db-pass",
			File: &swarm.SecretReferenceFileTarget{Name: "/run/secrets/db-pass", UID: "0", GID: "0", Mode: 0o444},
		}}
	}), rollbackService())
	op := f.caller(t, e, "operator-1", "operator")

	rec := op.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "secret_reveal_required" {
		t.Fatalf("rollback by an operator: %d %s, want 403 secret_reveal_required", rec.Code, rec.Body)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
		t.Fatal(err)
	}
	if len(prob.Errors) != 1 || prob.at("TaskTemplate.ContainerSpec.Secrets[0]") != "db-pass" || !strings.Contains(prob.Detail, "db-pass") {
		t.Errorf("problem = %+v, want Secrets[0]: db-pass named", prob)
	}
	if ups, w := f.fake.ServiceUpdates(), f.fake.Writes(); len(ups) != 0 || len(w) != 0 {
		t.Fatalf("updates = %v, writes = %v, a refused rollback sends nothing", ups, w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionServiceRolledBack || rows[0].Result != string(audit.ResultFailure) ||
		rows[0].Reason != "secret_reveal_required" || rows[0].Details["code"] != "secret_reveal_required" || rows[0].Details["name"] != "api" {
		t.Fatalf("audit rows = %+v, want one secret_reveal_required failure", rows)
	}

	// A previous spec that adds no secret is unaffected, whoever rolls back.
	if rec := op.do(http.MethodPost, "services/svc-web/rollback", `{"version":9}`); rec.Code != http.StatusAccepted {
		t.Fatalf("rollback by an operator of a service with no secrets: %d %s, want 202", rec.Code, rec.Body)
	}

	admin := f.caller(t, e, "envadmin-1", "admin")
	if rec := admin.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`); rec.Code != http.StatusAccepted {
		t.Fatalf("rollback by an env admin: %d %s", rec.Code, rec.Body)
	}
	var privUpdates int
	for _, u := range f.fake.ServiceUpdates() {
		if u.ServiceID == "svc-priv" && u.Query.Get("rollback") == "previous" && u.Version == "9" {
			privUpdates++
		}
	}
	if privUpdates != 1 {
		t.Errorf("updates = %+v, want the admin's one at rollback=previous", f.fake.ServiceUpdates())
	}
}

func TestRollbackByAnOperatorToAPreviousSpecWithNoFindingIsAccepted(t *testing.T) {
	cases := []struct {
		name string
		svc  dockertest.FakeService
	}{
		{"a clean image", privilegedRollback(nil)},
		{"a bind under /srv", privilegedRollback(func(s *swarm.ServiceSpec) {
			s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/srv/data", Target: "/data"}}
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, e := actionsEnv(t, nil, tc.svc)
			op := f.caller(t, e, "operator-1", "operator")
			if rec := op.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`); rec.Code != http.StatusAccepted {
				t.Fatalf("rollback by an operator: %d %s, want 202", rec.Code, rec.Body)
			}
			ups := f.fake.ServiceUpdates()
			if len(ups) != 1 || ups[0].Query.Get("rollback") != "previous" {
				t.Errorf("updates = %+v, want one with rollback=previous", ups)
			}
		})
	}
}

func TestRollbackToEachHostEscalationFormIsRefusedForAnOperator(t *testing.T) {
	const hostNetID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name     string
		tune     func(*dockertest.FakeConfig)
		prev     func(*swarm.ServiceSpec)
		location string
		message  string
	}{
		{
			name: "bind of /etc",
			prev: func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/etc", Target: "/host-etc"}}
			},
			location: "TaskTemplate.ContainerSpec.Mounts[0].Source", message: "/etc",
		},
		{
			name: "added capability",
			prev: func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.CapabilityAdd = []string{"SYS_ADMIN"}
			},
			location: "TaskTemplate.ContainerSpec.CapabilityAdd[0]", message: "SYS_ADMIN",
		},
		{
			name: "lowered sandbox",
			prev: func(s *swarm.ServiceSpec) {
				s.TaskTemplate.ContainerSpec.Privileges = &swarm.Privileges{Seccomp: &swarm.SeccompOpts{Mode: swarm.SeccompModeUnconfined}}
			},
			location: "TaskTemplate.ContainerSpec.Privileges.Seccomp.Mode", message: "unconfined",
		},
		{
			name: "host network by name",
			prev: func(s *swarm.ServiceSpec) {
				s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "host"}}
			},
			location: "TaskTemplate.Networks[0].Target", message: "host",
		},
		{
			name: "host network by ID",
			tune: func(c *dockertest.FakeConfig) {
				c.Networks = []dockertest.FakeNetwork{{ID: hostNetID, Name: "host", Driver: "host", Scope: "local"}}
			},
			prev: func(s *swarm.ServiceSpec) {
				s.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: hostNetID}}
			},
			location: "TaskTemplate.Networks[0].Target", message: hostNetID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, e := actionsEnv(t, tc.tune, privilegedRollback(tc.prev))
			op := f.caller(t, e, "operator-1", "operator")
			rec := op.do(http.MethodPost, "services/svc-priv/rollback", `{"version":9}`)
			if rec.Code != http.StatusForbidden || problemType(t, rec) != "privileged_service" {
				t.Fatalf("rollback by an operator: %d %s, want 403 privileged_service", rec.Code, rec.Body)
			}
			var prob actionProblem
			if err := json.Unmarshal(rec.Body.Bytes(), &prob); err != nil {
				t.Fatal(err)
			}
			if prob.at(tc.location) != tc.message {
				t.Errorf("problem = %+v, want %s: %s", prob, tc.location, tc.message)
			}
			if ups, w := f.fake.ServiceUpdates(), f.fake.Writes(); len(ups) != 0 || len(w) != 0 {
				t.Errorf("updates = %v, writes = %v, a refused rollback sends nothing", ups, w)
			}
		})
	}
}

func TestRollbackWithAStaleVersionChangesNothing(t *testing.T) {
	f, e := actionsEnv(t, nil, rollbackService())
	rec := f.rollback(t, e, "svc-web", `{"version":8}`)
	if rec.code != http.StatusConflict || rec.ptyp != "service_changed" {
		t.Fatalf("status = %d %s (%s), want 409 service_changed", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestRollbackNeedsAVersion(t *testing.T) {
	f, e := actionsEnv(t, nil, rollbackService())
	for _, body := range []string{`{}`, `{"version":0}`} {
		if rec := f.rollback(t, e, "svc-web", body); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d: %s, want 422", body, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

// sdSpecAnswer is the spec route's answer.
type sdSpecAnswer struct {
	Version      uint64         `json:"version"`
	Name         string         `json:"name"`
	Mode         string         `json:"mode"`
	Stack        string         `json:"stack"`
	Agent        bool           `json:"agent"`
	Masked       bool           `json:"masked"`
	Spec         map[string]any `json:"spec"`
	PreviousSpec map[string]any `json:"previousSpec"`
}

// imageOf reads TaskTemplate.ContainerSpec.Image of a spec document.
func (sdSpecAnswer) imageOf(spec map[string]any) string {
	tt, _ := spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	image, _ := cs["Image"].(string)
	return image
}

func TestServiceSpecIsMasked(t *testing.T) {
	f, e := sdEnv(t)
	get := f.asRole(t, e, "sd-spec-viewer", "viewer")
	rec := get("services/svc-api/spec")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	sdNoSecrets(t, "a viewer", rec)
	var got sdSpecAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 7 || got.Name != "api" || got.Mode != "replicated" || got.Stack != "shop" || got.Agent || !got.Masked {
		t.Errorf("identity = %+v", got)
	}
	for name, spec := range map[string]map[string]any{"spec": got.Spec, "previousSpec": got.PreviousSpec} {
		if spec == nil {
			t.Fatalf("%s is missing", name)
		}
		tt, _ := spec["TaskTemplate"].(map[string]any)
		cs, _ := tt["ContainerSpec"].(map[string]any)
		env, _ := cs["Env"].([]any)
		if len(env) != 3 || env[0] != "DB_PASSWORD=<hidden>" || env[1] != "MODE=<hidden>" || env[2] != "FLAG" {
			t.Errorf("%s env = %v", name, env)
		}
	}
	if img := got.imageOf(got.Spec); img != "registry.example.com/shop/api:1.4@"+sdDigest {
		t.Errorf("spec image = %q", img)
	}
	if img := got.imageOf(got.PreviousSpec); img != "registry.example.com/shop/api:1.3" {
		t.Errorf("previous image = %q", img)
	}

	// An Admin gets the values, and a service never updated has no previousSpec.
	adm := decode[sdSpecAnswer](t, f.get(t, e, "services/svc-api/spec"))
	tt, _ := adm.Spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	if env, _ := cs["Env"].([]any); adm.Masked || len(env) == 0 || env[0] != "DB_PASSWORD="+sdSecrets["env"] {
		t.Errorf("an Admin's spec = %s", f.get(t, e, "services/svc-api/spec").body)
	}
	plain := decode[map[string]any](t, f.get(t, e, "services/svc-web/spec"))
	if _, has := plain["previousSpec"]; has {
		t.Errorf("a service never updated has a previousSpec: %v", plain["previousSpec"])
	}
}

func TestServiceSpecOfAnUnknownServiceIsNotFound(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	rec := f.get(t, e, "services/ghost/spec")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Errorf("status = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
}

// Remove a service (plan 06-10, RES-02, D-05, D-13).

func (f *envFix) removeService(t *testing.T, e envBody, service, query string) *envRec {
	t.Helper()
	path := "/api/env/" + e.ID + "/services/" + service
	if query != "" {
		path += "?" + query
	}
	return f.req(t, http.MethodDelete, path, "")
}

// loneService is a service nobody's stack owns.
func loneService() dockertest.FakeService {
	return dockertest.FakeService{ID: "svc-lone", Name: "lone", Mode: "replicated", Running: 1, Desired: 1, Image: "lone:1", Version: 2}
}

// agentService is Bayplan's own agent, as the install command names it.
func agentService() dockertest.FakeService {
	return dockertest.FakeService{ID: "svc-agent", Name: "bayplan-agent", Mode: "global", Running: 1, Desired: 1, Image: "bayplan/agent:1", Version: 3}
}

func TestRemoveService(t *testing.T) {
	f, e := actionsEnv(t, nil, loneService(), webService())
	rec := f.removeService(t, e, "svc-lone", "")
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /services/svc-lone"}) {
		t.Errorf("writes = %v, want one DELETE of the service", w)
	}
	list := f.get(t, e, "services")
	if list.code != http.StatusOK || strings.Contains(string(list.body), "svc-lone") || !strings.Contains(string(list.body), "svc-web") {
		t.Errorf("services after the remove = %d %s, want the other service only", list.code, list.body)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want exactly one", rows)
	}
	r := rows[0]
	if r.Action != "service.removed" || r.Result != string(audit.ResultSuccess) || r.TargetID != "svc-lone" || r.EnvID != e.ID {
		t.Errorf("row = %+v, want service.removed success on svc-lone", r)
	}
	for k, want := range map[string]string{"name": "lone", "stack": "", "agent": "false", "confirmed": "false", "code": "done"} {
		if got, ok := r.Details[k]; !ok || got != want {
			t.Errorf("details[%s] = %q (present %v), want %q", k, got, ok, want)
		}
	}
	// A second remove finds nothing: it is not audited again.
	if again := f.removeService(t, e, "svc-lone", ""); again.code != http.StatusNotFound || again.ptyp != "service_not_found" {
		t.Errorf("second remove = %d %s (%s), want 404 service_not_found", again.code, again.body, again.ptyp)
	}
	if rows := f.serviceAudits(t); len(rows) != 1 {
		t.Errorf("audit rows after the second remove = %+v, want still one", rows)
	}
}

func TestRemoveServiceOfAnUnknownServiceIsNotFoundAndNotAudited(t *testing.T) {
	f, e := actionsEnv(t, nil, loneService())
	rec := f.removeService(t, e, "ghost", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "service_not_found" {
		t.Fatalf("status = %d %s (%s), want 404 service_not_found", rec.code, rec.body, rec.ptyp)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a service that does not exist", rows)
	}
}

func TestRemoveServiceOfAStandaloneHostIsNotASwarm(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.h = f.build(t)
	e := f.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	if rec := f.removeService(t, e, "web", ""); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("remove = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

func TestRemoveAgentServiceNeedsItsName(t *testing.T) {
	f, e := actionsEnv(t, nil, agentService())
	for _, query := range []string{"", "confirm=lone", "confirm=Bayplan-Agent"} {
		rec := f.removeService(t, e, "svc-agent", query)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("remove with %q = %d %s (%s), want 422 confirmation_required", query, rec.code, rec.body, rec.ptyp)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none before its name is typed", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %+v, want one failure per refused attempt", rows)
	}
	for _, r := range rows {
		if r.Action != "service.removed" || r.Result != string(audit.ResultFailure) || r.Reason != "confirmation_required" ||
			r.Details["agent"] != "true" || r.Details["confirmed"] != "false" || r.Details["name"] != "bayplan-agent" {
			t.Errorf("row = %+v, want a service.removed failure confirmation_required for the agent", r)
		}
	}

	rec := f.removeService(t, e, "svc-agent", "confirm=bayplan-agent")
	if rec.code != http.StatusNoContent {
		t.Fatalf("with its name: %d %s, want 204", rec.code, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /services/svc-agent"}) {
		t.Errorf("writes = %v, want one DELETE of the agent service", w)
	}
	rows = f.serviceAudits(t)
	last := rows[len(rows)-1]
	if last.Result != string(audit.ResultSuccess) || last.Details["agent"] != "true" || last.Details["confirmed"] != "true" || last.Details["code"] != "done" {
		t.Errorf("last row = %+v, want a success with agent true and confirmed true", last)
	}
}

func TestRemoveStackOwnedServiceIsRefused(t *testing.T) {
	f, e := actionsEnv(t, nil, webService())
	rec := f.removeService(t, e, "svc-web", "")
	if rec.code != http.StatusConflict || rec.ptyp != "stack_owned" {
		t.Fatalf("status = %d %s (%s), want 409 stack_owned", rec.code, rec.body, rec.ptyp)
	}
	var p actionProblem
	if err := json.Unmarshal(rec.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.at("stack") != "shop" || p.Detail != "Managed by stack shop. Change it through the stack." {
		t.Errorf("problem = %+v, want the stack shop named", p)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	rows := f.serviceAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "stack_owned" || rows[0].Details["stack"] != "shop" {
		t.Errorf("audit rows = %+v, want one failure stack_owned naming the stack", rows)
	}
	// Typing a name does not lift a stack's ownership.
	if rec := f.removeService(t, e, "svc-web", "confirm=web"); rec.code != http.StatusConflict || rec.ptyp != "stack_owned" {
		t.Errorf("with confirm = %d %s (%s), want 409 stack_owned", rec.code, rec.body, rec.ptyp)
	}
}

func TestRemoveServiceNeedsAdmin(t *testing.T) {
	f, e := actionsEnv(t, nil, loneService())
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	cookie, csrf := f.signIn(t, "operator", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/services/svc-lone", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Errorf("an operator's remove = %d: %s, want 403", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if rows := f.serviceAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused caller", rows)
	}

	f.addUser(t, "boss", false)
	f.addEnvRole(t, "boss", e.ID, "admin")
	cookie, csrf = f.signIn(t, "boss", false)
	rec = f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/services/svc-lone", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Errorf("an env admin's remove = %d: %s, want 204", rec.Code, rec.Body)
	}
}
