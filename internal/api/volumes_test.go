// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Volume create (plan 04-16, RES-07, D-44, D-51, P2 D-42). Docker is a fake daemon
// behind the real monitor, snapshot cache and aggregator; the router, policy, CSRF
// guard and audit writer are the real ones.

func volumeEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Volumes: []dockertest.FakeVolume{{Name: "existing", Driver: "local"}},
	}
}

type volumeCreated struct {
	Name   string `json:"name"`
	NodeID string `json:"nodeId"`
}

// createVol posts a create as the signed-in admin. A write goes only to a node the
// monitor has seen online, so it waits for the first probe like act and remove do.
func (f *envFix) createVol(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/volumes", body)
}

func (f *envFix) volumeAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'volume.%' ORDER BY seq`)
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

func TestCreateVolumeAsAnEnvAdminAnswers201SendsTheFieldsOnceAndAuditsOnce(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	f.addUser(t, "voladmin", false)
	f.addEnvRole(t, "voladmin", e.ID, "admin")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "voladmin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/volumes",
		body:   `{"name":"data1","driver":"local","labels":{"app":"x"}}`,
		cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got volumeCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Name != "data1" || got.NodeID != e.ID {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /volumes/create"}) {
		t.Fatalf("writes = %v, want one create", w)
	}
	creates := f.fake.VolumeCreates()
	if len(creates) != 1 || creates[0].Name != "data1" || creates[0].Driver != "local" || creates[0].Labels["app"] != "x" {
		t.Fatalf("the engine saw %+v", creates)
	}

	rows := f.volumeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionVolumeCreated || r.Result != string(audit.ResultSuccess) || r.TargetType != "volume" || r.TargetID != "data1" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "data1", "node": e.ID, "driver": "local", "code": "created"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}

	// The list reads the volume the write made.
	list := decode[listBody[volumeItem]](t, f.get(t, e, "volumes"))
	if !slices.ContainsFunc(list.Items, func(v volumeItem) bool { return v.Name == "data1" && v.Driver == "local" }) {
		t.Fatalf("volumes after the create = %+v", list.Items)
	}
}

func TestCreateVolumeANameTheNodeHasIs409NameTakenWithoutTellingTheEngineToCreateAndIsAudited(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	rec := f.createVol(t, e, `{"name":"existing"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("existing name: %d %q, want 409 name_taken: %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.createVol(t, e, `{"name":"data1"}`); rec.code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.code, rec.body)
	}
	rec = f.createVol(t, e, `{"name":"data1"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("second create: %d %q, want 409 name_taken: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := f.fake.VolumeCreates(); len(got) != 1 || got[0].Name != "data1" {
		t.Fatalf("the engine was asked to create %+v, want data1 once and never the taken names", got)
	}
	rows := f.volumeAudits(t)
	if len(rows) != 3 || rows[0].Reason != "name_taken" || rows[1].Result != string(audit.ResultSuccess) || rows[2].Reason != "name_taken" || rows[2].Details["code"] != "name_taken" {
		t.Fatalf("audit rows = %+v, want a failure, a success and a failure named name_taken", rows)
	}
}

func TestCreateVolumeTheSchemaRefusesBadNamesAndTooManyEntriesBeforeAnyDockerCall(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, body := range map[string]string{
		"a name with a space":         `{"name":"my vol"}`,
		"an empty name":               `{"name":""}`,
		"a name starting with a dash": `{"name":"-vol"}`,
		"a name with a slash":         `{"name":"a/b"}`,
		"a name of 256 characters":    fmt.Sprintf(`{"name":%q}`, strings.Repeat("a", 256)),
		"more than 64 labels":         fmt.Sprintf(`{"name":"v","labels":%s}`, manyEntries(65)),
		"more than 64 driver options": fmt.Sprintf(`{"name":"v","driverOptions":%s}`, manyEntries(65)),
	} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/volumes", body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body reached the engine: %v", w)
	}
	if rows := f.volumeAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited as an attempt: %+v", rows)
	}
}

func TestCreateVolumeAnOperatorAndAViewerAreRefusedAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })

	for _, who := range []string{"viewer", "operator"} {
		cookie, csrf := f.signIn(t, who, false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/volumes",
			body: `{"name":"data1"}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403: %s", who, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.volumeAudits(t); len(rows) != 0 {
		t.Fatalf("volume audit rows = %+v, want none", rows)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/volumes" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestCreateVolumeAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, volumeEngine())
	rec := f.req(t, http.MethodPost, "/api/env/nope/volumes", `{"name":"v"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("%d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}

// Where a volume is made on a swarm, and the options that must not reach the audit log
// (plan 04-16 task 2, D-44, T-04-55, P2 D-42).

// volumeCreatesOn lists, per agent, the volume creates its daemon served.
func (s *swarmFix) volumeCreatesOn() map[string][]string {
	out := map[string][]string{}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		var creates []string
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "POST /volumes/create") {
				creates = append(creates, w)
			}
		}
		out[name] = creates
	}
	return out
}

func (s *swarmFix) postVolume(t *testing.T, body string) *envRec {
	t.Helper()
	return s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/volumes", body)
}

func (s *swarmFix) volumeAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := s.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'volume.%' ORDER BY seq`)
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

func TestCreateVolumeOnASwarmNeedsANodeAndGoesToThatNodeOnly(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.postVolume(t, `{"name":"vol-a"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "node_required" {
		t.Fatalf("without a node: %d %q, want 422 node_required: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.volumeCreatesOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a refused create reached an engine: %v", got)
	}
	if rows := s.volumeAudits(t); len(rows) != 0 {
		t.Fatalf("a request refused before any attempt was audited: %+v", rows)
	}

	rec = s.postVolume(t, fmt.Sprintf(`{"name":"vol-a","nodeId":%q}`, s.w.id))
	if rec.code != http.StatusCreated {
		t.Fatalf("with a node: %d %s", rec.code, rec.body)
	}
	got := s.volumeCreatesOn()
	if len(got["w"]) != 1 || len(got["m1"])+len(got["m2"]) != 0 {
		t.Fatalf("creates = %v, want exactly one, on the worker that was named", got)
	}
	audits := s.volumeAudits(t)
	if len(audits) != 1 || audits[0].Details["node"] != s.w.id || audits[0].Details["driver"] != "local" {
		t.Fatalf("audit rows = %+v, want the worker's node ID", audits)
	}
}

func TestCreateVolumeANameIsTakenPerNodeNotAcrossTheSwarm(t *testing.T) {
	s := newSwarmFix(t)
	// Both managers hold a volume called data (the fixture); the worker does not.
	rec := s.postVolume(t, fmt.Sprintf(`{"name":"data","nodeId":%q}`, s.m1.id))
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("on the manager that has it: %d %q, want 409 name_taken: %s", rec.code, rec.ptyp, rec.body)
	}
	rec = s.postVolume(t, fmt.Sprintf(`{"name":"data","nodeId":%q}`, s.w.id))
	if rec.code != http.StatusCreated {
		t.Fatalf("on the worker that lacks it: %d %s", rec.code, rec.body)
	}
	if got := s.volumeCreatesOn(); len(got["m1"]) != 0 || len(got["w"]) != 1 {
		t.Fatalf("creates = %v, want one, on the worker", got)
	}
}

func TestCreateVolumeANodeThatIsNotOneOfTheEnvironmentsIs503AndNothingIsSent(t *testing.T) {
	s := newSwarmFix(t)
	rec := s.postVolume(t, `{"name":"vol-a","nodeId":"not-a-node"}`)
	if rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Fatalf("%d %q, want 503 node_unavailable: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := s.volumeCreatesOn(); len(got["m1"])+len(got["m2"])+len(got["w"]) != 0 {
		t.Fatalf("a create reached an engine: %v", got)
	}
}

func TestCreateVolumeAuditsTheOptionsButNeverAPasswordAndTheEngineGetsThemWhole(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	rec := f.createVol(t, e, `{"name":"media","driverOptions":{"type":"cifs","o":"addr=nas.lan,username=bob,password=hunter2,vers=3.0","device":"//nas.lan/media"},"labels":{"app":"x"}}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	// The engine is given the real options, or the share could not be mounted.
	creates := f.fake.VolumeCreates()
	if len(creates) != 1 || creates[0].DriverOpts["o"] != "addr=nas.lan,username=bob,password=hunter2,vers=3.0" ||
		creates[0].DriverOpts["device"] != "//nas.lan/media" || creates[0].DriverOpts["type"] != "cifs" {
		t.Fatalf("the engine saw %+v", creates)
	}

	rows := f.volumeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	want := "device=//nas.lan/media; o=addr=nas.lan,username=bob,password=<hidden>,vers=3.0; type=cifs"
	if got := rows[0].Details["options"]; got != want {
		t.Errorf("options detail = %q, want %q", got, want)
	}
	// Nowhere in the stored row, in any column, is the password.
	var stored string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(details, '') || COALESCE(reason, '') || target_id FROM audit_log WHERE action = 'volume.created'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "hunter2") {
		t.Fatalf("the password is in the audit log: %s", stored)
	}
	// Nor is it in the answer.
	if strings.Contains(string(rec.body), "hunter2") {
		t.Fatalf("the password is in the response: %s", rec.body)
	}
}

func TestCreateVolumeAFailedAttemptIsAuditedWithMaskedOptionsToo(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	rec := f.createVol(t, e, `{"name":"existing","driverOptions":{"type":"cifs","o":"password=hunter2"}}`)
	if rec.code != http.StatusConflict {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	rows := f.volumeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) {
		t.Fatalf("audit rows = %+v, want one failure", rows)
	}
	if got := rows[0].Details["options"]; got != "o=password=<hidden>; type=cifs" {
		t.Errorf("options detail = %q", got)
	}
}

func TestCreateVolumeWithNoOptionsRecordsNoOptionsDetail(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	if rec := f.createVol(t, e, `{"name":"plain"}`); rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	rows := f.volumeAudits(t)
	if _, has := rows[0].Details["options"]; len(rows) != 1 || has {
		t.Fatalf("audit rows = %+v, want no options key", rows)
	}
}
