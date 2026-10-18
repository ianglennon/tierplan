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
	"github.com/ianglennon/bayplan/proto/testsupport"
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
	f.localHost.ProbeTimeout = testsupport.Deadline // a probe cut short under load would read as an unreachable engine
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
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local","name":"Home lab"}`)
	if rec.code != http.StatusCreated || rec.env(t).Name != "Home lab" {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
}

func TestEnvironmentAddLocalASecondTimeIs409LocalEnvironmentExists(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.add(t)
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusConflict || rec.ptyp != "local_environment_exists" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentAddLocalWithoutASocketIs409SocketNotFound(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true})
	if got := f.req(t, http.MethodGet, "/api/environments", ""); got.code != http.StatusOK || !strings.Contains(string(got.body), `"items":[]`) {
		t.Fatalf("empty list = %d %s", got.code, got.body)
	}
	added := f.add(t)
	// Right after add the monitor has not finished its first probe, and the
	// list shows its state; wait for it so the assertions below do not race it.
	awaitOnline(t, f.mon, added.ID)
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addUser(t, "nobody", false)
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")

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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	e := f.add(t)

	rec := f.req(t, http.MethodPatch, "/api/env/"+e.ID, `{"name":" Lab "}`)
	if rec.code != http.StatusOK || rec.env(t).Name != "Lab" {
		t.Fatalf("rename: %d %s", rec.code, rec.body)
	}
	if got := f.audits(t, audit.ActionEnvironmentRenamed); len(got) != 1 || got[0].TargetID != e.ID {
		t.Errorf("environment.renamed = %+v", got)
	}
	if rec := f.req(t, http.MethodPatch, "/api/env/"+e.ID, `{"name":""}`); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
		t.Errorf("empty name: %d %q", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPatch, "/api/env/nope", `{"name":"x"}`); rec.code != http.StatusNotFound {
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
	t.Parallel()
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
	awaitOnline(t, f.mon, e.ID)
	before := infoCalls()
	rec := f.req(t, http.MethodPost, "/api/env/"+e.ID+"/reconnect", "")
	if rec.code != http.StatusAccepted {
		t.Fatalf("reconnect: %d %s", rec.code, rec.body)
	}
	if got := rec.env(t); got.ID != e.ID || got.Status.State != "online" {
		t.Errorf("reconnect body = %+v", got)
	}
	awaitFake(t, f.fake, "the reconnect to cause a probe", func() bool { return infoCalls() > before })
	if got := f.audits(t, "environment.reconnect"); len(got) != 0 {
		t.Errorf("reconnect changes no state and must not be audited as a mutation: %+v", got)
	}
}

func TestEnvironmentScopedRoutesResolveAgainstTheDatabase(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/environments", `{"kind":"local"}`},
		{http.MethodPatch, "/api/env/" + e.ID, `{"name":"x"}`},
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/environments", `{"kind":"local"}`},
		{http.MethodPatch, "/api/env/" + e.ID, `{"name":"x"}`},
		{http.MethodDelete, "/api/environments/" + e.ID, ""},
		{http.MethodPost, "/api/env/" + e.ID + "/reconnect", ""},
	} {
		// A global route refuses with 403; an environment the user has no grant on
		// is not visible to them at all, so its route answers 404 (D-08).
		want := http.StatusForbidden
		if strings.HasPrefix(c.path, "/api/env/") {
			want = http.StatusNotFound
		}
		if rec := f.call(t, authReq{method: c.method, path: c.path, body: c.body, cookie: cookie, csrf: csrf}); rec.Code != want {
			t.Errorf("%s %s as a user with no permissions: %d, want %d", c.method, c.path, rec.Code, want)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"agent","name":"`+strings.Repeat("x", 65)+`"}`)
	if rec.code != http.StatusUnprocessableEntity || rec.ptyp != "invalid_name" {
		t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
	}
}

func TestEnvironmentAddAgentNeedsTheCreatePermission(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.Apply("Bayplan", "https://example.com")
	f.addUser(t, "viewer", false)
	cookie, csrf := f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"agent"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
}

// addInstanceAdmin adds a user who is an instance admin (D-01) and returns the
// user's ID.
func (f *authFixture) addInstanceAdmin(t *testing.T, name string) string {
	t.Helper()
	return f.addUser(t, name, true)
}

// addEnvRole gives a user that addUser has made a role ("viewer", "operator" or
// "admin") on one environment, or on every environment when envID is empty. A
// subject holds one assignment per scope.
func (f *authFixture) addEnvRole(t *testing.T, name, envID, role string) {
	t.Helper()
	var env any
	if envID != "" {
		env = envID
	}
	if _, err := f.st.Write.Exec(`INSERT INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at)
		VALUES (?, 'user', ?, ?, ?, 0)`, "ra-"+name+"-"+envID, "id-"+name, role, env); err != nil {
		t.Fatal(err)
	}
}

// addAllEnvAdmin adds a user who is Admin on every environment but is not an
// instance admin: the caller D-01 refuses every global operation.
func (f *authFixture) addAllEnvAdmin(t *testing.T, name string) {
	t.Helper()
	f.addUser(t, name, false)
	f.addEnvRole(t, name, "", "admin")
}

func TestEnvironmentAddIsForInstanceAdminsNotForAnAdminOfEveryEnvironment(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.addAllEnvAdmin(t, "envadmin")

	cookie, csrf := f.signIn(t, "envadmin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"local"}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("add as an admin of every environment: %d %s, want 403", rec.Code, rec.Body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorID != "id-envadmin" || denied[0].Result != "denied" || denied[0].TargetID != "POST /api/environments" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	f.addInstanceAdmin(t, "owner")
	ownerCookie, ownerCSRF := f.signIn(t, "owner", false)
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/environments", body: `{"kind":"local"}`, cookie: ownerCookie, csrf: ownerCSRF})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add as an instance admin: %d %s, want 201", rec.Code, rec.Body)
	}
}

// asUser calls a route as an existing user (made by addUser), signing them in first.
func (f *authFixture) asUser(t *testing.T, name, method, path, body string) *envRec {
	t.Helper()
	cookie, csrf := f.signIn(t, name, false)
	rec := f.call(t, authReq{method: method, path: path, body: body, cookie: cookie, csrf: csrf})
	ptyp := ""
	if rec.Code >= 400 {
		ptyp = problemType(t, rec)
	}
	return &envRec{rec.Code, rec.Body.Bytes(), ptyp}
}

// addEnvUser makes a user who holds one role on one environment and nothing else.
func (f *authFixture) addEnvUser(t *testing.T, name, envID, role string) {
	t.Helper()
	f.addUser(t, name, false)
	f.addEnvRole(t, name, envID, role)
}

// An env Admin renames the environment they administer through the environment-scoped
// route (D-07), sees no other environment, and cannot go through the old path.
func TestAnEnvAdminRenamesTheirOwnEnvironmentAndNoOtherThroughTheEnvironmentScopedRoute(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	a := f.srv.NewEnvironment(t, "Alpha")
	b := f.srv.NewEnvironment(t, "Beta")
	f.addUser(t, "adminone", false)
	f.addEnvRole(t, "adminone", a.ID, "admin")
	f.addUser(t, "viewerone", false)
	f.addEnvRole(t, "viewerone", a.ID, "viewer")

	rec := f.asUser(t, "adminone", http.MethodPatch, "/api/env/"+a.ID, `{"name":"prod"}`)
	if rec.code != http.StatusOK || rec.env(t).Name != "prod" {
		t.Fatalf("rename as an env Admin on the environment: %d %s, want 200", rec.code, rec.body)
	}
	renamed := f.audits(t, audit.ActionEnvironmentRenamed)
	if len(renamed) != 1 || renamed[0].TargetID != a.ID || renamed[0].ActorID != "id-adminone" {
		t.Fatalf("environment.renamed = %+v", renamed)
	}
	var envID string
	if err := f.st.Read.QueryRow(`SELECT env_id FROM audit_log WHERE action = ?`, audit.ActionEnvironmentRenamed).Scan(&envID); err != nil || envID != a.ID {
		t.Fatalf("environment.renamed env_id = %q (%v), want %s", envID, err, a.ID)
	}

	rec = f.asUser(t, "adminone", http.MethodPatch, "/api/env/"+b.ID, `{"name":"stolen"}`)
	if rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Fatalf("rename of another environment: %d %q %s, want 404 environment_not_found", rec.code, rec.ptyp, rec.body)
	}
	if got := f.audits(t, audit.ActionEnvironmentRenamed); len(got) != 1 {
		t.Fatalf("a refused rename was audited as a rename: %+v", got)
	}

	rec = f.asUser(t, "viewerone", http.MethodPatch, "/api/env/"+a.ID, `{"name":"nope"}`)
	if rec.code != http.StatusForbidden {
		t.Fatalf("rename as a Viewer: %d %s, want 403", rec.code, rec.body)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].ActorID != "id-viewerone" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	// An instance admin renames any environment; the old path is gone.
	if rec := f.req(t, http.MethodPatch, "/api/env/"+b.ID, `{"name":"Beta 2"}`); rec.code != http.StatusOK {
		t.Fatalf("rename as an instance admin: %d %s, want 200", rec.code, rec.body)
	}
	if rec := f.req(t, http.MethodPatch, "/api/environments/"+a.ID, `{"name":"x"}`); rec.code != http.StatusNotFound && rec.code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH on the old path: %d %s, want 404 or 405", rec.code, rec.body)
	}
}

func TestEnvironmentRemoveIsForInstanceAdminsNotForAnAdminOfEveryEnvironment(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	e := f.add(t)
	f.addAllEnvAdmin(t, "envadmin")

	cookie, csrf := f.signIn(t, "envadmin", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/environments/" + e.ID, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remove as an admin of every environment: %d %s, want 403", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-envadmin" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}

	rec2 := f.req(t, http.MethodDelete, "/api/environments/"+e.ID, "")
	if rec2.code != http.StatusNoContent {
		t.Fatalf("remove as an instance admin: %d %s, want 204", rec2.code, rec2.body)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{})
	f.inst.ApplyAgentURL("https://10.0.0.5:7480")
	got, rec := f.addAgent(t)
	if rec.code != http.StatusCreated || got.Install == nil || got.Install.AgentURL != "wss://10.0.0.5:7480/agent/connect" {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
}

// With the development switch off, an address that is plain ws:// for a host that
// is not this machine is one agents refuse at start (plan 02-41), so Bayplan hands
// out no install for it: 409 insecure_agent_url, before any state changes
// (G-02-R4-3, T-02-144, T-02-145). The agent URL and the public URL are both
// judged, whichever supplies the address.
func TestEnvironmentAddAgentRefusesAPlainAddressForAnotherHostWithoutTheSwitch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		apply func(f *envFix)
	}{
		{"only an agent URL", func(f *envFix) { f.inst.ApplyAgentURL("http://10.0.0.5:7480") }},
		{"only a public URL", func(f *envFix) { f.inst.Apply("Bayplan", "http://10.0.0.5:7480") }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newEnvFix(t, dockertest.FakeConfig{})
			tc.apply(f)
			_, rec := f.addAgent(t)
			if rec.code != http.StatusConflict || rec.ptyp != "insecure_agent_url" {
				t.Fatalf("status = %d, type %q: %s", rec.code, rec.ptyp, rec.body)
			}
			if strings.Contains(string(rec.body), "10.0.0.5") {
				t.Errorf("the refusal repeats the address: %s", rec.body)
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
		})
	}
}

func TestEnvironmentAddAgentNeedsAUsableURLAndCreatesNothingWithoutOne(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func newAgentFix(t *testing.T) *agentFix { return newAgentFixHost(t, "") }

// newAgentFixHost is newAgentFix with a Docker host for the local socket, so a test can
// opt the local host in beside its agent environments.
func newAgentFixHost(t *testing.T, dockerHost string) *agentFix {
	t.Helper()
	f := newAuthFixture(t, config.Config{})
	log := slog.New(slog.DiscardHandler)
	tclk := tunneltest.NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	mon := envs.NewMonitor(log, tclk.Now, nil)
	t.Cleanup(mon.StopAll)
	svc := envs.NewService(f.st, f.aud, mon, dockerHost, "0123456789ab", tclk.Now)
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
	f.agentSvc, f.liveAgents = srv.Svc, srv.Hub
	f.envWarningsNow = tclk.Now
	// The server is v0.2.1 and the agents tunneltest connects are v0.2.0.
	f.serverVersion = "v0.2.1"
	f.updater = agents.NewUpdater(srv.Svc, agents.HubSource(srv.Hub), nil, f.serverVersion, tclk.Now)
	t.Cleanup(f.updater.Close)
	svc.WithUninstaller(f.updater)
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
	control io.ReadWriter
	id      string
}

// connect enrols an agent in env, answers its Docker streams from a fake daemon
// and sends a heartbeat. It returns once the agent is ready for a call to one of its
// nodes: the server has read the first heartbeat and the monitor has finished the
// first probe of the agent's engine. A call through the monitor's client is ErrOffline
// until that probe is over, so a test that forgot to wait for it would see
// node_unreachable in place of what it asserts (the agent_policy_denied flake).
func (f *agentFix) connect(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
	t.Helper()
	a := f.connectNoProbe(t, env, cfg, facts)
	awaitFirstProbe(t, f.mon, envs.AgentTarget(a.id))
	return a
}

// connectNoProbe is connect without the wait for the first probe, for a test that must see
// a state from before the first probe or holds the probe on purpose (FakeConfig.ProbeHold).
// It still returns only once the server has read the agent's first heartbeat.
func (f *agentFix) connectNoProbe(t *testing.T, env tunneltest.Environment, cfg dockertest.FakeConfig, facts *wire.Facts) *agentConn {
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
	// The server takes the control stream asynchronously. Its first heartbeat is
	// read only after the stream is registered, so once it is seen the server can
	// send this agent control messages (an uninstall, an update) without racing.
	f.awaitFirstHeartbeat(t, c)
	return &agentConn{conn: c, fake: fake, control: control, id: c.Identity.AgentID}
}

// awaitFirstHeartbeat waits until the server has registered the session of the agent on c and
// read its first heartbeat, and returns the session.
func (f *agentFix) awaitFirstHeartbeat(t *testing.T, c *tunneltest.Conn) *tunnel.Session {
	t.Helper()
	s, err := f.srv.Hub.AwaitSession(testsupport.Context(t), c.Identity.AgentID, func(s *tunnel.Session) bool { return s != nil })
	if err != nil {
		t.Fatalf("the server never registered the session of agent %s: %v", c.Identity.AgentID, err)
	}
	awaitHeartbeat(t, s, func(st tunnel.SessionStatus) bool { return !st.LastHeartbeat.IsZero() })
	return s
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
	return awaitEnvState(t, f, id, state)
}

// whenProbed waits until the monitor has probed the engine of every node key and
// found it online. An environment is online as soon as its agent's session is,
// while a call to one node goes through the monitor's client, which is ErrOffline
// until the first probe of that engine has finished.
func (f *agentFix) whenProbed(t *testing.T, keys ...string) {
	t.Helper()
	awaitProbed(t, f.mon, keys...)
}

func utc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestAgentEnvironmentIsPendingThenOnlineThenOfflineWithReasonTimeoutAndSince(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Remote")
	if e := f.environment(t, env.ID); e.Status.State != "pending" || e.Status.Reason != "" || e.Status.Since != "" || e.Kind != "agent" {
		t.Fatalf("a new environment is %+v, want pending with no reason and no since", e)
	}

	a := f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "28.4.0"}, nil)
	e := f.whenState(t, env.ID, "online")
	// connect returned after the first probe, which is what records the engine version.
	if e.Engine.Version != "28.4.0" {
		t.Fatalf("the engine read through the tunnel is %q, want 28.4.0 after the first probe", e.Engine.Version)
	}
	if e.Name != "Remote" || e.Status.Reason != "" || len(e.Status.Issues) != 0 {
		t.Fatalf("environment = %+v, want the name the admin gave and plain online", e)
	}

	f.srv.Events.WaitLen(t, 1)
	s := f.srv.Hub.Session(a.id)
	awaitHeartbeat(t, s, func(st tunnel.SessionStatus) bool { return !st.LastHeartbeat.IsZero() })
	f.tclk.Advance(31 * time.Second) // silent past the window: the hub closes it with the reason timeout
	closedAt := f.tclk.Now()
	f.srv.Events.WaitLen(t, 2)
	e = f.environment(t, env.ID)
	if e.Status.State != "offline" || e.Status.Reason != "timeout" || e.Status.Since != utc(closedAt) {
		t.Fatalf("status = %+v, want offline / timeout since %s", e.Status, utc(closedAt))
	}
}

func TestAgentEnvironmentWithAMissingNodeIsDegradedAndListsItsIssues(t *testing.T) {
	t.Parallel()
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
	awaitHeartbeat(t, f.srv.Hub.Session(m.id), func(st tunnel.SessionStatus) bool { return st.LastHeartbeat.Equal(f.tclk.Now()) })
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
	t.Parallel()
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

// The overview columns of GET /api/environments (plan 02-12, ENV-06, D-32): what
// the /environments table, the switcher and the mobile rows show for each
// environment without asking for its resources.

type countsBody struct {
	Ready     int `json:"ready"`
	Total     int `json:"total"`
	Failing   int `json:"failing"`
	Connected int `json:"connected"`
	Expected  int `json:"expected"`
}

// overview is the overview part of one listed environment. A pointer field is nil
// when the body said null.
type overview struct {
	ID              string      `json:"id"`
	EnvType         *string     `json:"envType"`
	IsLocalSocket   bool        `json:"isLocalSocket"`
	Nodes           *countsBody `json:"nodes"`
	FailingServices *countsBody `json:"failingServices"`
	Agents          *countsBody `json:"agents"`
	AgentVersion    *string     `json:"agentVersion"`
	UpdateAvailable bool        `json:"updateAvailable"`
	EngineVersion   *string     `json:"engineVersion"`
	LastSeen        *string     `json:"lastSeen"`
}

// rawEnvironment is one listed environment as the raw fields of its JSON object, so
// a test can tell null from absent.
func rawEnvironment(t *testing.T, body []byte, id string) map[string]json.RawMessage {
	t.Helper()
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	for _, it := range list.Items {
		if string(it["id"]) == `"`+id+`"` {
			return it
		}
	}
	t.Fatalf("environment %s is not in %s", id, body)
	return nil
}

func overviewOf(t *testing.T, f *envFix, id string) overview {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/environments", "")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []overview `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &list); err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Items {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("environment %s is not in %s", id, rec.body)
	return overview{}
}

func counts(t *testing.T, what string, got *countsBody, ready, total int) {
	t.Helper()
	if got == nil || got.Ready != ready || got.Total != total {
		t.Fatalf("%s = %+v, want ready %d of %d", what, got, ready, total)
	}
}

func TestOverviewOfAStandaloneAgentEnvironmentFollowsItsAgentAndTheServerVersion(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	f.svc.WithHealth(envs.HealthConfig{ServerVersion: "v0.2.0"})
	env := f.srv.NewEnvironment(t, "Remote")
	f.connect(t, env, dockertest.FakeConfig{Hostname: "edge-1", Version: "28.4.0"}, nil)
	if e := f.whenState(t, env.ID, "online"); e.Engine.Version != "28.4.0" {
		t.Fatalf("the engine read through the tunnel is %q, want 28.4.0 after the first probe", e.Engine.Version)
	}
	connectedAt := f.tclk.Now()

	o := overviewOf(t, f.envFix, env.ID)
	if o.EnvType == nil || *o.EnvType != "standalone" || o.IsLocalSocket {
		t.Fatalf("envType = %v, isLocalSocket = %v, want standalone and false", o.EnvType, o.IsLocalSocket)
	}
	counts(t, "nodes", o.Nodes, 1, 1)
	if o.Agents != nil || o.FailingServices != nil {
		t.Fatalf("agents = %+v, failingServices = %+v, want null for both on a standalone host with no service data", o.Agents, o.FailingServices)
	}
	if o.AgentVersion == nil || *o.AgentVersion != "0.2.0" || o.UpdateAvailable {
		t.Fatalf("agentVersion = %v, updateAvailable = %v, want 0.2.0 and false for an agent that matches the server", o.AgentVersion, o.UpdateAvailable)
	}
	if o.EngineVersion == nil || *o.EngineVersion != "28.4.0" {
		t.Fatalf("engineVersion = %v, want the version its target reported", o.EngineVersion)
	}
	if o.LastSeen == nil || *o.LastSeen != utc(connectedAt) {
		t.Fatalf("lastSeen = %v, want the hub's last sign of life %s", o.LastSeen, utc(connectedAt))
	}
	raw := rawEnvironment(t, f.req(t, http.MethodGet, "/api/environments", "").body, env.ID)
	for _, k := range []string{"agents", "failingServices"} {
		if string(raw[k]) != "null" {
			t.Errorf("%s = %s, want the key present with null", k, raw[k])
		}
	}
	for _, k := range []string{"updates", "pools", "addressPools"} {
		if _, ok := raw[k]; ok {
			t.Errorf("the list carries a %q column: D-32 leaves placeholder columns out", k)
		}
	}

	// The agent goes quiet: still a node inside the 30 s window, gone after it, and
	// the last sign of life stays the time it was last heard.
	f.srv.Events.WaitLen(t, 1)
	f.tclk.Advance(31 * time.Second)
	f.srv.Events.WaitLen(t, 2)
	o = overviewOf(t, f.envFix, env.ID)
	counts(t, "nodes after the agent was lost", o.Nodes, 0, 1)
	if o.LastSeen == nil || *o.LastSeen != utc(connectedAt) {
		t.Fatalf("lastSeen = %v after the loss, want it unchanged at %s", o.LastSeen, utc(connectedAt))
	}
}

func TestUpdateAvailableIsTrueWhenTheAgentIsOlderThanTheServerByAMinorOrAPatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		server, agent string
		want          bool
	}{
		{"v0.3.1", "0.3.0", true},
		{"v0.3.0", "0.2.9", true},
		{"v0.3.0", "0.3.0", false},
		{"v0.3.0", "0.3.1", false},
		{"v0.3.0", "0.4.0", false},
		{"v0.3.0-rc.1", "0.3.0", false},
		{"edge", "0.3.0", false},
		{"v0.3.0", "dev (abc123)", false},
	} {
		t.Run(c.server+" with agent "+c.agent, func(t *testing.T) {
			f := newAgentFix(t)
			f.svc.WithHealth(envs.HealthConfig{ServerVersion: c.server})
			env := f.srv.NewEnvironment(t, "Remote")
			conn := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, Version: c.agent})
			if conn.Welcome == nil {
				t.Skipf("the hub refuses this pair (%+v), so there is no list row to read", conn.Refuse)
			}
			f.srv.Events.WaitLen(t, 1)
			o := overviewOf(t, f.envFix, env.ID)
			if o.AgentVersion == nil || *o.AgentVersion != c.agent || o.UpdateAvailable != c.want {
				t.Fatalf("agentVersion = %v, updateAvailable = %v, want %s and %v", o.AgentVersion, o.UpdateAvailable, c.agent, c.want)
			}
		})
	}
}

func TestOverviewOfAPendingEnvironmentIsNullWhereNothingIsKnown(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Waiting")
	o := overviewOf(t, f.envFix, env.ID)
	if o.Nodes != nil || o.FailingServices != nil || o.Agents != nil || o.AgentVersion != nil || o.EngineVersion != nil || o.LastSeen != nil || o.EnvType != nil {
		t.Fatalf("overview = %+v, want null for every unknown value", o)
	}
	if o.UpdateAvailable || o.IsLocalSocket {
		t.Fatalf("overview = %+v, want updateAvailable and isLocalSocket false", o)
	}
	raw := rawEnvironment(t, f.req(t, http.MethodGet, "/api/environments", "").body, env.ID)
	for _, k := range []string{"nodes", "failingServices", "agents", "agentVersion"} {
		if string(raw[k]) != "null" {
			t.Errorf("%s = %s, want the key present with null", k, raw[k])
		}
	}
}

func TestOverviewOfASwarmCountsNodesAndAgentsSeparatelyAndTakesTheOldestAgentVersion(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	f.svc.WithHealth(envs.HealthConfig{ServerVersion: "v0.3.0"})
	env := f.srv.NewEnvironment(t, "Swarm")
	manager := &wire.Facts{
		EngineID: "ENG:m", Hostname: "manager-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-m", ClusterID: "cluster-1", Control: true},
	}
	worker := &wire.Facts{
		EngineID: "ENG:w", Hostname: "worker-1", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-w", Control: false},
	}
	fm := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "manager-1", SwarmState: "active", SwarmControl: true})
	fw := dockertest.NewFake(t, dockertest.FakeConfig{Hostname: "worker-1", SwarmState: "active"})
	cm := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: manager, Version: "0.3.0"})
	cw := f.srv.Dial(t, tunneltest.Agent{Token: env.Token, Facts: worker, Version: "0.2.9"})
	if cm.Welcome == nil || cw.Welcome == nil {
		t.Fatalf("enrolment refused: %+v / %+v", cm.Refuse, cw.Refuse)
	}
	cm.ProxyDocker(t, fm.SocketPath())
	cw.ProxyDocker(t, fw.SocketPath())
	mc, wc := cm.OpenControl(t), cw.OpenControl(t)
	tunneltest.SendHeartbeat(t, mc, true, f.tclk.Now().UnixMilli())
	tunneltest.SendHeartbeat(t, wc, true, f.tclk.Now().UnixMilli())
	f.srv.Events.WaitLen(t, 2)
	// The overview counts the nodes the monitor has probed, so both agents are ready (as
	// connect leaves them) before it is read once.
	for _, c := range []*tunneltest.Conn{cm, cw} {
		f.awaitFirstHeartbeat(t, c)
		awaitFirstProbe(t, f.mon, envs.AgentTarget(c.Identity.AgentID))
	}
	f.whenState(t, env.ID, "online")

	o := overviewOf(t, f.envFix, env.ID)
	if o.EnvType == nil || *o.EnvType != "swarm" {
		t.Fatalf("envType = %v, want swarm", o.EnvType)
	}
	counts(t, "nodes", o.Nodes, 2, 2)
	if o.Agents == nil || o.Agents.Connected != 2 || o.Agents.Expected != 2 {
		t.Fatalf("agents = %+v, want 2 connected of 2 expected", o.Agents)
	}
	if o.AgentVersion == nil || *o.AgentVersion != "0.2.9" || !o.UpdateAvailable {
		t.Fatalf("agentVersion = %v, updateAvailable = %v, want the oldest agent's 0.2.9 and an update", o.AgentVersion, o.UpdateAvailable)
	}

	// The worker is lost: the manager keeps the environment up, and the counts show it.
	f.tclk.Advance(20 * time.Second)
	tunneltest.SendHeartbeat(t, mc, true, f.tclk.Now().UnixMilli())
	var mid string
	for _, s := range f.srv.Events.Sessions() {
		if s.Facts.Swarm != nil && s.Facts.Swarm.Control {
			mid = s.AgentID
		}
	}
	awaitHeartbeat(t, f.srv.Hub.Session(mid), func(st tunnel.SessionStatus) bool { return st.LastHeartbeat.Equal(f.tclk.Now()) })
	f.tclk.Advance(11 * time.Second)
	f.srv.Events.WaitLen(t, 3)
	o = overviewOf(t, f.envFix, env.ID)
	counts(t, "nodes after the worker was lost", o.Nodes, 1, 2)
	if o.Agents == nil || o.Agents.Connected != 1 || o.Agents.Expected != 2 {
		t.Fatalf("agents = %+v, want 1 connected of 2 expected", o.Agents)
	}
}

// awaitFirstProbe blocks until the monitor of a just-added local environment has
// finished its first probe. Adding starts the monitor's loop in the background and
// the list reads the monitor's status without waiting: an engine nobody has asked
// yet is "not known" (an empty envType, no nodes, no engine version), by design.
// A test that reads the overview straight after adding would race that probe, so
// it waits on the monitor's own first-probe signal rather than on a clock.
func (f *envFix) awaitFirstProbe(t *testing.T, id string) {
	t.Helper()
	ctx := testsupport.Context(t)
	f.mon.AwaitProbe(ctx, id)
	if st := f.mon.Status(id); st.State == envs.StateUnknown {
		t.Fatalf("the monitor of %s has no first probe result: %v", id, ctx.Err())
	}
}

func TestOverviewOfTheLocalEnvironmentIsTheLocalSocketWithOneNodeAndNoAgent(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	e := f.add(t)
	f.awaitFirstProbe(t, e.ID)
	o := overviewOf(t, f, e.ID)
	if !o.IsLocalSocket || o.EnvType == nil || *o.EnvType != "standalone" {
		t.Fatalf("overview = %+v, want the local socket of a standalone engine", o)
	}
	counts(t, "nodes", o.Nodes, 1, 1)
	if o.AgentVersion != nil || o.Agents != nil || o.UpdateAvailable {
		t.Fatalf("overview = %+v, want no agent version, agents or update for the local socket", o)
	}
	if o.EngineVersion == nil || *o.EngineVersion != "29.1.0" {
		t.Fatalf("engineVersion = %v, want 29.1.0", o.EngineVersion)
	}
}

func TestOverviewOfALocalSwarmManagerDoesNotGuessItsNodeCount(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", SwarmState: "active", SwarmControl: true})
	e := f.add(t)
	f.awaitFirstProbe(t, e.ID)
	o := overviewOf(t, f, e.ID)
	if o.EnvType == nil || *o.EnvType != "swarm" || !o.IsLocalSocket {
		t.Fatalf("overview = %+v, want the local socket of a swarm", o)
	}
	if o.Nodes != nil {
		t.Fatalf("nodes = %+v, want null: the cluster's node count is not read here", o.Nodes)
	}
}

// The agent's own BAYPLAN_AGENT_ALLOW_INSECURE is in the install texts only while
// the server's switch is on and the address is plain ws:// (T-02-31). A production
// install, wss:// or a server without the switch, never carries it.
func TestEnvironmentAddAgentCarriesTheAgentSwitchOnlyWhileTheServerSwitchIsOn(t *testing.T) {
	t.Parallel()
	const name = "BAYPLAN_AGENT_ALLOW_INSECURE"
	cases := []struct {
		label string
		allow bool
		url   string
		want  bool
	}{
		{"switch off, http agent URL on this machine", false, "http://127.0.0.1:7480", false},
		{"switch on, http agent URL", true, "http://10.0.0.5:7480", true},
		{"switch on, https agent URL", true, "https://bayplan.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			f := newEnvFix(t, dockertest.FakeConfig{})
			f.agentAllowInsecure = tc.allow
			f.h = f.build(t)
			f.inst.ApplyAgentURL(tc.url)
			got, rec := f.addAgent(t)
			if rec.code != http.StatusCreated || got.Install == nil {
				t.Fatalf("status = %d: %s", rec.code, rec.body)
			}
			for label, text := range map[string]string{
				"swarm command": got.Install.Swarm.Command, "stack file": got.Install.Swarm.StackFile,
				"standalone command": got.Install.Standalone.Command, "compose file": got.Install.Standalone.ComposeFile,
			} {
				if has := strings.Contains(text, name); has != tc.want {
					t.Errorf("%s carries the agent switch = %v, want %v:\n%s", label, has, tc.want, text)
				}
			}
		})
	}
}
