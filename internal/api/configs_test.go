// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// Swarm configs (plan 06-24, RES-08, D-24, D-25, D-27, D-40). Docker is a fake daemon behind the
// real monitor, aggregator and cluster reader; the router, policy, CSRF guard and audit writer
// are the real ones. A config is not a secret: its content is readable on the detail route.

type configItem struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	CreatedAt  string            `json:"createdAt"`
	UpdatedAt  string            `json:"updatedAt"`
	Templating string            `json:"templating"`
	UsedBy     int               `json:"usedBy"`
	Owner      string            `json:"owner"`
	OwnerKind  string            `json:"ownerKind"`
}

type configList struct {
	Items           []configItem `json:"items"`
	Total           int          `json:"total"`
	UnfilteredTotal int          `json:"unfilteredTotal"`
	Capped          bool         `json:"capped"`
}

type configUser struct {
	ServiceID string `json:"serviceId"`
	Service   string `json:"service"`
	Stack     string `json:"stack"`
	File      string `json:"file"`
	UID       string `json:"uid"`
	GID       string `json:"gid"`
	Mode      string `json:"mode"`
}

type configDetail struct {
	configItem
	Size       int          `json:"size"`
	Data       string       `json:"data"`
	Text       *string      `json:"text"`
	UsedByRows []configUser `json:"usedByRows"`
}

type configCreated struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const nginxContent = "worker_processes 1;\nserver_name {{.Service.Name}};\n"

func nginxConfig() dockertest.FakeConfigObject {
	return dockertest.FakeConfigObject{
		ID: "cfg-nginx", Name: "nginx_conf", Labels: map[string]string{"app": "web", "com.docker.stack.namespace": "shop"},
		Data: []byte(nginxContent), Templating: "golang",
		CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), UpdatedAt: time.Date(2026, 3, 5, 5, 6, 7, 0, time.UTC),
	}
}

func configUsers() []dockertest.FakeService {
	return []dockertest.FakeService{
		{ID: "svc-b", Name: "bravo", Mode: "replicated", Running: 0, Desired: 0, Image: "b:1",
			Configs: []dockertest.FakeServiceObject{{ID: "cfg-nginx", Name: "nginx_conf", File: "/etc/nginx/nginx.conf", UID: "0", GID: "0", Mode: 0o444}}},
		{ID: "svc-a", Name: "alpha", Mode: "replicated", Running: 1, Desired: 1, Image: "a:1", Version: 2,
			Labels:  map[string]string{"com.docker.stack.namespace": "shop"},
			Configs: []dockertest.FakeServiceObject{{ID: "cfg-nginx", Name: "nginx_conf"}}},
	}
}

// configsEnv is a swarm manager environment holding the given configs and services.
func configsEnv(t *testing.T, configs []dockertest.FakeConfigObject, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	return actionsEnv(t, func(c *dockertest.FakeConfig) { c.Configs = configs }, services...)
}

func (f *envFix) createConfig(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/configs", body)
}

func (f *envFix) deleteConfig(t *testing.T, e envBody, id string) *envRec {
	t.Helper()
	return f.req(t, http.MethodDelete, "/api/env/"+e.ID+"/configs/"+id, "")
}

func configBody(name string, content []byte, extra string) string {
	return `{"name":"` + name + `","data":"` + base64.StdEncoding.EncodeToString(content) + `"` + extra + `}`
}

func (f *envFix) configAudits(t *testing.T) []auditRowFull {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action LIKE 'config.%' ORDER BY seq`)
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

func TestCreateConfigSendsItOnceAndAnswersTheIDAndNameOnly(t *testing.T) {
	t.Parallel()
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig()})
	rec := f.createConfig(t, e, configBody("app.env", []byte("A=1\n"), `,"labels":{"team":"core","tier":"1"},"templating":"golang","source":"file"`))
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[configCreated](t, rec)
	if got.ID == "" || got.Name != "app.env" {
		t.Errorf("created = %+v", got)
	}
	var generic map[string]any
	if err := json.Unmarshal(rec.body, &generic); err != nil || len(generic) != 2 {
		t.Errorf("the answer = %s, want id and name only", rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"POST /configs/create"}) {
		t.Errorf("writes = %v, want one create", w)
	}
	cs := f.fake.ConfigCreates()
	if len(cs) != 1 || cs[0].Name != "app.env" || string(cs[0].Data) != "A=1\n" || cs[0].Labels["team"] != "core" || cs[0].Templating != "golang" {
		t.Errorf("creates = %+v", cs)
	}
	rows := f.configAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != "config.created" || r.Result != string(audit.ResultSuccess) || r.TargetType != "config" || r.TargetID != got.ID || r.EnvID != e.ID {
		t.Errorf("row = %+v", r)
	}
	for k, want := range map[string]string{"name": "app.env", "labels": "2", "size": "4", "templating": "golang", "source": "file", "code": "created"} {
		if got, ok := r.Details[k]; !ok || got != want {
			t.Errorf("details[%s] = %q (present %v), want %q", k, got, ok, want)
		}
	}
	// Left out, the templating is none and the source is the API; "none" says the same as nothing.
	for i, extra := range []string{``, `,"templating":"none"`, `,"templating":""`} {
		name := "plain" + string(rune('a'+i))
		if rec := f.createConfig(t, e, configBody(name, []byte("v"), extra)); rec.code != http.StatusCreated {
			t.Fatalf("create with %q = %d: %s", extra, rec.code, rec.body)
		}
	}
	rows = f.configAudits(t)
	if len(rows) != 4 || rows[1].Details["source"] != "api" || rows[1].Details["templating"] != "none" || rows[3].Details["templating"] != "none" {
		t.Errorf("rows = %+v, want three more with source api and templating none", rows)
	}
	for _, c := range f.fake.ConfigCreates()[1:] {
		if c.Templating != "" {
			t.Errorf("create %+v sent a templating driver, want none", c)
		}
	}
}

func TestCreateConfigContentBoundsAreCheckedBeforeAnythingIsForwarded(t *testing.T) {
	t.Parallel()
	f, e := configsEnv(t, nil)
	if rec := f.createConfig(t, e, configBody("big", bytes.Repeat([]byte("x"), 1023999), "")); rec.code != http.StatusCreated {
		t.Fatalf("1,023,999 bytes = %d %.200s, want 201", rec.code, rec.body)
	}
	before := len(f.fake.Writes())
	cases := []struct {
		name, body, wantType string
	}{
		{"1,024,000 bytes", configBody("toobig", bytes.Repeat([]byte("x"), 1024000), ""), "value_too_large"},
		{"an empty value", `{"name":"empty","data":""}`, "value_required"},
		{"no value", `{"name":"novalue"}`, "value_required"},
		{"a bad name", configBody("-bad", []byte("v"), ""), "name_invalid"},
		{"a name with a slash", configBody("a/b", []byte("v"), ""), "name_invalid"},
		{"no name", configBody("", []byte("v"), ""), "name_invalid"},
		{"a 65 character name", configBody(strings.Repeat("a", 65), []byte("v"), ""), "name_invalid"},
		{"a value that is not base64", `{"name":"notb64","data":"!!!not base64!!!"}`, "value_invalid"},
		{"another templating driver", configBody("tpl", []byte("v"), `,"templating":"vault"`), "templating_invalid"},
		{"templating in capitals", configBody("tpl", []byte("v"), `,"templating":"Golang"`), "templating_invalid"},
		{"a long label value", configBody("longlabel", []byte("v"), `,"labels":{"k":"`+strings.Repeat("v", 4097)+`"}`), "labels_invalid"},
	}
	for _, tc := range cases {
		rec := f.createConfig(t, e, tc.body)
		if rec.code != http.StatusUnprocessableEntity || rec.ptyp != tc.wantType {
			t.Errorf("%s = %d %.200s (%s), want 422 %s", tc.name, rec.code, rec.body, rec.ptyp, tc.wantType)
		}
	}
	if rec := f.createConfig(t, e, configBody("toobig", bytes.Repeat([]byte("x"), 1024000), "")); !strings.Contains(string(rec.body), "Values can be at most 1000 KB.") {
		t.Errorf("the too-large detail = %s, want the 1000 KB wording", rec.body)
	}
	many := map[string]string{}
	for i := range 65 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	lb, _ := json.Marshal(many)
	if rec := f.createConfig(t, e, configBody("labelled", []byte("v"), `,"labels":`+string(lb))); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("65 labels = %d %.200s, want 422", rec.code, rec.body)
	}
	// A body over the 2 MiB request cap is refused whole.
	if rec := f.createConfig(t, e, `{"name":"huge","data":"`+strings.Repeat("A", 2<<20)+`"}`); rec.code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over 2 MiB = %d, want 413", rec.code)
	}
	if after := len(f.fake.Writes()); after != before {
		t.Errorf("writes grew from %d to %d, want nothing forwarded for a refused body", before, after)
	}
	if got := f.fake.ConfigCreates(); len(got) != 1 || len(got[0].Data) != 1023999 {
		t.Errorf("creates = %d, want the one 1,023,999 byte config", len(got))
	}
}

func TestCreateConfigOfATakenNameIsConflictAndAuditedAsAFailure(t *testing.T) {
	t.Parallel()
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig()})
	rec := f.createConfig(t, e, configBody("nginx_conf", []byte("v"), ""))
	if rec.code != http.StatusConflict || rec.ptyp != "name_taken" || !strings.Contains(string(rec.body), "A config named nginx_conf already exists.") {
		t.Fatalf("status = %d %s (%s), want 409 name_taken", rec.code, rec.body, rec.ptyp)
	}
	rows := f.configAudits(t)
	if len(rows) != 1 || rows[0].Action != "config.created" || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "name_taken" || rows[0].Details["code"] != "name_taken" {
		t.Errorf("rows = %+v, want one failure name_taken", rows)
	}
}

func TestListConfigsCarriesNoContentCountsUsersFromServiceSpecsAndNamesTheOwner(t *testing.T) {
	t.Parallel()
	zeta := dockertest.FakeConfigObject{ID: "cfg-z", Name: "zeta", Data: []byte("ZETA-CONTENT-MARKER")}
	alpha := dockertest.FakeConfigObject{ID: "cfg-a", Name: "alpha", Data: []byte("ALPHA-CONTENT-MARKER")}
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig(), zeta, alpha}, configUsers()...)
	rec := f.get(t, e, "configs")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	// The manager sent every config's content; none of it, raw or base64, reaches the answer.
	for _, content := range []string{nginxContent, "ZETA-CONTENT-MARKER", "ALPHA-CONTENT-MARKER"} {
		if strings.Contains(string(rec.body), content) || strings.Contains(string(rec.body), base64.StdEncoding.EncodeToString([]byte(content))) {
			t.Errorf("the list holds config content %q: %s", content, rec.body)
		}
	}
	var generic struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &generic); err != nil {
		t.Fatal(err)
	}
	for _, it := range generic.Items {
		for _, banned := range []string{"data", "text", "size", "content"} {
			if _, has := it[banned]; has {
				t.Errorf("a list row has the field %q: %v", banned, it)
			}
		}
	}
	list := decode[configList](t, rec)
	var names []string
	for _, it := range list.Items {
		names = append(names, it.Name)
	}
	if !slices.Equal(names, []string{"alpha", "nginx_conf", "zeta"}) || list.Total != 3 || list.UnfilteredTotal != 3 || list.Capped {
		t.Fatalf("list = %+v", list)
	}
	if rec := f.get(t, e, "configs?q="+strings.Repeat("a", 129)); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "query_too_long" {
		t.Errorf("a 129 character search = %d (%s), want 422 query_too_long", rec.code, rec.ptyp)
	}
	by := map[string]configItem{}
	for _, it := range list.Items {
		by[it.Name] = it
	}
	ng := by["nginx_conf"]
	// A service scaled to zero still uses the config (D-27).
	if ng.UsedBy != 2 || ng.Owner != "shop" || ng.OwnerKind != "stack" || ng.Templating != "golang" || ng.Labels["app"] != "web" || ng.ID != "cfg-nginx" ||
		ng.CreatedAt != "2026-03-04T05:06:07Z" || ng.UpdatedAt != "2026-03-05T05:06:07Z" {
		t.Errorf("nginx_conf = %+v", ng)
	}
	if z := by["zeta"]; z.UsedBy != 0 || z.Owner != "" || z.OwnerKind != "" || z.Templating != "none" || z.Labels == nil {
		t.Errorf("zeta = %+v", z)
	}
	if rec := f.get(t, e, "configs?q=NGINX"); rec.code != http.StatusOK {
		t.Fatalf("search = %d", rec.code)
	} else if l := decode[configList](t, rec); len(l.Items) != 1 || l.Items[0].Name != "nginx_conf" || l.Total != 1 || l.UnfilteredTotal != 3 {
		t.Errorf("search list = %+v", l)
	}
}

func TestGetConfigAnswersMetadataTemplatingSizeContentAndWhoUsesIt(t *testing.T) {
	t.Parallel()
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig()}, configUsers()...)
	rec := f.get(t, e, "configs/cfg-nginx")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	d := decode[configDetail](t, rec)
	if d.ID != "cfg-nginx" || d.Name != "nginx_conf" || d.Templating != "golang" || d.UsedBy != 2 || d.Owner != "shop" || d.Size != len(nginxContent) {
		t.Errorf("detail = %+v", d)
	}
	if d.Data != base64.StdEncoding.EncodeToString([]byte(nginxContent)) || d.Text == nil || *d.Text != nginxContent {
		t.Errorf("content = data %q text %v, want the base64 and the UTF-8 text", d.Data, d.Text)
	}
	want := []configUser{
		{ServiceID: "svc-a", Service: "alpha", Stack: "shop"},
		{ServiceID: "svc-b", Service: "bravo", File: "/etc/nginx/nginx.conf", UID: "0", GID: "0", Mode: "0444"},
	}
	if !slices.Equal(d.UsedByRows, want) {
		t.Errorf("usedByRows = %+v, want %+v", d.UsedByRows, want)
	}
	if byName := f.get(t, e, "configs/nginx_conf"); byName.code != http.StatusOK {
		t.Errorf("by name = %d: %s", byName.code, byName.body)
	}
	if rec := f.get(t, e, "configs/ghost"); rec.code != http.StatusNotFound || rec.ptyp != "config_not_found" {
		t.Errorf("unknown = %d %s (%s), want 404 config_not_found", rec.code, rec.body, rec.ptyp)
	}

	ins := f.get(t, e, "configs/cfg-nginx/inspect")
	if ins.code != http.StatusOK {
		t.Fatalf("inspect = %d: %s", ins.code, ins.body)
	}
	if !strings.Contains(string(ins.body), `"nginx_conf"`) || !strings.Contains(string(ins.body), `"Templating"`) {
		t.Errorf("inspect = %s, want the manager's document", ins.body)
	}
	if rec := f.get(t, e, "configs/ghost/inspect"); rec.code != http.StatusNotFound || rec.ptyp != "config_not_found" {
		t.Errorf("unknown inspect = %d (%s), want 404 config_not_found", rec.code, rec.ptyp)
	}
}

func TestGetConfigOfContentThatIsNotTextHasNoText(t *testing.T) {
	t.Parallel()
	binary := []byte{0xff, 0xfe, 0x00, 0x01, 0x80}
	f, e := configsEnv(t, []dockertest.FakeConfigObject{{ID: "cfg-bin", Name: "keystore", Data: binary}})
	rec := f.get(t, e, "configs/cfg-bin")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if !strings.Contains(string(rec.body), `"text":null`) {
		t.Errorf("body = %s, want text null for content that is not UTF-8", rec.body)
	}
	d := decode[configDetail](t, rec)
	if d.Size != len(binary) || d.Data != base64.StdEncoding.EncodeToString(binary) || d.Templating != "none" {
		t.Errorf("detail = %+v", d)
	}
}

func TestDeleteConfigNeedsNoTypedNameAndRefusesOneAServiceStillUses(t *testing.T) {
	t.Parallel()
	free := dockertest.FakeConfigObject{ID: "cfg-free", Name: "free", Data: []byte("x")}
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig(), free}, configUsers()...)

	// In use, even by a service scaled to 0: 409 naming the services, and nothing is sent.
	rec := f.deleteConfig(t, e, "cfg-nginx")
	if rec.code != http.StatusConflict || rec.ptyp != "object_in_use" || !strings.Contains(string(rec.body), "alpha") || !strings.Contains(string(rec.body), "bravo") {
		t.Fatalf("delete in use = %d %s (%s), want 409 object_in_use naming both services", rec.code, rec.body, rec.ptyp)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none for a config in use", w)
	}

	// Unused: removed with no name typed.
	if rec := f.deleteConfig(t, e, "cfg-free"); rec.code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", rec.code, rec.body)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /configs/cfg-free"}) {
		t.Errorf("writes = %v, want one DELETE", w)
	}

	// Gone: 404 and no audit row.
	if rec := f.deleteConfig(t, e, "cfg-free"); rec.code != http.StatusNotFound || rec.ptyp != "config_not_found" {
		t.Errorf("second delete = %d %s (%s), want 404 config_not_found", rec.code, rec.body, rec.ptyp)
	}
	rows := f.configAudits(t)
	var codes []string
	for _, r := range rows {
		if r.Action != "config.removed" {
			t.Errorf("row = %+v", r)
		}
		codes = append(codes, r.Result+"/"+r.Details["code"])
	}
	if want := []string{"failure/object_in_use", "success/done"}; !slices.Equal(codes, want) {
		t.Errorf("audit codes = %v, want %v (a config that is gone is not audited)", codes, want)
	}
	if last := rows[len(rows)-1]; last.TargetType != "config" || last.TargetID != "cfg-free" || last.Details["name"] != "free" {
		t.Errorf("last row = %+v", last)
	}
}

func TestConfigsNeedTheirPermissionsAndAStandaloneHostHasNone(t *testing.T) {
	t.Parallel()
	f, e := configsEnv(t, []dockertest.FakeConfigObject{nginxConfig()})
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	cookie, csrf := f.signIn(t, "viewer", false)
	call := func(method, path, body string) int {
		return f.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf}).Code
	}
	base := "/api/env/" + e.ID + "/configs"
	if c := call(http.MethodGet, base, ""); c != http.StatusOK {
		t.Errorf("a viewer's list = %d, want 200", c)
	}
	// configs.read holders read the content: a viewer's detail and inspect answer.
	if c := call(http.MethodGet, base+"/cfg-nginx", ""); c != http.StatusOK {
		t.Errorf("a viewer's detail = %d, want 200", c)
	}
	if c := call(http.MethodGet, base+"/cfg-nginx/inspect", ""); c != http.StatusOK {
		t.Errorf("a viewer's inspect = %d, want 200", c)
	}
	if c := call(http.MethodPost, base, configBody("x", []byte("v"), "")); c != http.StatusForbidden {
		t.Errorf("a viewer's create = %d, want 403", c)
	}
	if c := call(http.MethodDelete, base+"/cfg-nginx", ""); c != http.StatusForbidden {
		t.Errorf("a viewer's delete = %d, want 403", c)
	}
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	ocookie, ocsrf := f.signIn(t, "operator", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: base, body: configBody("x", []byte("v"), ""), cookie: ocookie, csrf: ocsrf}); rec.Code != http.StatusForbidden {
		t.Errorf("an operator's create = %d, want 403: configs.manage is Admin", rec.Code)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
}

func TestConfigsOfAStandaloneHostAreNotASwarm(t *testing.T) {
	t.Parallel()
	f, e := actionsEnv(t, func(c *dockertest.FakeConfig) { c.SwarmState, c.SwarmControl = "inactive", false })
	for _, rec := range []*envRec{
		f.get(t, e, "configs"),
		f.get(t, e, "configs/x"),
		f.createConfig(t, e, configBody("x", []byte("v"), "")),
		f.deleteConfig(t, e, "x"),
	} {
		if rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
			t.Errorf("standalone = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
		}
	}
}
