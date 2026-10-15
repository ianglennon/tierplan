// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// Swarm secrets (plan 06-22, RES-08, D-24, D-25, D-27, D-40). Docker is a fake daemon behind
// the real monitor, aggregator and cluster reader; the router, policy, CSRF guard and audit
// writer are the real ones.

type secretItem struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	CreatedAt string            `json:"createdAt"`
	UpdatedAt string            `json:"updatedAt"`
	UsedBy    int               `json:"usedBy"`
	Owner     string            `json:"owner"`
	OwnerKind string            `json:"ownerKind"`
}

type secretList struct {
	Items           []secretItem `json:"items"`
	Total           int          `json:"total"`
	UnfilteredTotal int          `json:"unfilteredTotal"`
	Capped          bool         `json:"capped"`
}

type secretUser struct {
	ServiceID string `json:"serviceId"`
	Service   string `json:"service"`
	Stack     string `json:"stack"`
	File      string `json:"file"`
	UID       string `json:"uid"`
	GID       string `json:"gid"`
	Mode      string `json:"mode"`
}

type secretDetail struct {
	secretItem
	UsedByRows []secretUser `json:"usedByRows"`
}

type secretCreated struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func dbpwSecret() dockertest.FakeSecret {
	return dockertest.FakeSecret{
		ID: "sec-db", Name: "dbpw", Labels: map[string]string{"app": "db", "com.docker.stack.namespace": "shop"},
		CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), UpdatedAt: time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC),
	}
}

func secretUsers() []dockertest.FakeService {
	ref := dockertest.FakeServiceObject{ID: "sec-db", Name: "dbpw", File: "dbpw", UID: "0", GID: "0", Mode: 0o444}
	return []dockertest.FakeService{
		{ID: "svc-b", Name: "bravo", Mode: "replicated", Running: 0, Desired: 0, Image: "b:1", Secrets: []dockertest.FakeServiceObject{ref}},
		{ID: "svc-a", Name: "alpha", Mode: "replicated", Running: 1, Desired: 1, Image: "a:1", Version: 2,
			Labels:  map[string]string{"com.docker.stack.namespace": "shop"},
			Secrets: []dockertest.FakeServiceObject{{ID: "sec-db", Name: "dbpw", File: "/run/x", UID: "70", GID: "71", Mode: 0o400}}},
	}
}

// secretsEnv is a swarm manager environment holding the given secrets and services.
func secretsEnv(t *testing.T, secrets []dockertest.FakeSecret, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	return actionsEnv(t, func(c *dockertest.FakeConfig) { c.Secrets = secrets }, services...)
}

func (f *envFix) createSecret(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/secrets", body)
}

func (f *envFix) deleteSecret(t *testing.T, e envBody, id, query string) *envRec {
	t.Helper()
	path := "/api/env/" + e.ID + "/secrets/" + id
	if query != "" {
		path += "?" + query
	}
	return f.req(t, http.MethodDelete, path, "")
}

func secretBody(name string, value []byte, extra string) string {
	return `{"name":"` + name + `","data":"` + base64.StdEncoding.EncodeToString(value) + `"` + extra + `}`
}

func (f *envFix) secretAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'secret.%' ORDER BY seq`)
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
	return out
}

// dumpAudit is every column of every audit row as one string, to search for a value.
func (f *envFix) dumpAudit(t *testing.T) string {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT * FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var sb strings.Builder
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			sb.Write(v)
			sb.WriteByte(' ')
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestCreateSecretSendsItOnceAndAnswersTheIDAndNameOnly(t *testing.T) {
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret()})
	rec := f.createSecret(t, e, secretBody("apikey", []byte("s3cr3t"), `,"labels":{"team":"core","tier":"1"},"source":"typed"`))
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[secretCreated](t, rec)
	if got.ID == "" || got.Name != "apikey" {
		t.Errorf("created = %+v", got)
	}
	var generic map[string]any
	if err := json.Unmarshal(rec.body, &generic); err != nil || len(generic) != 2 {
		t.Errorf("the answer = %s, want id and name only", rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /secrets/create"}) {
		t.Errorf("writes = %v, want one create", w)
	}
	cs := f.fake.SecretCreates()
	if len(cs) != 1 || cs[0].Name != "apikey" || cs[0].DataBytes != len("s3cr3t") || cs[0].Labels["team"] != "core" || cs[0].Driver != "" || cs[0].Templating != "" {
		t.Errorf("creates = %+v", cs)
	}
	rows := f.secretAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != "secret.created" || r.Result != string(audit.ResultSuccess) || r.TargetType != "secret" || r.TargetID != got.ID || r.EnvID != e.ID {
		t.Errorf("row = %+v", r)
	}
	for k, want := range map[string]string{"name": "apikey", "labels": "2", "size": "6", "source": "typed", "code": "created"} {
		if got, ok := r.Details[k]; !ok || got != want {
			t.Errorf("details[%s] = %q (present %v), want %q", k, got, ok, want)
		}
	}
	// Left out, the source is the API.
	if rec := f.createSecret(t, e, secretBody("other", []byte("v"), "")); rec.code != http.StatusCreated {
		t.Fatalf("second create = %d: %s", rec.code, rec.body)
	}
	if rows := f.secretAudits(t); len(rows) != 2 || rows[1].Details["source"] != "api" {
		t.Errorf("rows = %+v, want a second with source api", rows)
	}
}

func TestCreateSecretValueBoundsAreCheckedBeforeAnythingIsForwarded(t *testing.T) {
	f, e := secretsEnv(t, nil)
	if rec := f.createSecret(t, e, secretBody("big", bytes.Repeat([]byte("x"), 511999), "")); rec.code != http.StatusCreated {
		t.Fatalf("511,999 bytes = %d %.200s, want 201", rec.code, rec.body)
	}
	before := len(f.fake.Writes())
	cases := []struct {
		name, body, wantType string
	}{
		{"512,000 bytes", secretBody("toobig", bytes.Repeat([]byte("x"), 512000), ""), "value_too_large"},
		{"an empty value", `{"name":"empty","data":""}`, "value_required"},
		{"no value", `{"name":"novalue"}`, "value_required"},
		{"a bad name", secretBody("-bad", []byte("v"), ""), "name_invalid"},
		{"a name with a slash", secretBody("a/b", []byte("v"), ""), "name_invalid"},
		{"no name", secretBody("", []byte("v"), ""), "name_invalid"},
		{"a 65 character name", secretBody(strings.Repeat("a", 65), []byte("v"), ""), "name_invalid"},
		{"a value that is not base64", `{"name":"notb64","data":"!!!not base64!!!"}`, "value_invalid"},
		{"a long label value", secretBody("longlabel", []byte("v"), `,"labels":{"k":"`+strings.Repeat("v", 4097)+`"}`), "labels_invalid"},
	}
	for _, tc := range cases {
		rec := f.createSecret(t, e, tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.wantType {
			t.Errorf("%s = %d %.200s (%s), want 422 %s", tc.name, rec.code, rec.body, rec.ptyp, tc.wantType)
		}
	}
	many := map[string]string{}
	for i := range 65 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	lb, _ := json.Marshal(many)
	if rec := f.createSecret(t, e, secretBody("labelled", []byte("v"), `,"labels":`+string(lb))); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("65 labels = %d %.200s, want 422", rec.code, rec.body)
	}
	if after := len(f.fake.Writes()); after != before {
		t.Errorf("writes grew from %d to %d, want nothing forwarded for a refused body", before, after)
	}
	if got := f.fake.SecretCreates(); len(got) != 1 || got[0].DataBytes != 511999 {
		t.Errorf("creates = %+v, want the one 511,999 byte secret", got)
	}
}

func TestCreateSecretOfATakenNameIsConflictAndAuditedAsAFailure(t *testing.T) {
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret()})
	rec := f.createSecret(t, e, secretBody("dbpw", []byte("v"), ""))
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" || !strings.Contains(string(rec.body), "A secret named dbpw already exists.") {
		t.Fatalf("status = %d %s (%s), want 409 name_taken", rec.code, rec.body, rec.ptyp)
	}
	rows := f.secretAudits(t)
	if len(rows) != 1 || rows[0].Action != "secret.created" || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "name_taken" || rows[0].Details["code"] != "name_taken" {
		t.Errorf("rows = %+v, want one failure name_taken", rows)
	}
}

func TestListSecretsCountsUsersFromServiceSpecsAndNamesTheOwner(t *testing.T) {
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret(), {ID: "sec-z", Name: "zeta"}, {ID: "sec-a", Name: "alpha"}}, secretUsers()...)
	rec := f.get(t, e, "secrets")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	list := decode[secretList](t, rec)
	var names []string
	for _, it := range list.Items {
		names = append(names, it.Name)
	}
	if !slices.Equal(names, []string{"alpha", "dbpw", "zeta"}) || list.Total != 3 || list.UnfilteredTotal != 3 || list.Capped {
		t.Fatalf("list = %+v", list)
	}
	if rec := f.get(t, e, "secrets?q="+strings.Repeat("a", 129)); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "query_too_long" {
		t.Errorf("a 129 character search = %d (%s), want 422 query_too_long", rec.code, rec.ptyp)
	}
	by := map[string]secretItem{}
	for _, it := range list.Items {
		by[it.Name] = it
	}
	db := by["dbpw"]
	// A service scaled to zero still uses the secret (D-27).
	if db.UsedBy != 2 || db.Owner != "shop" || db.OwnerKind != "stack" || db.Labels["app"] != "db" || db.ID != "sec-db" ||
		db.CreatedAt != "2026-03-04T05:06:07Z" || db.UpdatedAt != "2026-03-05T05:06:07Z" {
		t.Errorf("dbpw = %+v", db)
	}
	if z := by["zeta"]; z.UsedBy != 0 || z.Owner != "" || z.OwnerKind != "" || z.Labels == nil {
		t.Errorf("zeta = %+v", z)
	}
	if rec := f.get(t, e, "secrets?q=DB"); rec.code != http.StatusOK {
		t.Fatalf("search = %d", rec.code)
	} else if l := decode[secretList](t, rec); len(l.Items) != 1 || l.Items[0].Name != "dbpw" || l.Total != 1 || l.UnfilteredTotal != 3 {
		t.Errorf("search list = %+v", l)
	}
}

func TestGetSecretAnswersMetadataAndWhoUsesItAndInspectHasNoData(t *testing.T) {
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret()}, secretUsers()...)
	rec := f.get(t, e, "secrets/sec-db")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	d := decode[secretDetail](t, rec)
	if d.ID != "sec-db" || d.Name != "dbpw" || d.UsedBy != 2 || d.Owner != "shop" {
		t.Errorf("detail = %+v", d)
	}
	want := []secretUser{
		{ServiceID: "svc-a", Service: "alpha", Stack: "shop", File: "/run/x", UID: "70", GID: "71", Mode: "0400"},
		{ServiceID: "svc-b", Service: "bravo", File: "dbpw", UID: "0", GID: "0", Mode: "0444"},
	}
	if !slices.Equal(d.UsedByRows, want) {
		t.Errorf("usedByRows = %+v, want %+v", d.UsedByRows, want)
	}
	if byName := f.get(t, e, "secrets/dbpw"); byName.code != http.StatusOK {
		t.Errorf("by name = %d: %s", byName.code, byName.body)
	}
	if rec := f.get(t, e, "secrets/ghost"); rec.code != http.StatusNotFound || rec.ptyp != "secret_not_found" {
		t.Errorf("unknown = %d %s (%s), want 404 secret_not_found", rec.code, rec.body, rec.ptyp)
	}

	ins := f.get(t, e, "secrets/sec-db/inspect")
	if ins.code != http.StatusOK {
		t.Fatalf("inspect = %d: %s", ins.code, ins.body)
	}
	if !strings.Contains(string(ins.body), `"dbpw"`) || strings.Contains(string(ins.body), `"Data"`) {
		t.Errorf("inspect = %s, want the document with no Data field", ins.body)
	}
	if rec := f.get(t, e, "secrets/ghost/inspect"); rec.code != http.StatusNotFound || rec.ptyp != "secret_not_found" {
		t.Errorf("unknown inspect = %d (%s), want 404 secret_not_found", rec.code, rec.ptyp)
	}
}

func TestDeleteSecretNeedsItsNameAndRefusesOneAServiceStillUses(t *testing.T) {
	more := dockertest.FakeSecret{ID: "sec-free", Name: "free"}
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret(), more}, secretUsers()...)

	// No typed name: nothing is sent.
	for _, q := range []string{"", "confirm=wrong"} {
		if rec := f.deleteSecret(t, e, "sec-free", q); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "confirmation_required" {
			t.Errorf("delete with %q = %d %s (%s), want 422 confirmation_required", q, rec.code, rec.body, rec.ptyp)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none before the name is typed", w)
	}

	// In use, even by a service scaled to 0: 409 naming the services.
	rec := f.deleteSecret(t, e, "sec-db", "confirm=dbpw")
	if rec.code != http.StatusConflict || rec.ptyp != "object_in_use" || !strings.Contains(string(rec.body), "alpha") || !strings.Contains(string(rec.body), "bravo") {
		t.Fatalf("delete in use = %d %s (%s), want 409 object_in_use naming both services", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none for a secret in use", w)
	}

	// Unused with its name typed.
	if rec := f.deleteSecret(t, e, "sec-free", "confirm=free"); rec.code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.code, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /secrets/sec-free"}) {
		t.Errorf("writes = %v, want one DELETE", w)
	}

	// Gone: 404 and no audit row.
	if rec := f.deleteSecret(t, e, "sec-free", "confirm=free"); rec.code != http.StatusNotFound || rec.ptyp != "secret_not_found" {
		t.Errorf("second delete = %d %s (%s), want 404 secret_not_found", rec.code, rec.body, rec.ptyp)
	}
	rows := f.secretAudits(t)
	var codes []string
	for _, r := range rows {
		if r.Action != "secret.removed" {
			t.Errorf("row = %+v", r)
		}
		codes = append(codes, r.Result+"/"+r.Details["code"])
	}
	if want := []string{"failure/confirmation_required", "failure/confirmation_required", "failure/object_in_use", "success/done"}; !slices.Equal(codes, want) {
		t.Errorf("audit codes = %v, want %v (a secret that is gone is not audited)", codes, want)
	}
	if last := rows[len(rows)-1]; last.TargetID != "sec-free" || last.Details["name"] != "free" {
		t.Errorf("last row = %+v", last)
	}
}

func TestSecretsNeedTheirPermissionsAndAStandaloneHostHasNone(t *testing.T) {
	f, e := secretsEnv(t, []dockertest.FakeSecret{dbpwSecret()})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "viewer", false)
	call := func(method, path, body string) int {
		return f.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf}).Code
	}
	base := "/api/env/" + e.ID + "/secrets"
	if c := call(http.MethodGet, base, ""); c != http.StatusOK {
		t.Errorf("a viewer's list = %d, want 200", c)
	}
	if c := call(http.MethodGet, base+"/sec-db", ""); c != http.StatusOK {
		t.Errorf("a viewer's detail = %d, want 200", c)
	}
	if c := call(http.MethodPost, base, secretBody("x", []byte("v"), "")); c != http.StatusForbidden {
		t.Errorf("a viewer's create = %d, want 403", c)
	}
	if c := call(http.MethodDelete, base+"/sec-db?confirm=dbpw", ""); c != http.StatusForbidden {
		t.Errorf("a viewer's delete = %d, want 403", c)
	}
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	ocookie, ocsrf := f.signIn(t, "operator", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: base, body: secretBody("x", []byte("v"), ""), cookie: ocookie, csrf: ocsrf}); rec.Code != http.StatusForbidden {
		t.Errorf("an operator's create = %d, want 403: secrets.manage is Admin", rec.Code)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestSecretsOfAStandaloneHostAreNotASwarm(t *testing.T) {
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) { c.SwarmState, c.SwarmControl = "inactive", false })
	for _, rec := range []*envRec{
		f.get(t, e, "secrets"),
		f.createSecret(t, e, secretBody("x", []byte("v"), "")),
		f.deleteSecret(t, e, "x", "confirm=x"),
	} {
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("standalone = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
		}
	}
}

func TestSecretValueNeverLeaves(t *testing.T) {
	const planted = "PLANTED-0xDEADBEEF-value"
	b64 := base64.StdEncoding.EncodeToString([]byte(planted))
	f, e := secretsEnv(t, nil, secretUsers()...)
	var seen []string
	collect := func(label string, rec *envRec) {
		seen = append(seen, label+": "+string(rec.body))
	}
	rec := f.createSecret(t, e, secretBody("leaktest", []byte(planted), `,"labels":{"a":"b"},"source":"typed"`))
	if rec.code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.code, rec.body)
	}
	collect("create", rec)
	id := decode[secretCreated](t, rec).ID
	collect("list", f.get(t, e, "secrets"))
	collect("detail", f.get(t, e, "secrets/"+id))
	collect("inspect", f.get(t, e, "secrets/"+id+"/inspect"))
	// A refused create (name taken) must not echo it either, nor an audited failure.
	collect("taken", f.createSecret(t, e, secretBody("leaktest", []byte(planted), "")))
	collect("deleted", f.deleteSecret(t, e, id, "confirm=leaktest"))
	// And a bad body that carries it.
	collect("badname", f.createSecret(t, e, secretBody("-bad", []byte(planted), "")))
	collect("toobig", f.createSecret(t, e, secretBody("big", append([]byte(planted), bytes.Repeat([]byte("x"), 512000)...), "")))

	haystacks := map[string]string{
		"a response": strings.Join(seen, "\n"),
		"the audit":  f.dumpAudit(t),
		"the log":    f.logs.String(),
	}
	for name, h := range haystacks {
		if strings.Contains(h, planted) || strings.Contains(h, b64) || strings.Contains(h, b64[:24]) {
			t.Errorf("%s holds the secret value", name)
		}
	}
	// The fake kept nothing but a length.
	for _, c := range f.fake.SecretCreates() {
		if c.DataBytes == 0 {
			t.Errorf("create %+v has no length", c)
		}
	}
}
