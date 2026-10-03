// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// The environment endpoints (plan 16, ENV-04, PLAT-06, D-19, D-20, D-23, D-25,
// D-26, D-48). Docker is a FakeDaemon on a real unix socket, so the whole path
// runs: HTTP, policy, registry, monitor, probe.

type envBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Type       string `json:"type"`
	IsSelfHost bool   `json:"isSelfHost"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"createdAt"`
	Status     struct {
		State       string `json:"state"`
		Since       string `json:"since"`
		Reason      string `json:"reason"`
		LastAttempt string `json:"lastAttempt"`
	} `json:"status"`
	Engine struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		APIVersion string `json:"apiVersion"`
	} `json:"engine"`
}

type envFix struct {
	*authFixture
	fake         *dockertest.Fake
	svc          *envs.Service
	mon          *envs.Monitor
	cookie, csrf string
}

func newEnvFix(t *testing.T, cfg dockertest.FakeConfig) *envFix {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	fake := dockertest.NewFake(t, cfg)
	mon := envs.NewMonitor(slog.New(slog.DiscardHandler), nil, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(f.st, f.aud, mon, "unix://"+fake.SocketPath(), "0123456789ab", f.clk.now)
	f.envSvc = svc
	f.snaps = envs.NewSnapshots(mon, 5*time.Second, f.clk.now)
	f.localHost.DockerHost = "unix://" + fake.SocketPath()
	f.localHost.ProbeTimeout = 500 * time.Millisecond
	f.localHost.Envs = svc
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	return &envFix{authFixture: f, fake: fake, svc: svc, mon: mon, cookie: cookie, csrf: csrf}
}

func (f *envFix) req(t *testing.T, method, path, body string) *envRec {
	t.Helper()
	rec := f.call(t, authReq{method: method, path: path, body: body, cookie: f.cookie, csrf: f.csrf})
	ptyp := ""
	if rec.Code >= 400 {
		ptyp = problemType(t, rec)
	}
	return &envRec{rec.Code, rec.Body.Bytes(), ptyp}
}

type envRec struct {
	code int
	body []byte
	ptyp string
}

func (r *envRec) env(t *testing.T) envBody {
	t.Helper()
	var e envBody
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return e
}

func (f *envFix) add(t *testing.T) envBody {
	t.Helper()
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.code, rec.body)
	}
	return rec.env(t)
}

func TestEnvironmentAddLocalAsAdminReturns201AndAudits(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0", APIVersion: "1.52"})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	e := rec.env(t)
	if e.ID == "" || e.Name != "node-1" || e.Kind != "local" || !e.IsSelfHost || !e.Enabled || e.Type != "standalone" {
		t.Errorf("environment = %+v", e)
	}
	if e.Status.State != "online" || e.Status.Since == "" || e.Engine.Name != "Docker Engine" || e.Engine.Version != "29.1.0" || e.Engine.APIVersion != "1.52" {
		t.Errorf("status/engine = %+v / %+v", e.Status, e.Engine)
	}
	for _, ts := range []string{e.CreatedAt, e.Status.Since} {
		if p, err := time.Parse(time.RFC3339, ts); err != nil || p.Location() != time.UTC {
			t.Errorf("timestamp %q is not RFC 3339 UTC (D-66): %v", ts, err)
		}
	}
	entries := f.audits(t, audit.ActionEnvironmentAdded)
	if len(entries) != 1 || entries[0].TargetID != e.ID || entries[0].ActorID != "id-admin" || entries[0].IP != authIP || entries[0].UserAgent != authUA {
		t.Errorf("environment.added = %+v", entries)
	}
}

func TestEnvironmentAddLocalWithAnExplicitNameKeepsIt(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local","name":"Home lab"}`)
	if rec.code != http.StatusCreated || rec.env(t).Name != "Home lab" {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
}

func TestEnvironmentAddLocalASecondTimeIs409LocalEnvironmentExists(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.add(t)
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "local_environment_exists" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentAddLocalWithoutASocketIs409SocketNotFound(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	svc := envs.NewService(f.st, f.aud, f.mon, "unix://"+filepath.Join(t.TempDir(), "none.sock"), "", f.clk.now)
	f.envSvc = svc
	f.h = f.build(t)
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "socket_not_found" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentAddLocalRefusesAnUnsupportedEngineUnlessAllowed(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{APIVersion: "1.41", Version: "20.10.24", Hostname: "old"})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "unsupported_engine" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := f.req(t, http.MethodGet, "/api/environments", ""); strings.Contains(string(got.body), `"id"`) {
		t.Errorf("a refused add stored an environment: %s", got.body)
	}
	rec = f.req(t, http.MethodPost, "/api/environments", `{"kind":"local","allowUnsupported":true}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("allowUnsupported: %d %s", rec.code, rec.body)
	}
	e := rec.env(t)
	if e.Status.State != "unsupported" || e.Status.Reason != "api_too_old" || e.Engine.Version != "20.10.24" {
		t.Errorf("environment = %+v", e)
	}
}

func TestEnvironmentAddLocalRejectsAnInvalidNameAndAnUnknownKind(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local","name":"`+strings.Repeat("x", 65)+`"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
		t.Errorf("long name: status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	rec = f.req(t, http.MethodPost, "/api/environments", `{"kind":"remote"}`)
	if rec.code != http.StatusUnprocessableEntity {
		t.Errorf("unknown kind: status = %d: %s", rec.code, rec.body)
	}
}

func TestEnvironmentListShowsEachEnvironmentWithItsStatus(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true})
	if got := f.req(t, http.MethodGet, "/api/environments", ""); got.code != http.StatusOK || !strings.Contains(string(got.body), `"items":[]`) {
		t.Fatalf("empty list = %d %s", got.code, got.body)
	}
	added := f.add(t)
	// Right after add the monitor has not finished its first probe, and the
	// list shows its state; wait for it so the assertions below do not race it.
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(added.ID).State == envs.StateOnline })
	rec := f.req(t, http.MethodGet, "/api/environments", "")
	var list struct {
		Items []envBody `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil || rec.code != http.StatusOK || len(list.Items) != 1 {
		t.Fatalf("list = %d %s (%v)", rec.code, rec.body, err)
	}
	e := list.Items[0]
	if e.ID != added.ID || e.Type != "swarm-manager" || !e.IsSelfHost || e.Status.State == "" {
		t.Errorf("listed = %+v", e)
	}
}

func TestEnvironmentListHidesEnvironmentsTheCallerCannotRead(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	for _, q := range []string{
		`INSERT INTO roles (id, name, builtin) VALUES ('role-viewer', 'viewer', 0)`,
		`INSERT INTO role_permissions (role_id, permission) VALUES ('role-viewer', 'environments.read')`,
	} {
		if _, err := f.st.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.addUser(t, "nobody", false)
	f.addUser(t, "viewer", false)
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES ('ra-1', 'user', 'id-viewer', 'role-viewer', ?, 0)`, e.ID); err != nil {
		t.Fatal(err)
	}

	list := func(user string) string {
		cookie, _ := f.signIn(t, user, false)
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments", cookie: cookie})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", user, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	if got := list("nobody"); strings.Contains(got, e.ID) {
		t.Errorf("a user with no environments.read saw the environment: %s", got)
	}
	if got := list("viewer"); !strings.Contains(got, e.ID) {
		t.Errorf("a user with environments.read on it did not see it: %s", got)
	}
}

func TestEnvironmentRenameAndRemoveAuditAndAnswer404ForAnUnknownID(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	e := f.add(t)

	rec := f.req(t, http.MethodPatch, "/api/environments/"+e.ID, `{"name":" Lab "}`)
	if rec.code != http.StatusOK || rec.env(t).Name != "Lab" {
		t.Fatalf("rename: %d %s", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionEnvironmentRenamed); len(got) != 1 || got[0].TargetID != e.ID {
		t.Errorf("environment.renamed = %+v", got)
	}
	if rec := f.req(t, http.MethodPatch, "/api/environments/"+e.ID, `{"name":""}`); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
		t.Errorf("empty name: %d %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPatch, "/api/environments/nope", `{"name":"x"}`); rec.code != http.StatusNotFound {
		t.Errorf("rename unknown: %d %s", rec.code, rec.body)
	}
	if rec := f.req(t, http.MethodDelete, "/api/environments/nope", ""); rec.code != http.StatusNotFound {
		t.Errorf("remove unknown: %d %s", rec.code, rec.body)
	}

	if rec := f.req(t, http.MethodDelete, "/api/environments/"+e.ID, ""); rec.code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionEnvironmentRemoved); len(got) != 1 || got[0].TargetID != e.ID {
		t.Errorf("environment.removed = %+v", got)
	}
	if rec := f.req(t, http.MethodGet, "/api/environments", ""); strings.Contains(string(rec.body), e.ID) {
		t.Errorf("a removed environment is still listed: %s", rec.body)
	}
}

func TestEnvironmentReconnectReturns202AndProbesNow(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	infoCalls := func() int {
		n := 0
		for _, p := range f.fake.Requests() {
			if strings.HasSuffix(p, "/info") {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.mon.Status(e.ID).State != envs.StateOnline {
		if time.Now().After(deadline) {
			t.Fatal("the monitor never saw the engine")
		}
		time.Sleep(2 * time.Millisecond)
	}
	before := infoCalls()
	rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/reconnect", "")
	if rec.code != http.StatusAccepted {
		t.Fatalf("reconnect: %d %s", rec.code, rec.body)
	}
	if got := rec.env(t); got.ID != e.ID || got.Status.State != "online" {
		t.Errorf("reconnect body = %+v", got)
	}
	for infoCalls() <= before {
		if time.Now().After(deadline) {
			t.Fatal("reconnect did not cause a probe")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := f.audits(t, "environment.reconnect"); len(got) != 0 {
		t.Errorf("reconnect changes no state and must not be audited as a mutation: %+v", got)
	}
}

func TestEnvironmentScopedRoutesResolveAgainstTheDatabase(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	rec := f.req(t, http.MethodPost, "/api/env/no-such-env/reconnect", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("unknown env: %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if _, err := f.st.Write.Exec(`UPDATE environments SET enabled = 0 WHERE id = ?`, e.ID); err != nil {
		t.Fatal(err)
	}
	rec = f.req(t, http.MethodPost, "/api/env/"+e.ID+"/reconnect", "")
	if rec.code != http.StatusConflict || rec.ptyp != "environment_disabled" {
		t.Errorf("disabled env: %d %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentMutationsRequireTheCSRFHeader(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/environments", `{"kind":"local"}`},
		{http.MethodPatch, "/api/environments/" + e.ID, `{"name":"x"}`},
		{http.MethodDelete, "/api/environments/" + e.ID, ""},
		{http.MethodPost, "/api/env/" + e.ID + "/reconnect", ""},
	} {
		rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body, cookie: f.cookie})
		if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
			t.Errorf("%s %s without the CSRF header: %d %s, want 403 csrf_failed", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if got := f.req(t, http.MethodGet, "/api/environments", ""); !strings.Contains(string(got.body), e.ID) {
		t.Error("a refused mutation changed the environment")
	}
}

func TestEnvironmentMutationsNeedEnvironmentsWriteAndASession(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/environments", `{"kind":"local"}`},
		{http.MethodPatch, "/api/environments/" + e.ID, `{"name":"x"}`},
		{http.MethodDelete, "/api/environments/" + e.ID, ""},
		{http.MethodPost, "/api/env/" + e.ID + "/reconnect", ""},
	} {
		if rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a user with no permissions: %d, want 403", c.method, c.path, rec.Code)
		}
		if rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s anonymous: %d, want 401", c.method, c.path, rec.Code)
		}
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/environments anonymous: %d, want 401", rec.Code)
	}
}

func TestEnvironmentLocalHostReportsManagedAndTheOwnContainerID(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	read := func() (managed bool, own string) {
		rec := f.call(t, authReq{method: http.MethodGet, path: "/api/local-host", cookie: f.cookie})
		var b struct {
			Managed        bool   `json:"managed"`
			OwnContainerID string `json:"ownContainerId"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &b) != nil {
			t.Fatalf("local-host: %d %s", rec.Code, rec.Body)
		}
		return b.Managed, b.OwnContainerID
	}
	if managed, own := read(); managed || own != "0123456789ab" {
		t.Errorf("before adding: managed=%v own=%q, want false and 0123456789ab", managed, own)
	}
	e := f.add(t)
	if managed, _ := read(); !managed {
		t.Error("after adding: managed = false, want true")
	}
	f.req(t, http.MethodDelete, "/api/environments/"+e.ID, "")
	if managed, _ := read(); managed {
		t.Error("after removing: managed = true, want false")
	}
}

// Agent environments (plan 02-01): kind agent creates an environment with its CA
// and a join token, and the answer carries the install details once.

type installBody struct {
	envBody
	Install *struct {
		JoinToken string `json:"joinToken"`
		AgentURL  string `json:"agentUrl"`
	} `json:"install"`
}

func TestEnvironmentAddAgentAnswers201WithTheJoinTokenAndTheAgentURL(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"agent","name":"Production"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	var got installBody
	if err := json.Unmarshal(rec.body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "agent" || got.Name != "Production" || got.IsSelfHost || !got.Enabled || got.Status.State != "unknown" {
		t.Errorf("environment = %+v", got.envBody)
	}
	if got.Install == nil || !strings.HasPrefix(got.Install.JoinToken, "bpjt_"+got.ID+"_") {
		t.Fatalf("install = %+v", got.Install)
	}
	if got.Install.AgentURL != "wss://example.com/bayplan/agent/connect" {
		t.Errorf("agent URL = %q", got.Install.AgentURL)
	}
	if entries := f.audits(t, audit.ActionEnvironmentAdded); len(entries) != 1 || entries[0].TargetID != got.ID || entries[0].ActorID != "id-admin" {
		t.Errorf("environment.added = %+v", entries)
	}

	// The token is never in a later answer, and the audit entry does not hold it.
	list := f.req(t, http.MethodGet, "/api/environments", "")
	if strings.Contains(string(list.body), got.Install.JoinToken) || strings.Contains(string(list.body), "joinToken") {
		t.Errorf("the list shows the join token: %s", list.body)
	}
	var raw struct{ Items []installBody }
	if err := json.Unmarshal(list.body, &raw); err != nil || len(raw.Items) != 1 || raw.Items[0].Kind != "agent" || raw.Items[0].Install != nil {
		t.Errorf("list = %s (%v)", list.body, err)
	}
	var details string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'environment.added'`).Scan(&details); err != nil ||
		strings.Contains(details, got.Install.JoinToken) {
		t.Errorf("audit details = %q, %v", details, err)
	}
}

func TestEnvironmentAddAgentWithoutAPublicURLIs409(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"agent"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "public_url_missing" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	if got := f.req(t, http.MethodGet, "/api/environments", ""); strings.Contains(string(got.body), `"id"`) {
		t.Errorf("a refused add stored an environment: %s", got.body)
	}
}

func TestEnvironmentAddAgentRejectsAnInvalidName(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"agent","name":"`+strings.Repeat("x", 65)+`"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentAddAgentNeedsTheWritePermission(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"agent"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}
