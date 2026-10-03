// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/tunnel"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
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
	f.agg = envs.NewAggregator(svc, f.snaps, nil, f.clk.now)
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

func TestEnvironmentMutationsNeedAPermissionAndASession(t *testing.T) {
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
	if got.Kind != "agent" || got.Name != "Production" || got.IsSelfHost || !got.Enabled || got.Status.State != "pending" {
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
	if rec.code != http.StatusConflict || rec.ptyp != "public_url_required" {
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

func TestEnvironmentAddAgentNeedsTheCreatePermission(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"agent"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

// addUserWithGlobalPerms adds a user holding a custom role, assigned
// everywhere, that grants exactly perms (D-49: the split permissions).
func (f *authFixture) addUserWithGlobalPerms(t *testing.T, name string, perms ...string) {
	t.Helper()
	f.addUser(t, name, false)
	role := "role-" + name
	if _, err := f.st.Write.Exec(`INSERT INTO roles (id, name, builtin) VALUES (?, ?, 0)`, role, name); err != nil {
		t.Fatal(err)
	}
	for _, p := range perms {
		if _, err := f.st.Write.Exec(`INSERT INTO role_permissions (role_id, permission) VALUES (?, ?)`, role, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES (?, 'user', ?, ?, NULL, 0)`, "ra-"+name, "id-"+name, role); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentAddNeedsEnvironmentsCreateNotManage(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.addUserWithGlobalPerms(t, "manager", "environments.manage", "environments.delete")
	f.addUserWithGlobalPerms(t, "creator", "environments.create")

	cookie, csrf := f.signIn(t, "manager", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"local"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("add with environments.manage only: %d %s, want 403", rec.Code, rec.Body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorID != "id-manager" || denied[0].Result != "denied" || denied[0].TargetID != "POST /api/environments" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	cookie, csrf = f.signIn(t, "creator", false)
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"local"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add with environments.create: %d %s, want 201", rec.Code, rec.Body)
	}
}

func TestEnvironmentRenameNeedsEnvironmentsManage(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addUserWithGlobalPerms(t, "creator", "environments.create", "environments.delete")
	f.addUserWithGlobalPerms(t, "manager", "environments.manage")

	cookie, csrf := f.signIn(t, "creator", false)
	rec := f.call(t, authReq{method: http.MethodPatch, path: "/api/environments/" + e.ID, body: `{"name":"Lab"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rename without environments.manage: %d %s, want 403", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-creator" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	cookie, csrf = f.signIn(t, "manager", false)
	rec = f.call(t, authReq{method: http.MethodPatch, path: "/api/environments/" + e.ID, body: `{"name":"Lab"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename with environments.manage: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestEnvironmentRemoveNeedsEnvironmentsDelete(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addUserWithGlobalPerms(t, "manager", "environments.create", "environments.manage")
	f.addUserWithGlobalPerms(t, "deleter", "environments.delete")

	cookie, csrf := f.signIn(t, "manager", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/environments/" + e.ID, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remove without environments.delete: %d %s, want 403", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-manager" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	cookie, csrf = f.signIn(t, "deleter", false)
	rec = f.call(t, authReq{method: http.MethodDelete, path: "/api/environments/" + e.ID, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove with environments.delete: %d %s, want 204", rec.Code, rec.Body)
	}
}

// The install object (plan 02-09): one call gives every command and file.

type fullInstall struct {
	envBody
	Install *struct {
		JoinToken  string `json:"joinToken"`
		AgentURL   string `json:"agentUrl"`
		Image      string `json:"image"`
		Pin        string `json:"pin"`
		SecretName string `json:"secretName"`
		Generation int    `json:"generation"`
		Swarm      struct {
			Command       string `json:"command"`
			StackFile     string `json:"stackFile"`
			RotateCommand string `json:"rotateCommand"`
		} `json:"swarm"`
		Standalone struct {
			Command     string `json:"command"`
			ComposeFile string `json:"composeFile"`
		} `json:"standalone"`
		ManualCleanup struct {
			Swarm      string `json:"swarm"`
			Standalone string `json:"standalone"`
		} `json:"manualCleanup"`
	} `json:"install"`
}

func (f *envFix) addAgent(t *testing.T) (fullInstall, *envRec) {
	t.Helper()
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"agent","name":"edge"}`)
	var got fullInstall
	if rec.code == http.StatusCreated {
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("decode %s: %v", rec.body, err)
		}
	}
	return got, rec
}

func TestEnvironmentAddAgentAnswersTheFullInstallObject(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	f.tlsPin = "sha256/PINPINPIN"
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	in := got.Install
	if !strings.HasPrefix(in.JoinToken, "bpjt_"+got.ID+"_") || in.Generation != 1 || in.SecretName != "bayplan-agent-token-g1" {
		t.Errorf("token %q generation %d secret %q", in.JoinToken, in.Generation, in.SecretName)
	}
	if in.AgentURL != "wss://example.com/bayplan/agent/connect" || in.Pin != "sha256/PINPINPIN" {
		t.Errorf("agent URL %q pin %q", in.AgentURL, in.Pin)
	}
	if want := agents.AgentImage(buildinfo.Get().Version); in.Image != want {
		t.Errorf("image = %q, want the server's own version %q", in.Image, want)
	}
	for name, want := range map[string]struct{ text, sub string }{
		"swarm command":      {in.Swarm.Command, "--mode global"},
		"swarm stack file":   {in.Swarm.StackFile, "external: true"},
		"standalone command": {in.Standalone.Command, "docker run -d --name bayplan-agent"},
		"compose file":       {in.Standalone.ComposeFile, "restart: unless-stopped"},
		"swarm cleanup":      {in.ManualCleanup.Swarm, "docker service rm bayplan-agent"},
		"standalone cleanup": {in.ManualCleanup.Standalone, "docker rm -f bayplan-agent"},
	} {
		if !strings.Contains(want.text, want.sub) {
			t.Errorf("%s lacks %q:\n%s", name, want.sub, want.text)
		}
	}
	for name, text := range map[string]string{"swarm command": in.Swarm.Command, "standalone command": in.Standalone.Command} {
		if !strings.Contains(text, in.JoinToken) || !strings.Contains(text, "BAYPLAN_AGENT_PIN=sha256/PINPINPIN") ||
			!strings.Contains(text, "BAYPLAN_AGENT_URL=wss://example.com/bayplan/agent/connect") {
			t.Errorf("%s does not carry the token, the pin and the endpoint:\n%s", name, text)
		}
	}
	if in.Swarm.RotateCommand != "" {
		t.Errorf("a first install has no rotate command, got %q", in.Swarm.RotateCommand)
	}
}

func TestEnvironmentAddAgentLeavesThePinOutWhenBayplanServesNoSelfSignedCertificate(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	f.tlsPin = ""
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if strings.Contains(string(rec.body), `"pin"`) {
		t.Errorf("the answer names a pin: %s", rec.body)
	}
	for _, text := range []string{got.Install.Swarm.Command, got.Install.Swarm.StackFile, got.Install.Standalone.Command, got.Install.Standalone.ComposeFile} {
		if strings.Contains(text, "BAYPLAN_AGENT_PIN") {
			t.Errorf("a text carries a pin although there is none:\n%s", text)
		}
	}
}

func TestEnvironmentAddAgentUsesTheAgentURLWhenOneIsSet(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com/bayplan")
	f.inst.ApplyAgentURL("https://bayplan.lan:7480")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil || got.Install.AgentURL != "wss://bayplan.lan:7480/agent/connect" {
		t.Fatalf("status = %d: %s, want the agent URL's endpoint", rec.code, rec.body)
	}
	if !strings.Contains(got.Install.Swarm.Command, "BAYPLAN_AGENT_URL=wss://bayplan.lan:7480/agent/connect") {
		t.Errorf("the swarm command does not dial the agent URL:\n%s", got.Install.Swarm.Command)
	}
}

func TestEnvironmentAddAgentWithOnlyAnAgentURLIsAllowed(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.ApplyAgentURL("http://10.0.0.5:7480")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil || got.Install.AgentURL != "ws://10.0.0.5:7480/agent/connect" {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
}

func TestEnvironmentAddAgentNeedsAUsableURLAndCreatesNothingWithoutOne(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	_, rec := f.addAgent(t)
	if rec.code != http.StatusConflict || rec.ptyp != "public_url_required" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
	for _, table := range []string{"environments", "env_join_tokens", "env_cas"} {
		var n int
		if err := f.st.Read.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s holds %d rows after a refused add (%v), want none", table, n, err)
		}
	}
	if n := len(f.audits(t, audit.ActionEnvironmentAdded)); n != 0 {
		t.Errorf("%d environment.added entries after a refused add", n)
	}
}

func TestEnvironmentAddAgentNeverPutsTheJoinTokenInTheAudit(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	_, secret, ok := agents.ParseToken(got.Install.JoinToken)
	if !ok || len(secret) < 40 {
		t.Fatalf("the token %q does not parse into a secret of the expected length", got.Install.JoinToken)
	}
	rows, err := f.st.Read.Query(`SELECT id, actor_name, action, target_type, target_id, COALESCE(env_id, ''), result, reason, ip, user_agent, details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var cols [11]string
		if err := rows.Scan(&cols[0], &cols[1], &cols[2], &cols[3], &cols[4], &cols[5], &cols[6], &cols[7], &cols[8], &cols[9], &cols[10]); err != nil {
			t.Fatal(err)
		}
		n++
		if row := strings.Join(cols[:], "|"); strings.Contains(row, secret) {
			t.Errorf("an audit row holds the join token's secret: %s", row)
		}
	}
	if err := rows.Err(); err != nil || n == 0 {
		t.Fatalf("scanned %d audit rows, %v", n, err)
	}
}

// Agent environments over the real tunnel hub (plan 02-11, ENV-06, ENV-08, D-33,
// D-40, D-41): the API is the production router, the agents are real sessions of
// the real hub answering from fake daemons, and the clock is the tunnel's, which
// the test moves.

// agentFix is the API with the tunnel hub behind it. fake is nil: the environments
// here are all agent environments.
type agentFix struct {
	*envFix
	srv  *tunneltest.Server
	tclk *tunneltest.Clock
}

func newAgentFix(t *testing.T) *agentFix {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	log := slog.New(slog.DiscardHandler)
	tclk := tunneltest.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	mon := envs.NewMonitor(log, tclk.Now, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(f.st, f.aud, mon, "", "0123456789ab", tclk.Now)
	f.envSvc = svc
	f.snaps = envs.NewSnapshots(mon, 5*time.Second, tclk.Now)
	f.hub = envs.NewHub(mon, f.snaps, log, tclk.Now)
	t.Cleanup(f.hub.Close)
	f.agg = envs.NewAggregator(svc, f.snaps, f.hub, tclk.Now)
	f.brk = envs.NewBroker(svc, tclk.Now)
	t.Cleanup(f.brk.Close)
	srv := tunneltest.Start(t, tunneltest.Options{
		Clock: tclk, Store: f.st, Observer: svc, Notifier: f.brk,
		Configure: func(h *tunnel.Hub) *tunnel.Hub {
			return h.WithTiming(10*time.Second, 30*time.Second).WithWatchInterval(5 * time.Millisecond)
		},
	})
	svc.WithAgents(srv.Hub)
	ctx, stopBroker := context.WithCancel(context.Background())
	t.Cleanup(stopBroker)
	go f.brk.Run(ctx)
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)
	// The reconnect grace is long over, so a missing agent is offline, not reconnecting.
	tclk.Advance(2 * time.Minute)
	return &agentFix{envFix: &envFix{authFixture: f, svc: svc, mon: mon, cookie: cookie, csrf: csrf}, srv: srv, tclk: tclk}
}

// agentConn is one connected agent and the daemon behind it.
type agentConn struct {
	conn    *tunneltest.Conn
	fake    *dockertest.Fake
	control io.Writer
	id      string
}

// connect enrols an agent in env, answers its Docker streams from a fake daemon
// and sends a heartbeat.
func (f *agentFix) connect(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
	t.Helper()
	fake := dockertest.NewFake(t, cfg)
	if facts == nil {
		facts = &wire.Facts{EngineID: "ENG:" + cfg.Hostname, Hostname: cfg.Hostname, EngineReachable: true}
	}
	c := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: facts})
	if c.Welcome == nil {
		t.Fatalf("enrolment refused: %+v", c.Refuse)
	}
	c.ProxyDocker(t, fake.SocketPath())
	control := c.OpenControl(t)
	tunneltest.SendHeartbeat(t, control, true, f.tclk.Now().UnixMilli())
	return &agentConn{conn: c, fake: fake, control: control, id: c.Identity.AgentID}
}

type agentStatus struct {
	State  string `json:"state"`
	Since  string `json:"since"`
	Reason string `json:"reason"`
	Issues []struct {
		Code  string `json:"code"`
		Node  string `json:"node"`
		Since string `json:"since"`
	} `json:"issues"`
}

type agentEnvBody struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Type   string      `json:"type"`
	Status agentStatus `json:"status"`
	Engine struct {
		Version string `json:"version"`
	} `json:"engine"`
}

// environment reads one environment from GET /api/environments.
func (f *agentFix) environment(t *testing.T, id string) agentEnvBody {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/environments", "")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []agentEnvBody `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Items {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("environment %s is not in %s", id, rec.body)
	return agentEnvBody{}
}

func (f *agentFix) whenState(t *testing.T, id, state string) agentEnvBody {
	t.Helper()
	var e agentEnvBody
	waitFor(t, "the environment to be "+state, func() bool {
		e = f.environment(t, id)
		return e.Status.State == state
	})
	return e
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestAgentEnvironmentIsPendingThenOnlineThenOfflineWithReasonTimeoutAndSince(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	if e := f.environment(t, env.ID); e.Status.State != "pending" || e.Status.Reason != "" || e.Status.Since != "" || e.Kind != "agent" {
		t.Fatalf("a new environment is %+v, want pending with no reason and no since", e)
	}

	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "28.4.0"}, nil)
	e := f.whenState(t, env.ID, "online")
	waitFor(t, "the engine read through the tunnel", func() bool { return f.environment(t, env.ID).Engine.Version == "28.4.0" })
	if e.Name != "Remote" || e.Status.Reason != "" || len(e.Status.Issues) != 0 {
		t.Fatalf("environment = %+v, want the name the admin gave and plain online", e)
	}

	f.srv.Events.WaitLen(t, 1)
	s := f.srv.Hub.Session(a.id)
	waitFor(t, "the heartbeat", func() bool { return !s.Status().LastHeartbeat.IsZero() })
	f.tclk.Advance(31 * time.Second) // silent past the window: the hub closes it with the reason timeout
	closedAt := f.tclk.Now()
	f.srv.Events.WaitLen(t, 2)
	e = f.environment(t, env.ID)
	if e.Status.State != "offline" || e.Status.Reason != "timeout" || e.Status.Since != utc(closedAt) {
		t.Fatalf("status = %+v, want offline / timeout since %s", e.Status, utc(closedAt))
	}
}

func TestAgentEnvironmentWithAMissingNodeIsDegradedAndListsItsIssues(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Swarm")
	manager := &wire.Facts{
		EngineID: "ENG:m", Hostname: "manager-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-m", ClusterID: "cluster-1", Control: true},
	}
	worker := &wire.Facts{
		EngineID: "ENG:w", Hostname: "worker-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-w", Control: false},
	}
	m := f.connect(t, env, dockertest.FakeConfig{Hostname: "manager-1", SwarmState: "active", SwarmControl: true}, manager)
	w := f.connect(t, env, dockertest.FakeConfig{Hostname: "worker-1", SwarmState: "active"}, worker)
	f.srv.Events.WaitLen(t, 2)
	f.whenState(t, env.ID, "online")

	f.tclk.Advance(20 * time.Second)
	tunneltest.SendHeartbeat(t, m.control, true, f.tclk.Now().UnixMilli())
	waitFor(t, "the manager's heartbeat", func() bool { return f.srv.Hub.Session(m.id).Status().LastHeartbeat.Equal(f.tclk.Now()) })
	f.tclk.Advance(11 * time.Second) // the worker has been silent for 31 s, the manager for 11
	f.srv.Events.WaitLen(t, 3)
	droppedAt := f.tclk.Now()

	e := f.environment(t, env.ID)
	if e.Status.State != "degraded" || e.Status.Reason != "agent_offline" {
		t.Fatalf("status = %+v, want degraded / agent_offline", e.Status)
	}
	if len(e.Status.Issues) != 1 || e.Status.Issues[0].Code != "agent_offline" || e.Status.Issues[0].Node != "worker-1" || e.Status.Issues[0].Since != utc(droppedAt) {
		t.Fatalf("issues = %+v, want agent_offline on worker-1 since %s", e.Status.Issues, utc(droppedAt))
	}
	_ = w
}

func TestAnEnvironmentTheCallerMayNotReadIsNotListedWhateverItsAgentsDo(t *testing.T) {
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Secret")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1"}, nil)
	f.whenState(t, env.ID, "online")

	f.addUser(t, "viewer", false)
	cookie, _ := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/environments", cookie: cookie})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), env.ID) || strings.Contains(rec.Body.String(), "edge-1") {
		t.Fatalf("a user with no grant saw the environment: %d %s", rec.Code, rec.Body)
	}
}
