// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

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
	f, e := actionsEnv(t, nil, svc)
	rec := f.rollback(t, e, "svc-web", `{"version":9}`)
	if rec.code != http.StatusConflict || rec.ptyp != "rollback_object_missing" {
		t.Fatalf("status = %d %s (%s), want 409 rollback_object_missing", rec.code, rec.body, rec.ptyp)
	}
	var prob actionProblem
	if err := json.Unmarshal(rec.body, &prob); err != nil {
		t.Fatal(err)
	}
	if prob.at("object.kind") != "network" || prob.at("object.name") != "front_net" {
		t.Errorf("errors = %+v, want network front_net", prob.Errors)
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
