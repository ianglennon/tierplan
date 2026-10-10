// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// Volume delete (plan 04-17, RES-07, D-45, D-46, P2 D-42). The holder of volumes.delete is
// an Operator with the dangerous extra.

type volumeDeleted struct {
	BulkID  string `json:"bulkId"`
	Results []struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
		Result string `json:"result"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"results"`
}

// volumeDeleter makes a user who holds volumes.delete on the environment and returns the
// way to post a delete as them. It waits for the monitor's first probe like createVol does.
func (f *envFix) volumeDeleter(t *testing.T, e envBody, user string) func(body string) *httptest.ResponseRecorder {
	t.Helper()
	f.addUser(t, user, false)
	f.addEnvRole(t, user, e.ID, "operator")
	if _, err := f.st.Write.Exec(`INSERT INTO assignment_extras (assignment_id, permission) VALUES (?, 'volumes.delete')`, "ra-"+user+"-"+e.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, user, false)
	return func(body string) *httptest.ResponseRecorder {
		return f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/volumes/delete", body: body, cookie: cookie, csrf: csrf})
	}
}

func deleteVolumesJSON(confirm string, targets ...[2]string) string {
	var ts []string
	for _, t := range targets {
		ts = append(ts, fmt.Sprintf(`{"nodeId":%q,"name":%q}`, t[0], t[1]))
	}
	return fmt.Sprintf(`{"confirm":%q,"targets":[%s]}`, confirm, strings.Join(ts, ","))
}

func TestDeleteVolumeAHolderOfVolumesDeleteDeletesAnUnusedVolumeWithItsNameTypedAndItIsAudited(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(deleteVolumesJSON("existing", [2]string{e.ID, "existing"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "done" || r.Name != "existing" || r.NodeID != e.ID || got.BulkID != "" {
		t.Fatalf("result = %+v bulk %q, want done and no bulk for one target", r, got.BulkID)
	}
	if w := f.fake.Writes(); !slices.Contains(w, "DELETE /volumes/existing") || slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "force") }) {
		t.Fatalf("writes = %v, want the delete and never a force", w)
	}
	rows := f.volumeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionVolumeRemoved || r.Result != string(audit.ResultSuccess) || r.TargetType != "volume" || r.TargetID != "existing" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "existing", "node": e.ID, "driver": "local", "code": "removed"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}
	list := decode[listBody[volumeItem]](t, f.get(t, e, "volumes"))
	if slices.ContainsFunc(list.Items, func(v volumeItem) bool { return v.Name == "existing" }) {
		t.Fatalf("the volume is still listed: %+v", list.Items)
	}
}

func TestDeleteVolumeAWrongConfirmationIs422ConfirmationRequiredAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	post := f.volumeDeleter(t, e, "trusted")
	for name, body := range map[string]string{
		"no confirmation":      deleteVolumesJSON("", [2]string{e.ID, "existing"}),
		"another name":         deleteVolumesJSON("other", [2]string{e.ID, "existing"}),
		"a different case":     deleteVolumesJSON("Existing", [2]string{e.ID, "existing"}),
		"the phrase for one":   deleteVolumesJSON("delete 1 volume", [2]string{e.ID, "existing"}),
		"the name for several": deleteVolumesJSON("existing", [2]string{e.ID, "existing"}, [2]string{e.ID, "two"}),
		"a number too small":   deleteVolumesJSON("delete 1 volumes", [2]string{e.ID, "existing"}, [2]string{e.ID, "two"}),
	} {
		rec := post(body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "confirmation_required") {
			t.Errorf("%s: %d %s, want 422 confirmation_required", name, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("an unconfirmed request reached the engine: %v", w)
	}
	if rows := f.volumeAudits(t); len(rows) != 0 {
		t.Fatalf("an unconfirmed request was audited as an attempt: %+v", rows)
	}
}

func TestDeleteVolumeWithoutTheDeleteVolumesPermissionIs403AndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	for _, who := range []struct{ user, role string }{{"viewer", "viewer"}, {"operator", "operator"}} {
		f.addUser(t, who.user, false)
		f.addEnvRole(t, who.user, e.ID, who.role)
	}
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for _, who := range []string{"viewer", "operator"} {
		cookie, csrf := f.signIn(t, who, false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/volumes/delete",
			body: deleteVolumesJSON("existing", [2]string{e.ID, "existing"}), cookie: cookie, csrf: csrf})
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
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/volumes/delete" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestDeleteVolumeASecondDeleteOfTheSameVolumeAnswersNotFoundAndIsNotAudited(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	post := f.volumeDeleter(t, e, "trusted")
	body := deleteVolumesJSON("existing", [2]string{e.ID, "existing"})
	if rec := post(body); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := post(body)
	if rec.Code != http.StatusOK {
		t.Fatalf("second: %d %s", rec.Code, rec.Body)
	}
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "not_found" {
		t.Fatalf("second result = %+v, want failed not_found", r)
	}
	if rows := f.volumeAudits(t); len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want only the first delete's", rows)
	}
}

func TestDeleteVolumeAnEmptyTargetListIs422(t *testing.T) {
	f, e := dashEnv(t, volumeEngine())
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(`{"confirm":"delete 0 volumes","targets":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// What uses each volume, and the rule that nothing in use is deleted (plan 04-17 task 2,
// D-45, D-46, T-04-54).

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func usageEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Volumes: []dockertest.FakeVolume{
			{Name: "appdata", Driver: "local", Labels: map[string]string{"com.docker.compose.project": "shop"}},
			{Name: "cache", Driver: "local"},
			{Name: "scratch", Driver: "local", Labels: map[string]string{"owner": "ops", "com.docker.stack.namespace": "mon"}},
			{Name: "labelled-anon", Driver: "local", Labels: map[string]string{"com.docker.volume.anonymous": ""}},
			{Name: hex64, Driver: "local"},
			{Name: "nfs-share", Driver: "nfs"},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c1", Names: []string{"/web"}, State: "running", Mounts: []string{"appdata"}},
			{ID: "c2", Names: []string{"/old-db"}, State: "exited", Mounts: []string{"appdata", "cache", "/srv/bind"}},
		},
	}
}

func listVolumes(t *testing.T, f *envFix, e envBody, query string) listBody[volumeItem] {
	t.Helper()
	path := "volumes"
	if query != "" {
		path += "?" + query
	}
	rec := f.get(t, e, path)
	if rec.code != http.StatusOK {
		t.Fatalf("list %q: %d %s", query, rec.code, rec.body)
	}
	return decode[listBody[volumeItem]](t, rec)
}

func volumeNames(items []volumeItem) []string {
	var out []string
	for _, v := range items {
		out = append(out, v.Name)
	}
	return out
}

func TestVolumeListCarriesWhatUsesEachVolumeThatIsAnonymousAndWhatItGroupsBy(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	got := map[string]volumeItem{}
	for _, v := range listVolumes(t, f, e, "").Items {
		got[v.Name] = v
	}
	if v := got["appdata"]; v.UsedByContainers != 2 || v.UsedByServices != 0 || v.Project != "shop" || v.Stack != "" || v.NodeID != e.ID {
		t.Errorf("appdata = %+v, want 2 containers (a running and a stopped one), project shop", v)
	}
	if v := got["cache"]; v.UsedByContainers != 1 {
		t.Errorf("cache = %+v, want the stopped container to count", v)
	}
	if v := got["scratch"]; v.UsedByContainers != 0 || v.Stack != "mon" {
		t.Errorf("scratch = %+v, want unused, stack mon", v)
	}
	for name, want := range map[string]bool{"appdata": false, "labelled-anon": true, hex64: true, "scratch": false} {
		if got[name].Anonymous != want {
			t.Errorf("%s anonymous = %v, want %v", name, got[name].Anonymous, want)
		}
	}
	// A bind mount of the same text as a volume name is not a use of the volume.
	if _, ok := got["/srv/bind"]; ok {
		t.Error("a bind mount was listed as a volume")
	}
}

func TestVolumeListUsageFilterRunsBeforeTheCapAndSearchMatchesNameDriverLabelAndStack(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	used := volumeNames(listVolumes(t, f, e, "usage=used").Items)
	if !slices.Equal(used, []string{"appdata", "cache"}) {
		t.Errorf("usage=used = %v", used)
	}
	unusedList := listVolumes(t, f, e, "usage=unused")
	if unusedList.Total != 4 || slices.Contains(volumeNames(unusedList.Items), "appdata") {
		t.Errorf("usage=unused = %+v, want the 4 unused ones and a total of 4", unusedList)
	}
	if all := listVolumes(t, f, e, "usage=all"); all.Total != 6 {
		t.Errorf("usage=all total = %d, want 6", all.Total)
	}
	for q, want := range map[string][]string{
		"APPD":    {"appdata"},
		"nfs":     {"nfs-share"}, // name and driver
		"ops":     {"scratch"},   // a label value
		"owner":   {"scratch"},   // a label key
		"mon":     {"scratch"},   // the stack
		"shop":    {"appdata"},   // the project
		"nomatch": nil,
	} {
		if got := volumeNames(listVolumes(t, f, e, "q="+q).Items); !slices.Equal(got, want) {
			t.Errorf("q=%s = %v, want %v", q, got, want)
		}
	}
	// The search and the usage filter combine.
	if got := volumeNames(listVolumes(t, f, e, "q=a&usage=used").Items); !slices.Equal(got, []string{"appdata", "cache"}) {
		t.Errorf("q=a&usage=used = %v", got)
	}
	rec := f.get(t, e, "volumes?usage=bogus")
	if rec.code != http.StatusUnprocessableEntity {
		t.Errorf("usage=bogus: %d, want 422", rec.code)
	}
}

func TestDeleteVolumeAVolumeAStoppedContainerMountsIsVolumeInUseAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(deleteVolumesJSON("cache", [2]string{e.ID, "cache"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	r := got.Results[0]
	if r.Result != "failed" || r.Code != "volume_in_use" || !strings.Contains(r.Detail, "old-db") {
		t.Fatalf("result = %+v, want failed volume_in_use naming old-db", r)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("the daemon was asked to delete a volume in use: %v", w)
	}
	rows := f.volumeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "volume_in_use" || rows[0].Details["code"] != "volume_in_use" || rows[0].Details["driver"] != "local" {
		t.Fatalf("audit rows = %+v, want one failure naming volume_in_use", rows)
	}
}

func TestDeleteVolumeAContainerThatStartsBetweenTheCheckAndTheCallMakesTheEngineRefuseAsVolumeInUse(t *testing.T) {
	cfg := usageEngine()
	cfg.BusyVolumes = []string{"scratch"}
	f, e := dashEnv(t, cfg)
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(deleteVolumesJSON("scratch", [2]string{e.ID, "scratch"}))
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "volume_in_use" {
		t.Fatalf("result = %+v, want failed volume_in_use", r)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /volumes/scratch"}) {
		t.Fatalf("writes = %v, want the one delete the engine refused", w)
	}
	if rows := f.volumeAudits(t); len(rows) != 1 || rows[0].Details["code"] != "volume_in_use" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

func TestDeleteVolumeABulkContinuesPastAFailureAndEveryAuditRowCarriesTheSameBulkID(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(deleteVolumesJSON("delete 4 volumes",
		[2]string{e.ID, "scratch"}, [2]string{e.ID, "appdata"}, [2]string{e.ID, "never-existed"}, [2]string{e.ID, "nfs-share"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 4 || got.BulkID == "" {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	var verdict []string
	for _, r := range got.Results {
		verdict = append(verdict, r.Name+":"+r.Result+":"+r.Code)
	}
	want := []string{"scratch:done:", "appdata:failed:volume_in_use", "never-existed:failed:not_found", "nfs-share:done:"}
	if !slices.Equal(verdict, want) {
		t.Fatalf("results = %v, want %v, in request order", verdict, want)
	}
	rows := f.volumeAudits(t)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %+v, want one per volume that exists (a volume that is not there has nothing to record)", rows)
	}
	for _, r := range rows {
		if r.Action != audit.ActionVolumeRemoved || r.Details["bulk"] != got.BulkID {
			t.Errorf("audit row = %+v, want volume.removed with bulk %s", r, got.BulkID)
		}
	}
	if w := f.fake.Writes(); slices.Contains(w, "DELETE /volumes/appdata") {
		t.Errorf("writes = %v: the volume in use was sent to the engine", w)
	}
}

func TestDeleteVolumeAChunkOfOneThatBelongsToABulkIsConfirmedByThePhraseOfTheWholeSelection(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	post := f.volumeDeleter(t, e, "trusted")
	id := "0198f0a0-0000-7000-8000-000000000001"
	body := fmt.Sprintf(`{"confirm":"delete 26 volumes","bulkId":%q,"targets":[{"nodeId":%q,"name":"scratch"}]}`, id, e.ID)
	rec := post(body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rows := f.volumeAudits(t); len(rows) != 1 || rows[0].Details["bulk"] != id {
		t.Fatalf("audit rows = %+v, want the bulk ID that was sent", rows)
	}
	// The same phrase without a bulk ID does not confirm a lone volume.
	body = fmt.Sprintf(`{"confirm":"delete 26 volumes","targets":[{"nodeId":%q,"name":"nfs-share"}]}`, e.ID)
	if rec := post(body); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a lone volume confirmed by a bulk phrase: %d %s", rec.Code, rec.Body)
	}
}

func TestDeleteVolumeMoreThan25TargetsIs422AndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	post := f.volumeDeleter(t, e, "trusted")
	var targets [][2]string
	for i := range 26 {
		targets = append(targets, [2]string{e.ID, fmt.Sprintf("v%d", i)})
	}
	rec := post(deleteVolumesJSON("delete 26 volumes", targets...))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestDeleteVolumeANodeThatIsNotOneOfTheEnvironmentsIsNodeUnreachableAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, usageEngine())
	post := f.volumeDeleter(t, e, "trusted")
	rec := post(deleteVolumesJSON("cache", [2]string{"not-a-node", "cache"}))
	var got volumeDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "node_unreachable" {
		t.Fatalf("result = %+v, want failed node_unreachable", r)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// On a swarm (plan 04-17 tasks 1 and 2, D-45).

func (s *swarmFix) deleteVolumes(t *testing.T, body string) (*envRec, volumeDeleted) {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/volumes/delete", body)
	var got volumeDeleted
	if rec.code == http.StatusOK {
		if err := json.Unmarshal([]byte(rec.body), &got); err != nil {
			t.Fatalf("body = %s (%v)", rec.body, err)
		}
	}
	return rec, got
}

func (s *swarmFix) volumeDeletesOn() map[string][]string {
	out := map[string][]string{}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		var dels []string
		for _, w := range a.fake.Writes() {
			if strings.HasPrefix(w, "DELETE /volumes/") {
				dels = append(dels, w)
			}
		}
		out[name] = dels
	}
	return out
}

func TestSwarmVolumeListsOneRowPerNodeAndCountsTheServiceSpecsThatMountIt(t *testing.T) {
	s := newSwarmFix(t)
	waitFor(t, "the cluster read", func() bool {
		rec := s.get(t, s.env.ID, "volumes")
		if rec.code != http.StatusOK {
			return false
		}
		items := decode[listBody[volumeItem]](t, rec).Items
		return len(items) == 2 && items[0].UsedByServices == 1
	})
	items := decode[listBody[volumeItem]](t, s.get(t, s.env.ID, "volumes")).Items
	if items[0].Name != "data" || items[1].Name != "data" || items[0].NodeID == items[1].NodeID {
		t.Fatalf("items = %+v, want the same name on two nodes as two rows", items)
	}
	for _, v := range items {
		// The idle service has no running task and its spec mounts the volume.
		if v.UsedByServices != 1 || v.UsedByContainers != 0 {
			t.Errorf("%+v, want 0 containers and 1 service", v)
		}
	}
}

func TestSwarmVolumeAServiceSpecThatMountsTheVolumeKeepsItEvenWithNoTaskAndNothingIsSent(t *testing.T) {
	s := newSwarmFix(t)
	rec, got := s.deleteVolumes(t, deleteVolumesJSON("data", [2]string{s.m1.id, "data"}))
	if rec.code != http.StatusOK || len(got.Results) != 1 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "volume_in_use" || !strings.Contains(r.Detail, "idle") {
		t.Fatalf("result = %+v, want failed volume_in_use naming the idle service", r)
	}
	if d := s.volumeDeletesOn(); len(d["m1"])+len(d["m2"])+len(d["w"]) != 0 {
		t.Fatalf("a delete reached an engine: %v", d)
	}
}

func TestSwarmVolumeDeletingOneNodesCopyLeavesTheSameNameOnTheOtherNode(t *testing.T) {
	s := newSwarmFix(t)
	for _, node := range []string{s.m1.id, s.w.id} {
		if rec := s.postVolume(t, fmt.Sprintf(`{"name":"shared","nodeId":%q}`, node)); rec.code != http.StatusCreated {
			t.Fatalf("create on %s: %d %s", node, rec.code, rec.body)
		}
	}
	waitFor(t, "two rows called shared", func() bool {
		n := 0
		for _, v := range decode[listBody[volumeItem]](t, s.get(t, s.env.ID, "volumes")).Items {
			if v.Name == "shared" {
				n++
			}
		}
		return n == 2
	})
	rec, got := s.deleteVolumes(t, deleteVolumesJSON("shared", [2]string{s.m1.id, "shared"}))
	if rec.code != http.StatusOK || len(got.Results) != 1 || got.Results[0].Result != "done" {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if d := s.volumeDeletesOn(); len(d["m1"]) != 1 || len(d["m2"])+len(d["w"]) != 0 {
		t.Fatalf("deletes = %v, want exactly one, on the node named", d)
	}
	waitFor(t, "one row called shared", func() bool {
		var rows []volumeItem
		for _, v := range decode[listBody[volumeItem]](t, s.get(t, s.env.ID, "volumes")).Items {
			if v.Name == "shared" {
				rows = append(rows, v)
			}
		}
		return len(rows) == 1 && rows[0].NodeID == s.w.id
	})
}
