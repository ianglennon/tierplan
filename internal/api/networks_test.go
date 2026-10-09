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

// Network create (plan 04-12, RES-06, D-39, D-40, D-51, P2 D-42). Docker is a fake
// daemon behind the real monitor, snapshot cache and aggregator; the router, policy,
// CSRF guard and audit writer are the real ones.

func networkEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: []dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Subnets: []string{"172.17.0.0/16"}},
		},
	}
}

type networkCreated struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// createNet posts a create as the signed-in admin. A write goes only to a node the
// monitor has seen online, so it waits for the first probe like act and remove do.
func (f *envFix) createNet(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks", body)
}

func netJSON(fields ...string) string { return "{" + strings.Join(fields, ",") + "}" }

func (f *envFix) networkAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'network.%' ORDER BY seq`)
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

func TestCreateNetworkAsAnEnvAdminAnswers201SendsTheFieldsOnceAndAuditsOnce(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	f.addUser(t, "netadmin", false)
	f.addEnvRole(t, "netadmin", e.ID, "admin")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	cookie, csrf := f.signIn(t, "netadmin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks",
		body:   `{"name":"app-net","driver":"bridge","labels":{"team":"a"}}`,
		cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got networkCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID == "" || got.Name != "app-net" {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /networks/create"}) {
		t.Fatalf("writes = %v, want one create", w)
	}
	creates := f.fake.NetworkCreates()
	if len(creates) != 1 || creates[0].Name != "app-net" || creates[0].Driver != "bridge" || creates[0].Labels["team"] != "a" {
		t.Fatalf("the engine saw %+v", creates)
	}

	rows := f.networkAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNetworkCreated || r.Result != string(audit.ResultSuccess) || r.TargetType != "network" || r.TargetID != got.ID || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "app-net", "node": e.ID, "driver": "bridge", "scope": "local", "code": "created"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}

	// The list reads the network the write made.
	list := decode[listBody[networkItem]](t, f.get(t, e, "networks"))
	if !slices.ContainsFunc(list.Items, func(n networkItem) bool { return n.Name == "app-net" && n.Driver == "bridge" }) {
		t.Fatalf("networks after the create = %+v", list.Items)
	}
}

func TestCreateNetworkTheSameNameAgainIs409NameTakenAndTheFailureIsAudited(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	if rec := f.createNet(t, e, `{"name":"app-net","driver":"bridge"}`); rec.code != http.StatusCreated {
		t.Fatalf("first create: %d %s", rec.code, rec.body)
	}
	rec := f.createNet(t, e, `{"name":"app-net","driver":"bridge"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" {
		t.Fatalf("second create: %d %q, want 409 name_taken: %s", rec.code, rec.ptyp, rec.body)
	}
	rows := f.networkAudits(t)
	if len(rows) != 2 || rows[1].Result != string(audit.ResultFailure) || rows[1].Reason != "name_taken" || rows[1].Details["code"] != "name_taken" {
		t.Fatalf("audit rows = %+v, want a success then a failure named name_taken", rows)
	}
}

func TestCreateNetworkTheSchemaRefusesBadNamesAndDriversBeforeAnyDockerCall(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for name, body := range map[string]string{
		"a name with a space":         `{"name":"my net","driver":"bridge"}`,
		"an empty name":               `{"name":"","driver":"bridge"}`,
		"a name starting with a dash": `{"name":"-net","driver":"bridge"}`,
		"a name of 129 characters":    fmt.Sprintf(`{"name":%q,"driver":"bridge"}`, strings.Repeat("a", 129)),
		"the host driver":             `{"name":"n","driver":"host"}`,
		"the null driver":             `{"name":"n","driver":"null"}`,
		"a third party driver":        `{"name":"n","driver":"weave"}`,
		"no driver":                   `{"name":"n"}`,
		"an ingress flag":             `{"name":"n","driver":"overlay","ingress":true}`,
		"a config-only flag":          `{"name":"n","driver":"bridge","configOnly":true}`,
		"a config-from reference":     `{"name":"n","driver":"bridge","configFrom":"base"}`,
		"more than 64 labels":         fmt.Sprintf(`{"name":"n","driver":"bridge","labels":%s}`, manyEntries(65)),
		"more than 64 driver options": fmt.Sprintf(`{"name":"n","driver":"bridge","options":%s}`, manyEntries(65)),
	} {
		rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/networks", body)
		if rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422: %s", name, rec.code, rec.body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused body reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("a refused body was audited as an attempt: %+v", rows)
	}
}

func manyEntries(n int) string {
	parts := make([]string, 0, n)
	for i := range n {
		parts = append(parts, fmt.Sprintf(`"k%d":"v"`, i))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func TestCreateNetworkAnOperatorAndAViewerAreRefusedAndNothingIsSent(t *testing.T) {
	f, e := dashEnv(t, networkEngine())
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })

	for _, who := range []string{"viewer", "operator"} {
		cookie, csrf := f.signIn(t, who, false)
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/networks",
			body: `{"name":"app-net","driver":"bridge"}`, cookie: cookie, csrf: csrf})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403: %s", who, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.networkAudits(t); len(rows) != 0 {
		t.Fatalf("network audit rows = %+v, want none", rows)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/networks" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestCreateNetworkAnUnknownEnvironmentIs404(t *testing.T) {
	f, _ := dashEnv(t, networkEngine())
	rec := f.req(t, http.MethodPost, "/api/env/nope/networks", `{"name":"n","driver":"bridge"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("%d %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}
