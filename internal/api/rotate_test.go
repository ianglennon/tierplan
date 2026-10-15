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
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// Rotate a secret or config to a new version (plan 06-28, RES-08, D-26, T-06-81 to T-06-84). Docker is a
// fake daemon behind the real monitor, aggregator and cluster reader; the router, policy, CSRF guard,
// registry resolver and audit writer are the real ones.

const rotateValue = "rotated-value-5e1b" //nolint:gosec // G101: a fixture value the tests look for in every answer and audit row

type rotateUserBody struct {
	ServiceID string         `json:"serviceId"`
	Service   string         `json:"service"`
	Stack     string         `json:"stack"`
	Skipped   bool           `json:"skipped"`
	File      string         `json:"file"`
	UID       string         `json:"uid"`
	GID       string         `json:"gid"`
	Mode      string         `json:"mode"`
	Current   map[string]any `json:"current"`
	Next      map[string]any `json:"next"`
}

type rotatePreviewAnswer struct {
	Kind       string            `json:"kind"`
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	NewName    string            `json:"newName"`
	NameTaken  bool              `json:"nameTaken"`
	Templating string            `json:"templating"`
	Labels     map[string]string `json:"labels"`
	Users      []rotateUserBody  `json:"users"`
	Skipped    int               `json:"skipped"`
}

type rotateServiceAnswer struct {
	ServiceID string `json:"serviceId"`
	Service   string `json:"service"`
	Stack     string `json:"stack"`
	State     string `json:"state"`
	Code      string `json:"code"`
	Image     string `json:"image"`
}

type rotateAnswer struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	OldName    string                `json:"oldName"`
	Services   []rotateServiceAnswer `json:"services"`
	Updated    int                   `json:"updated"`
	Skipped    int                   `json:"skipped"`
	OldRemoved bool                  `json:"oldRemoved"`
	RemoveCode string                `json:"removeCode"`
}

type rotateProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string `json:"location"`
		Message  string `json:"message"`
	} `json:"errors"`
}

func (p rotateProblem) at(location string) []string {
	var out []string
	for _, e := range p.Errors {
		if e.Location == location {
			out = append(out, e.Message)
		}
	}
	return out
}

// rotateServices are three users of the secret dbpw and of the config app.conf: api (an editable spec),
// shop_web (a stack's) and worker (a plain row). They sort api, shop_web, worker.
func rotateServices(tune ...func(*dockertest.FakeService)) []dockertest.FakeService {
	secret := dockertest.FakeServiceObject{ID: "sec-db", Name: "dbpw", File: "/run/secrets/dbpw", UID: "70", GID: "71", Mode: 0o400}
	config := dockertest.FakeServiceObject{ID: "cfg-app", Name: "app.conf", File: "/etc/app.conf", UID: "0", GID: "0", Mode: 0o444}
	api := editableService(func(fs *dockertest.FakeService) {
		cs := fs.Spec.TaskTemplate.ContainerSpec
		cs.Secrets = []*swarm.SecretReference{{
			SecretID: "sec-db", SecretName: "dbpw", File: &swarm.SecretReferenceFileTarget{Name: "/run/secrets/dbpw", UID: "70", GID: "71", Mode: 0o400},
		}}
		cs.Configs = []*swarm.ConfigReference{{
			ConfigID: "cfg-app", ConfigName: "app.conf", File: &swarm.ConfigReferenceFileTarget{Name: "/etc/app.conf", UID: "0", GID: "0", Mode: 0o444},
		}}
	})
	out := []dockertest.FakeService{
		api,
		{ID: "svc-shop", Name: "shop_web", Mode: "replicated", Running: 1, Desired: 1, Image: "shop:1", Labels: map[string]string{"com.docker.stack.namespace": "shop"},
			Secrets: []dockertest.FakeServiceObject{secret}, Configs: []dockertest.FakeServiceObject{config}},
		{ID: "svc-worker", Name: "worker", Mode: "replicated", Running: 1, Desired: 1, Version: 3, Image: "worker:1",
			Secrets: []dockertest.FakeServiceObject{secret}, Configs: []dockertest.FakeServiceObject{config}},
	}
	for _, t := range tune {
		for i := range out {
			t(&out[i])
		}
	}
	return out
}

func rotateObjects(c *dockertest.FakeConfig) {
	c.Secrets = []dockertest.FakeSecret{{ID: "sec-db", Name: "dbpw", Labels: map[string]string{"app": "db"}}}
	c.Configs = []dockertest.FakeConfigObject{{ID: "cfg-app", Name: "app.conf", Labels: map[string]string{"app": "web"}, Data: []byte("level=info"), Templating: "golang"}}
}

func rotateEnv(t *testing.T, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	return actionsEnv(t, rotateObjects, services...)
}

func (f *envFix) rotateCall(t *testing.T, e envBody, kind, id, tail, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/"+kind+"/"+id+"/"+tail, body)
}

func (c *roleCaller) rotateCall(kind, id, tail, body string) *http.Response {
	rec := c.do(http.MethodPost, kind+"/"+id+"/"+tail, body)
	return rec.Result()
}

func rotateBody(value string, extra string) string {
	return `{"data":"` + b64(value) + `"` + extra + `}`
}

func serviceUpdatedRows(rows []auditRowFull) []auditRowFull {
	var out []auditRowFull
	for _, r := range rows {
		if r.Action == audit.ActionServiceUpdated {
			out = append(out, r)
		}
	}
	return out
}

func TestRotatePreviewListsEveryUserInNameOrderAndMarksTheStacksAsSkipped(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate/preview", `{}`)
	if rec.code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.code, rec.body)
	}
	got := decode[rotatePreviewAnswer](t, rec)
	if got.Kind != "secret" || got.ID != "sec-db" || got.Name != "dbpw" || got.NewName != "dbpw_v2" || got.NameTaken || got.Skipped != 1 || got.Labels["app"] != "db" {
		t.Fatalf("preview = %+v", got)
	}
	var names []string
	for _, u := range got.Users {
		names = append(names, u.Service)
	}
	if !slices.Equal(names, []string{"api", "shop_web", "worker"}) {
		t.Fatalf("users = %v, want name order", names)
	}
	shop, worker := got.Users[1], got.Users[2]
	if !shop.Skipped || shop.Stack != "shop" || shop.Current != nil || shop.Next != nil {
		t.Errorf("shop_web = %+v, want skipped with no diff", shop)
	}
	if worker.Skipped || worker.File != "/run/secrets/dbpw" || worker.UID != "70" || worker.GID != "71" || worker.Mode != "0400" || worker.Current == nil || worker.Next == nil {
		t.Errorf("worker = %+v, want its file target and both specs", worker)
	}
	if rec.code == http.StatusOK && !strings.Contains(string(rec.body), `"dbpw_v2"`) {
		t.Errorf("the next spec doesn't name dbpw_v2: %s", rec.body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a preview changes nothing", w)
	}
	if rows := f.dumpAudit(t); strings.Contains(rows, "secret.rotated") {
		t.Errorf("a preview was audited: %s", rows)
	}
}

func TestRotatePreviewUsesTheNameItIsGivenAndFlagsATakenOne(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	got := decode[rotatePreviewAnswer](t, f.rotateCall(t, e, "secrets", "sec-db", "rotate/preview", `{"newName":"dbpw_next"}`))
	if got.NewName != "dbpw_next" || got.NameTaken || got.Users[0].Next == nil || !strings.Contains(string(mustJSON(t, got.Users[0].Next)), "dbpw_next") {
		t.Errorf("preview = %+v, want the typed name used in the next spec", got)
	}
	got = decode[rotatePreviewAnswer](t, f.rotateCall(t, e, "secrets", "sec-db", "rotate/preview", `{"newName":"dbpw"}`))
	if !got.NameTaken {
		t.Errorf("preview = %+v, want the old secret's own name flagged as taken", got)
	}
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate/preview", `{"newName":"-bad"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "name_invalid" {
		t.Errorf("a bad name: %d %s (%s), want 422 name_invalid", rec.code, rec.body, rec.ptyp)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRotateNeedsTheObjectsManagePermissionAndAnOperatorIsForbiddenOnAllFourRoutes(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	for _, role := range []string{"viewer", "operator"} {
		who := f.caller(t, e, role+"-1", role)
		for _, tc := range []struct{ kind, id, tail, body string }{
			{"secrets", "sec-db", "rotate/preview", `{}`},
			{"secrets", "sec-db", "rotate", rotateBody(rotateValue, "")},
			{"configs", "cfg-app", "rotate/preview", `{}`},
			{"configs", "cfg-app", "rotate", rotateBody("x=1", "")},
		} {
			if rec := who.rotateCall(tc.kind, tc.id, tc.tail, tc.body); rec.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s/%s by a %s: %d, want 403", tc.kind, tc.id, tc.tail, role, rec.StatusCode)
			}
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
	if rows := serviceUpdatedRows(f.serviceAudits(t)); len(rows) != 0 {
		t.Errorf("service rows = %+v", rows)
	}
}

func TestRotateASecretMovesTheStandaloneUsersAndAuditsTheRotateAndEachServiceNeverTheValue(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", rotateBody(rotateValue, `,"removeOld":true`))
	if rec.code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	got := decode[rotateAnswer](t, rec)
	if got.ID == "" || got.Name != "dbpw_v2" || got.OldName != "dbpw" || got.Updated != 2 || got.Skipped != 1 || got.OldRemoved || got.RemoveCode != "object_in_use" {
		t.Fatalf("answer = %+v", got)
	}
	var states []string
	for _, s := range got.Services {
		states = append(states, s.Service+":"+s.State)
	}
	if !slices.Equal(states, []string{"api:updated", "shop_web:skipped", "worker:updated"}) {
		t.Errorf("services = %v", states)
	}
	creates := f.fake.SecretCreates()
	if len(creates) != 1 || creates[0].Name != "dbpw_v2" || creates[0].Labels["app"] != "db" {
		t.Fatalf("creates = %+v", creates)
	}
	ups := f.fake.ServiceUpdates()
	if len(ups) != 2 || ups[0].ServiceID != "svc-api" || ups[1].ServiceID != "svc-worker" {
		t.Fatalf("updates = %+v, want api then worker", ups)
	}
	ref := ups[1].Spec.TaskTemplate.ContainerSpec.Secrets[0]
	if ref.SecretID != got.ID || ref.SecretName != "dbpw_v2" || ref.File.Name != "/run/secrets/dbpw" || ref.File.UID != "70" || ref.File.GID != "71" || ref.File.Mode != 0o400 {
		t.Errorf("worker's reference = %+v file %+v", ref, ref.File)
	}
	if w := writeVerbs(f.fake.Writes()); !slices.Equal(w, []string{"POST /secrets", "POST /services", "POST /services"}) {
		t.Errorf("writes = %v, want no delete while a stack still uses the secret", w)
	}

	rows := f.secretAudits(t)
	var rotated *auditRowFull
	for i := range rows {
		if rows[i].Action == audit.ActionSecretRotated {
			rotated = &rows[i]
		}
	}
	if rotated == nil || rotated.Result != string(audit.ResultSuccess) || rotated.TargetID != "sec-db" || rotated.EnvID != e.ID {
		t.Fatalf("secret audit rows = %+v, want a secret.rotated success about the old secret", rows)
	}
	for k, want := range map[string]string{"from": "dbpw", "to": "dbpw_v2", "updated": "2", "skipped": "1", "removed_old": "false", "code": "done"} {
		if rotated.Details[k] != want {
			t.Errorf("rotated detail %s = %q, want %q (%+v)", k, rotated.Details[k], want, rotated.Details)
		}
	}
	if len(rows) != 2 || rows[0].Action != audit.ActionSecretCreated || rows[0].Details["source"] != "rotate" {
		t.Errorf("secret audit rows = %+v, want the new secret created with source rotate, then the rotate", rows)
	}
	svc := serviceUpdatedRows(f.serviceAudits(t))
	if len(svc) != 2 || svc[0].Details["name"] != "api" || svc[1].Details["name"] != "worker" || svc[0].Result != string(audit.ResultSuccess) || svc[0].Details["code"] != "done" {
		t.Errorf("service rows = %+v, want one success each for api and worker", svc)
	}
	for what, h := range map[string]string{"the answer": string(rec.body), "the audit": f.dumpAudit(t), "the log": f.logs.String()} {
		if strings.Contains(h, rotateValue) || strings.Contains(h, b64(rotateValue)) {
			t.Errorf("%s holds the value", what)
		}
	}
}

func TestRotateRemovesTheOldSecretWhenNothingIsLeftUsingIt(t *testing.T) {
	f, e := rotateEnv(t, rotateServices(func(fs *dockertest.FakeService) {
		if fs.Name == "shop_web" {
			fs.Secrets = nil
		}
	})...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", rotateBody(rotateValue, `,"removeOld":true`))
	if rec.code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	got := decode[rotateAnswer](t, rec)
	if !got.OldRemoved || got.RemoveCode != "" {
		t.Errorf("answer = %+v, want the old secret removed", got)
	}
	if w := writeVerbs(f.fake.Writes()); w[len(w)-1] != "DELETE /secrets" {
		t.Errorf("writes = %v, want the old secret deleted last", w)
	}
	var kinds []string
	for _, r := range f.secretAudits(t) {
		kinds = append(kinds, r.Action)
	}
	if !slices.Equal(kinds, []string{audit.ActionSecretCreated, audit.ActionSecretRemoved, audit.ActionSecretRotated}) {
		t.Errorf("secret audit rows = %v", kinds)
	}
	for _, r := range f.secretAudits(t) {
		if r.Action == audit.ActionSecretRotated && r.Details["removed_old"] != "true" {
			t.Errorf("rotated row = %+v, want removed_old true", r)
		}
	}
}

func TestRotateWithoutRemoveOldKeepsTheOldSecret(t *testing.T) {
	f, e := rotateEnv(t, rotateServices(func(fs *dockertest.FakeService) {
		if fs.Name == "shop_web" {
			fs.Secrets = nil
		}
	})...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", rotateBody(rotateValue, `,"removeOld":false`))
	if got := decode[rotateAnswer](t, rec); rec.code != http.StatusOK || got.OldRemoved || got.RemoveCode != "" {
		t.Fatalf("rotate = %d %+v", rec.code, got)
	}
	if w := writeVerbs(f.fake.Writes()); slices.Contains(w, "DELETE /secrets") {
		t.Errorf("writes = %v", w)
	}
}

func TestRotateWithNoUsersSaysSoAndOnlyCreatesTheNewSecret(t *testing.T) {
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) {
		c.Secrets = []dockertest.FakeSecret{{ID: "sec-spare", Name: "spare"}}
	}, editableService(nil))
	rec := f.rotateCall(t, e, "secrets", "sec-spare", "rotate", rotateBody(rotateValue, `,"removeOld":true`))
	got := decode[rotateAnswer](t, rec)
	if rec.code != http.StatusOK || got.Name != "spare_v2" || len(got.Services) != 0 || got.OldRemoved {
		t.Fatalf("rotate = %d %+v, want only spare_v2 created", rec.code, got)
	}
	if w := writeVerbs(f.fake.Writes()); !slices.Equal(w, []string{"POST /secrets"}) {
		t.Errorf("writes = %v", w)
	}
}

func TestRotateRefusesBadInputBeforeAnythingIsSent(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	huge := b64(strings.Repeat("x", 512000))
	for name, tc := range map[string]struct {
		body, want string
	}{
		"no value":      {`{}`, "value_required"},
		"empty value":   {`{"data":""}`, "value_required"},
		"not base64":    {`{"data":"***"}`, "value_invalid"},
		"too large":     {`{"data":"` + huge + `"}`, "value_too_large"},
		"bad name":      {rotateBody("v", `,"name":"-bad"`), "name_invalid"},
		"too long name": {rotateBody("v", `,"name":"`+strings.Repeat("a", 65)+`"`), "name_invalid"},
	} {
		rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.want {
			t.Errorf("%s: %d %s (%s), want 422 %s", name, rec.code, rec.body, rec.ptyp, tc.want)
		}
	}
	if rec := f.rotateCall(t, e, "configs", "cfg-app", "rotate", rotateBody("x", `,"templating":"jinja"`)); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "templating_invalid" {
		t.Errorf("templating: %d %s (%s), want 422 templating_invalid", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestRotateWhoseNewNameIsTakenSaysNothingElseChangedAndAuditsTheFailure(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", rotateBody(rotateValue, `,"name":"dbpw"`))
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("rotate: %d %s (%s), want 409 name_taken", rec.code, rec.body, rec.ptyp)
	}
	p := decode[rotateProblem](t, rec)
	if !strings.HasPrefix(p.Detail, "Couldn't create dbpw.") || !strings.HasSuffix(p.Detail, "Nothing else changed.") {
		t.Errorf("detail = %q, want Couldn't create dbpw. … Nothing else changed.", p.Detail)
	}
	if w := writeVerbs(f.fake.Writes()); !slices.Equal(w, []string{"POST /secrets"}) {
		t.Errorf("writes = %v, want the refused create only", w)
	}
	var rotated []auditRowFull
	for _, r := range f.secretAudits(t) {
		if r.Action == audit.ActionSecretRotated {
			rotated = append(rotated, r)
		}
	}
	if len(rotated) != 1 || rotated[0].Result != string(audit.ResultFailure) || rotated[0].Reason != "name_taken" || rotated[0].Details["updated"] != "0" {
		t.Errorf("rotated rows = %+v, want one failure for name_taken", rotated)
	}
	if rows := serviceUpdatedRows(f.serviceAudits(t)); len(rows) != 0 {
		t.Errorf("service rows = %+v, want none", rows)
	}
}

func TestRotateThatStopsPartWayKeepsTheOldSecretNamesWhatMovedAndAuditsBothSides(t *testing.T) {
	f, e := rotateEnv(t, rotateServices(func(fs *dockertest.FakeService) {
		if fs.Name == "worker" {
			fs.UpdateError = "rpc error: code = InvalidArgument desc = bad secret reference"
		}
	})...)
	rec := f.rotateCall(t, e, "secrets", "sec-db", "rotate", rotateBody(rotateValue, `,"removeOld":true`))
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "spec_rejected" {
		t.Fatalf("rotate: %d %s (%s), want 422 spec_rejected", rec.code, rec.body, rec.ptyp)
	}
	p := decode[rotateProblem](t, rec)
	if !slices.Equal(p.at("updated"), []string{"api"}) || !slices.Equal(p.at("failedAt"), []string{"worker"}) || !slices.Equal(p.at("created"), []string{"dbpw_v2"}) ||
		len(p.at("reason")) != 1 || !slices.Equal(p.at("skipped"), []string{"shop_web"}) {
		t.Errorf("errors = %+v, want updated api, failedAt worker, created dbpw_v2, skipped shop_web and a reason", p.Errors)
	}
	if !strings.HasPrefix(p.Detail, "Updated 1 of 2 services. dbpw stays until the rest use dbpw_v2.") {
		t.Errorf("detail = %q", p.Detail)
	}
	if w := writeVerbs(f.fake.Writes()); slices.Contains(w, "DELETE /secrets") {
		t.Errorf("writes = %v, want the old secret kept", w)
	}
	var rotated []auditRowFull
	for _, r := range f.secretAudits(t) {
		if r.Action == audit.ActionSecretRotated {
			rotated = append(rotated, r)
		}
	}
	if len(rotated) != 1 || rotated[0].Result != string(audit.ResultFailure) || rotated[0].Reason != "spec_rejected" || rotated[0].Details["updated"] != "1" {
		t.Errorf("rotated rows = %+v", rotated)
	}
	svc := serviceUpdatedRows(f.serviceAudits(t))
	if len(svc) != 2 || svc[0].Details["name"] != "api" || svc[0].Result != string(audit.ResultSuccess) || svc[1].Details["name"] != "worker" || svc[1].Reason != "spec_rejected" {
		t.Errorf("service rows = %+v, want api done and worker failed with spec_rejected", svc)
	}
	if strings.Contains(string(rec.body), rotateValue) || strings.Contains(f.dumpAudit(t), rotateValue) {
		t.Error("the value is in an answer or an audit row")
	}
}

func TestRotateAConfigCopiesLabelsAndTemplatingAndAuditsConfigRotated(t *testing.T) {
	f, e := rotateEnv(t, rotateServices(func(fs *dockertest.FakeService) {
		if fs.Name == "shop_web" {
			fs.Configs = nil
		}
	})...)
	prev := f.rotateCall(t, e, "configs", "cfg-app", "rotate/preview", `{}`)
	pa := decode[rotatePreviewAnswer](t, prev)
	if prev.code != http.StatusOK || pa.Kind != "config" || pa.Templating != "golang" || pa.NewName != "app.conf_v2" || pa.Labels["app"] != "web" {
		t.Fatalf("preview = %d %+v", prev.code, pa)
	}
	rec := f.rotateCall(t, e, "configs", "cfg-app", "rotate", rotateBody("level=debug", `,"templating":"golang","removeOld":true`))
	if rec.code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.code, rec.body)
	}
	got := decode[rotateAnswer](t, rec)
	if got.Name != "app.conf_v2" || got.Updated != 2 || got.Skipped != 0 || !got.OldRemoved {
		t.Errorf("answer = %+v", got)
	}
	creates := f.fake.ConfigCreates()
	if len(creates) != 1 || creates[0].Templating != "golang" || creates[0].Labels["app"] != "web" || string(creates[0].Data) != "level=debug" {
		t.Errorf("creates = %+v", creates)
	}
	rows := f.auditLike(t, "config.%")
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action)
	}
	if !slices.Equal(actions, []string{audit.ActionConfigCreated, audit.ActionConfigRemoved, audit.ActionConfigRotated}) {
		t.Fatalf("config audit rows = %v", actions)
	}
	if r := rows[2]; r.Details["from"] != "app.conf" || r.Details["to"] != "app.conf_v2" || r.Details["updated"] != "2" || r.Details["removed_old"] != "true" || r.Details["code"] != "done" {
		t.Errorf("rotated row = %+v", r)
	}
}

func TestRotateOfASecretThatIsGoneIsNotFoundAndOfAStandaloneHostIsNotASwarm(t *testing.T) {
	f, e := rotateEnv(t, rotateServices()...)
	for _, tail := range []string{"rotate/preview", "rotate"} {
		rec := f.rotateCall(t, e, "secrets", "sec-gone", tail, rotateBody("v", ""))
		if rec.code != http.StatusNotFound || rec.ptyp != "secret_not_found" {
			t.Errorf("%s of a missing secret: %d %s (%s), want 404 secret_not_found", tail, rec.code, rec.body, rec.ptyp)
		}
	}
	f, e = actionsEnv(t, func(c *dockertest.FakeConfig) { c.SwarmState, c.SwarmControl = "inactive", false })
	for _, tail := range []string{"rotate/preview", "rotate"} {
		rec := f.rotateCall(t, e, "secrets", "x", tail, rotateBody("v", ""))
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("%s on a standalone host: %d %s (%s), want 409 not_a_swarm", tail, rec.code, rec.body, rec.ptyp)
		}
	}
}
