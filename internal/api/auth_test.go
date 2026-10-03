// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/agents"
	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/datadir"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Sign-in, sign-out and /api/me (plan 12, AUTH-02, AUTH-03, AUTH-08). Negative
// cases are mandatory (D-81).

const (
	authUA     = "auth-test/1.0"
	authIP     = "203.0.113.9"
	authIdle   = 12 * time.Hour
	authRemain = 30 * 24 * time.Hour
)

var authHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(goodPassword)
	if err != nil {
		panic(err)
	}
	return h
})

type authFixture struct {
	h       http.Handler
	st      *store.Store
	v       *vault.Vault
	aud     *audit.Writer
	sess    *auth.Sessions
	inst    *instance.Instance
	clk     *fakeClock
	logs    *bytes.Buffer
	limiter *auth.IPLimiter
	// dataInfo and acctVault feed the account routes (plan 14). A test sets them
	// before calling build; the defaults are a persisted directory and the
	// environment-keyed vault.
	dataInfo  datadir.Info
	acctVault *vault.Vault
	// pwLimiter is the per-user throttle on POST /api/me/password; nil means none.
	pwLimiter *auth.IPLimiter
	// localHost feeds GET /api/local-host (plan 15).
	localHost api.LocalHostDeps
	// envSvc, when set, is the environment registry behind the environment
	// routes and the {envId} resolver (plan 16). Without it, no environment
	// exists and the environment routes are never called.
	envSvc *envs.Service
	// snaps, when set with envSvc, is the snapshot cache behind the summary and
	// list routes (plan 18); agg is the aggregator over it that the routes read
	// through (plan 02-11).
	snaps *envs.Snapshots
	agg   *envs.Aggregator
	// hub, when set with envSvc, is the events hub behind the events routes
	// (plan 21); eventsHeartbeat and eventsMaxStreams tune them for a test.
	hub              *envs.Hub
	eventsHeartbeat  time.Duration
	eventsMaxStreams int
	// eventsMaxLifetime caps how long one stream may live; zero is the default.
	eventsMaxLifetime time.Duration
	// brk, when set with envSvc, is the health broker behind GET
	// /api/environments/stream (plan 02-12); envStreamHeartbeat, envStreamMaxStreams
	// and envStreamMaxLifetime tune that stream for a test.
	brk                  *envs.Broker
	envStreamHeartbeat   time.Duration
	envStreamMaxStreams  int
	envStreamMaxLifetime time.Duration
	// auditRetention feeds GET /api/audit (plan 24).
	auditRetention int
	// tlsPin is the pin Bayplan's own self-signed certificate would have; empty
	// means it serves none (plan 02-09).
	tlsPin string
	// agentSvc and liveAgents, when set, replace the agents service the agent routes
	// use and say which tunnel hub the node list reads live state from (plan 02-15).
	// A test with a real hub sets them to the tunneltest server's, so a revoke closes
	// a real session.
	agentSvc   *agents.Service
	liveAgents api.AgentSessions
}

func newAuthFixture(t *testing.T, cfg config.Config) *authFixture {
	t.Helper()
	f := &authFixture{st: storetest.New(t), clk: &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	v, err := vault.LoadOrCreate(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f.v = v
	f.acctVault = v
	f.dataInfo = datadir.Info{Path: t.TempDir(), Persisted: true}
	f.aud = audit.NewWriter(f.st, v, log, f.clk.now)
	if err := rbac.SeedAdminRole(context.Background(), f.st); err != nil {
		t.Fatal(err)
	}
	if f.inst, err = instance.Load(context.Background(), f.st, cfg); err != nil {
		t.Fatal(err)
	}
	f.sess = auth.NewSessions(f.st, v, authIdle, authRemain, f.clk.now)
	f.limiter = auth.NewIPLimiter(time.Second, 1000, 10*time.Minute, f.clk.now)
	f.addUser(t, "admin", true)
	f.h = f.build(t)
	return f
}

// build assembles the router from the fixture's long-lived parts, the way
// server.Build does. Calling it again is a restart: everything in memory is
// new, everything in the database is kept.
func (f *authFixture) build(t *testing.T) http.Handler {
	t.Helper()
	log := slog.New(slog.NewJSONHandler(f.logs, nil))
	login := auth.NewLogin(f.st, f.sess, f.aud, 5, 30*time.Minute, f.limiter, log, f.clk.now)
	checker := rbac.NewChecker(f.st)
	opts := httpx.Options{
		Checker:       checker,
		Authenticator: auth.NewAuthenticator(f.st, authIdle, f.inst.CookieScope, f.clk.now),
		Auditor:       f.aud,
		MutationGuard: auth.NewCSRFGuard(f.sess),
		PublicOrigin:  f.inst.PublicOrigin,
		Logger:        log,
	}
	if f.envSvc != nil {
		opts.EnvResolver = f.envSvc
	}
	r := httpx.New(opts)
	api.RegisterAuth(r, api.AuthDeps{Login: login, Sessions: f.sess, Instance: f.inst, Store: f.st, Checker: checker, Log: log})
	api.RegisterAccount(r, api.AccountDeps{
		Sessions: f.sess, Instance: f.inst, Store: f.st, Audit: f.aud,
		DataDir: f.dataInfo, Vault: f.acctVault, Limiter: f.pwLimiter, Log: log,
	})
	api.RegisterSettings(r, api.SettingsDeps{Instance: f.inst, Store: f.st, Audit: f.aud, Log: log})
	api.RegisterLocalHost(r, f.localHost)
	api.RegisterEnvironments(r, api.EnvironmentsDeps{
		Envs: f.envSvc, Agents: agents.NewService(f.st, f.v, f.aud, f.clk.now), Instance: f.inst,
		TLSPin: func() string { return f.tlsPin }, Checker: checker, Log: log,
	})
	agentSvc := f.agentSvc
	if agentSvc == nil {
		agentSvc = agents.NewService(f.st, f.v, f.aud, f.clk.now)
	}
	api.RegisterAgents(r, api.AgentsDeps{
		Agents: agentSvc, Live: f.liveAgents, Instance: f.inst, TLSPin: func() string { return f.tlsPin }, Log: log,
	})
	if f.envSvc != nil && f.agg != nil {
		api.RegisterDashboard(r, api.DashboardDeps{Aggregator: f.agg, SelfContainerID: f.envSvc.SelfContainerID(), Log: log})
	}
	if f.envSvc != nil && f.agg != nil && f.hub != nil {
		api.RegisterEvents(r, api.EventsDeps{Aggregator: f.agg, Heartbeat: f.eventsHeartbeat, MaxStreamsPerUser: f.eventsMaxStreams, MaxLifetime: f.eventsMaxLifetime, Log: log})
	}
	if f.envSvc != nil && f.brk != nil {
		api.RegisterEnvStream(r, api.EnvStreamDeps{
			Broker: f.brk, Checker: checker, Heartbeat: f.envStreamHeartbeat,
			MaxStreamsPerUser: f.envStreamMaxStreams, MaxLifetime: f.envStreamMaxLifetime, Log: log,
		})
	}
	api.RegisterRecent(r, api.RecentDeps{Store: f.st, Envs: f.envSvc, Checker: checker, Now: f.clk.now, Log: log})
	api.RegisterAudit(r, api.AuditDeps{Store: f.st, RetentionDays: f.auditRetention, Now: f.clk.now, Log: log})
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Fatal(err)
	}
	return r.Handler()
}

func (f *authFixture) addUser(t *testing.T, name string, admin bool) string {
	t.Helper()
	ctx := context.Background()
	id := "id-" + name
	n := f.clk.now().UnixNano()
	tx, err := f.st.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.st.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: id, Username: name, PasswordHash: authHash(), CreatedAt: n, UpdatedAt: n, PasswordChangedAt: n,
	}); err != nil {
		t.Fatal(err)
	}
	if admin {
		if err := rbac.AssignGlobalRole(ctx, tx, f.st.Q, rbac.PrincipalUser, id, rbac.AdminRoleID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

type authReq struct {
	method, path, body string
	cookie, csrf       string // cookie is the raw session ID
	ip                 string
}

func (f *authFixture) do(t *testing.T, h http.Handler, r authReq) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(r.method, r.path, body)
	req.Header.Set("User-Agent", authUA)
	if r.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = authIP + ":40000"
	if r.ip != "" {
		req.RemoteAddr = r.ip + ":40000"
	}
	if r.cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: r.cookie}) //nolint:gosec // a request cookie as a browser sends it
	}
	if r.csrf != "" {
		req.Header.Set("X-CSRF-Token", r.csrf)
	}
	if r.method != http.MethodGet {
		if origin := f.inst.PublicOrigin(); origin != "" {
			req.Header.Set("Origin", origin)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) call(t *testing.T, r authReq) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, f.h, r)
}

func loginBody(user, password string, remember bool) string {
	b, _ := json.Marshal(map[string]any{"username": user, "password": password, "remember": remember})
	return string(b)
}

type loginResp struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
	CSRFToken string `json:"csrfToken"`
}

// signIn signs admin in and returns the raw session ID and the CSRF token.
func (f *authFixture) signIn(t *testing.T, user string, remember bool) (cookie, csrf string) {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(user, goodPassword, remember)})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in as %s: %d %s", user, rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("sign-in set no session cookie")
	}
	var r loginResp
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return c.Value, r.CSRFToken
}

type auditEntry struct{ Action, ActorType, ActorID, TargetID, Result, Reason, IP, UserAgent string }

func (f *authFixture) audits(t *testing.T, action string) []auditEntry {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, actor_type, actor_id, target_id, result, reason, ip, user_agent
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditEntry
	for rows.Next() {
		var e auditEntry
		if err := rows.Scan(&e.Action, &e.ActorType, &e.ActorID, &e.TargetID, &e.Result, &e.Reason, &e.IP, &e.UserAgent); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *authFixture) sessionCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- POST /api/auth/login --------------------------------------------------

func TestLoginSignsInWithABrowserSessionCookieAndReturnsTheUserAndCSRFToken(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("Admin", goodPassword, false)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got loginResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.User.ID != "id-admin" || got.User.Username != "admin" || got.User.Role != "admin" || got.CSRFToken == "" {
		t.Fatalf("body = %+v", got)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "bayplan_session" || c.Value == "" {
		t.Fatalf("cookie = %+v", c)
	}
	if c.MaxAge != 0 || !c.Expires.IsZero() {
		t.Fatalf("a browser-session cookie must have no Max-Age or Expires: %+v", c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie flags = %+v", c)
	}
	if want := f.sess.CSRFToken(auth.HashSessionID(c.Value)); got.CSRFToken != want {
		t.Fatalf("csrfToken = %q, want the session's token %q", got.CSRFToken, want)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestLoginRememberMeSetsAThirtyDayCookieAndSession(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, true)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.MaxAge != 2592000 {
		t.Fatalf("cookie = %+v, want Max-Age 2592000", c)
	}
	row, err := f.st.RQ.GetSessionByHash(context.Background(), auth.HashSessionID(c.Value))
	if err != nil || row.Remember != 1 || !row.AbsoluteExpiresAt.Valid {
		t.Fatalf("session row = %+v, %v; want remembered with an absolute expiry", row, err)
	}
}

func TestLoginOverHTTPSUsesTheHostPrefixedSecureCookie(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://bayplan.example"})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Name != "__Host-bayplan_session" || !c.Secure || !c.HttpOnly || c.Domain != "" {
		t.Fatalf("cookie = %+v", c)
	}
}

func TestLoginUnknownUserWrongPasswordLockedAndDisabledAreByteIdentical(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "locked", false)
	f.addUser(t, "off", false)
	for range 5 {
		f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("locked", "wrong wrong wrong", false)})
	}
	if _, err := f.st.Write.Exec(`UPDATE users SET disabled = 1 WHERE username = 'off'`); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"unknown user":   loginBody("nobody", goodPassword, false),
		"wrong password": loginBody("admin", "wrong wrong wrong", false),
		"locked account": loginBody("locked", goodPassword, false), // the right password
		"disabled":       loginBody("off", goodPassword, false),
		"malformed name": loginBody("a b", goodPassword, false),
	}
	var first *httptest.ResponseRecorder
	for name, body := range cases {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: body})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401: %s", name, rec.Code, rec.Body)
		}
		if got := problemType(t, rec); got != "invalid_credentials" {
			t.Fatalf("%s: problem type = %q, want invalid_credentials", name, got)
		}
		var p struct{ Detail string }
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Detail != "Invalid username or password." {
			t.Fatalf("%s: detail = %q (%v)", name, p.Detail, err)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: a failed sign-in set a cookie", name)
		}
		if first == nil {
			first = rec
			continue
		}
		if rec.Body.String() != first.Body.String() {
			t.Fatalf("%s: body %q differs from %q (D-30)", name, rec.Body, first.Body)
		}
		if rec.Header().Get("Content-Type") != first.Header().Get("Content-Type") {
			t.Fatalf("%s: content type differs", name)
		}
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a failed sign-in created a session")
	}
}

func TestLoginWithAnExistingSessionRotatesIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	oldCookie, oldCSRF := f.signIn(t, "admin", false)

	rec := f.call(t, authReq{
		method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false),
		cookie: oldCookie, csrf: oldCSRF,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	fresh := sessionCookie(rec)
	if fresh == nil || fresh.Value == oldCookie {
		t.Fatalf("the session ID was not rotated: %+v", fresh)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: oldCookie}); got.Code != http.StatusUnauthorized {
		t.Fatalf("the old session after rotation: %d, want 401", got.Code)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: fresh.Value}); got.Code != http.StatusOK {
		t.Fatalf("the new session: %d, want 200", got.Code)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
}

func TestLoginWhileHoldingASessionStillNeedsItsCSRFToken(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{
		method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false), cookie: cookie,
	})
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
		t.Fatalf("status = %d, type %q: %s; want 403 csrf_failed (D-36: every session mutation carries the token)", rec.Code, problemType(t, rec), rec.Body)
	}
	if n := f.sessionCount(t); n != 1 {
		t.Fatalf("a refused sign-in changed the sessions: %d", n)
	}
}

func TestLoginIsRateLimitedPerAddressWithTheSameError(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.limiter = auth.NewIPLimiter(time.Hour, 2, time.Hour, f.clk.now)
	h := f.build(t)
	post := func(ip, pw string) *httptest.ResponseRecorder {
		return f.do(t, h, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", pw, false), ip: ip})
	}
	a := post("198.51.100.4", "wrong wrong wrong")
	post("198.51.100.4", "wrong wrong wrong")
	limited := post("198.51.100.4", goodPassword) // the right password, but the address is spent
	if limited.Code != http.StatusUnauthorized || limited.Body.String() != a.Body.String() {
		t.Fatalf("rate-limited answer = %d %s, want the same 401 body as a wrong password (%s)", limited.Code, limited.Body, a.Body)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a rate-limited attempt signed in")
	}
	if got := post("198.51.100.5", goodPassword); got.Code != http.StatusOK {
		t.Fatalf("another address: %d, want 200", got.Code)
	}
}

func TestLoginAndFailedLoginAreAuditedWithIPAndUserAgent(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", "wrong wrong wrong", false)})
	f.signIn(t, "admin", false)

	failed := f.audits(t, audit.ActionAuthLoginFailed)
	if len(failed) != 1 || failed[0].Reason != "bad_password" || failed[0].IP != authIP || failed[0].UserAgent != authUA || failed[0].ActorType != "anonymous" {
		t.Fatalf("auth.login_failed = %+v", failed)
	}
	ok := f.audits(t, audit.ActionAuthLogin)
	if len(ok) != 1 || ok[0].ActorID != "id-admin" || ok[0].IP != authIP || ok[0].UserAgent != authUA || ok[0].Result != "success" {
		t.Fatalf("auth.login = %+v", ok)
	}
}

func TestLoginFiveWrongPasswordsLockTheAccountAndAuditIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	for range 5 {
		f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", "wrong wrong wrong", false)})
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("right password while locked: %d, want 401", rec.Code)
	}
	if got := f.audits(t, audit.ActionAuthLocked); len(got) != 1 {
		t.Fatalf("auth.locked entries = %d, want 1", len(got))
	}
	f.clk.t = f.clk.t.Add(30*time.Minute + time.Second)
	f.signIn(t, "admin", false) // the lock expired on its own
}

func TestLoginNeverEchoesOrLogsThePassword(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	const secret = "an-attempted-secret-9876"
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", secret, false)})
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(f.logs.String(), secret) {
		t.Fatal("an attempted password was echoed or logged")
	}
	good := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", goodPassword, false)})
	if strings.Contains(good.Body.String(), goodPassword) || strings.Contains(f.logs.String(), goodPassword) {
		t.Fatal("the password was echoed or logged")
	}
}

// A password typed into the username box is stored nowhere (CR-01, D-39,
// AUTH-08): not in the table, the list, either export or the container log. The
// third string is a valid-looking name, so only "no account matched" can keep it
// out.
func TestLoginWithAPasswordTypedAsTheUsernameLeavesNoTraceInTheAuditLogItsExportsOrTheLog(t *testing.T) {
	f := newAuditFix(t)
	typed := []string{"Tr0ub4dor&3 hunter", "correct horse battery staple!", "hunter2hunter2"}
	for _, name := range typed {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(name, "wrong wrong wrong", false)})
		if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_credentials" {
			t.Fatalf("username %q: status = %d, type %q: %s; want the one 401 invalid_credentials", name, rec.Code, problemType(t, rec), rec.Body)
		}
	}

	bodies := map[string]string{}
	for label, path := range map[string]string{
		"list":        "/api/audit?action=auth.login_failed",
		"export csv":  "/api/audit/export?format=csv",
		"export json": "/api/audit/export?format=json",
	} {
		rec := f.get(t, path, f.cookie)
		if rec.code != http.StatusOK {
			t.Fatalf("GET %s: status = %d: %s", path, rec.code, rec.body)
		}
		bodies[label] = rec.body
	}
	bodies["container log"] = f.logs.String()
	for _, name := range typed {
		for label, body := range bodies {
			if strings.Contains(body, name) {
				t.Errorf("the typed string %q appears in the %s", name, label)
			}
		}
		var hits int
		if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log
			WHERE target_id LIKE ?1 OR details LIKE ?1 OR reason LIKE ?1 OR actor_name LIKE ?1 OR actor_id LIKE ?1`,
			"%"+name+"%").Scan(&hits); err != nil {
			t.Fatal(err)
		}
		if hits != 0 {
			t.Errorf("the typed string %q is in %d audit_log rows", name, hits)
		}
	}

	failed := f.audits(t, audit.ActionAuthLoginFailed)
	if len(failed) != len(typed) {
		t.Fatalf("auth.login_failed entries = %d, want %d", len(failed), len(typed))
	}
	for _, e := range failed {
		if e.TargetID != "(unknown user)" || e.Reason != "unknown_user" {
			t.Errorf("entry = %+v, want target (unknown user) and reason unknown_user", e)
		}
	}
}

func TestLoginRejectsAnOversizedBodyFieldsWithoutTouchingTheAccount(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody(strings.Repeat("x", 65), goodPassword, false)})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 65-character username: %d, want 422", rec.Code)
	}
	rec = f.call(t, authReq{method: http.MethodPost, path: "/api/auth/login", body: loginBody("admin", strings.Repeat("x", 1025), false)})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 1025-character password: %d, want 422", rec.Code)
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("an oversized sign-in created a session")
	}
}

// --- GET /api/me -----------------------------------------------------------

type meResp struct {
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
	CSRFToken   string `json:"csrfToken"`
	Permissions struct {
		Global       []string            `json:"global"`
		Environments map[string][]string `json:"environments"`
	} `json:"permissions"`
}

func TestMeReturnsTheUserCSRFTokenAndEffectivePermissions(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got meResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.User.ID != "id-admin" || got.User.Username != "admin" || got.User.Role != "admin" || got.CSRFToken != csrf {
		t.Fatalf("body = %+v, want the user and the same CSRF token the sign-in returned", got)
	}

	var wantGlobal, wantEnv []string
	for _, p := range rbac.All() {
		if p.Scope == rbac.ScopeGlobal {
			wantGlobal = append(wantGlobal, string(p.Name))
		} else {
			wantEnv = append(wantEnv, string(p.Name))
		}
	}
	slices.Sort(wantGlobal)
	slices.Sort(wantEnv)
	if !slices.Equal(got.Permissions.Global, wantGlobal) {
		t.Fatalf("global permissions = %v, want %v", got.Permissions.Global, wantGlobal)
	}
	if !slices.Equal(got.Permissions.Environments["*"], wantEnv) || len(got.Permissions.Environments) != 1 {
		t.Fatalf("environment permissions = %v, want only %q: %v", got.Permissions.Environments, "*", wantEnv)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestMeForAUserWithNoRoleHasEmptyPermissionListsNotNull(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "plain", false)
	cookie, _ := f.signIn(t, "plain", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"global":[]`) || !strings.Contains(rec.Body.String(), `"environments":{}`) {
		t.Fatalf("body = %s, want empty lists, not null", rec.Body)
	}
	var got meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.User.Role != "" {
		t.Fatalf("role = %q, want empty", got.User.Role)
	}
}

func TestMeWithoutASessionIs401AndAuditedAsADeniedRead(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 1 || denied[0].ActorType != "anonymous" || denied[0].Result != "denied" || denied[0].TargetID != "GET /api/me" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
}

func TestMeWithAGarbageOrForeignCookieIs401(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	for _, v := range []string{"garbage", strings.Repeat("A", 43), strings.Repeat("A", 5000)} {
		if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: v}); rec.Code != http.StatusUnauthorized {
			t.Errorf("cookie %.12q: %d, want 401", v, rec.Code)
		}
	}
}

func TestAnHTTPSInstanceIgnoresABareSessionCookie(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://bayplan.example"})
	raw, _ := f.signIn(t, "admin", false)
	me := func(name string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: name, Value: raw}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := me("bayplan_session"); got != http.StatusUnauthorized {
		t.Fatalf("a bare cookie on https: %d, want 401 (WR-01)", got)
	}
	if got := me("__Host-bayplan_session"); got != http.StatusOK {
		t.Fatalf("the __Host- cookie on https: %d, want 200", got)
	}
}

func TestAPlainHTTPInstanceIgnoresAHostPrefixedCookie(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	raw, _ := f.signIn(t, "admin", false)
	me := func(name string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.RemoteAddr = authIP + ":40000"
		req.AddCookie(&http.Cookie{Name: name, Value: raw}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := me("__Host-bayplan_session"); got != http.StatusUnauthorized {
		t.Fatalf("a __Host- cookie on plain http: %d, want 401", got)
	}
	if got := me("bayplan_session"); got != http.StatusOK {
		t.Fatalf("the bare cookie on plain http: %d, want 200", got)
	}
}

func TestMeSessionSurvivesAServerRestart(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	restarted := f.build(t) // new Authenticator and Login, same database
	rec := f.do(t, restarted, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("after a restart: %d, want 200 (D-32: sessions live in SQLite)", rec.Code)
	}
	var got meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.CSRFToken != csrf {
		t.Fatal("the CSRF token changed across a restart")
	}
}

func TestMeSessionEndsAfterTwelveHoursIdleButARememberedOneDoesNot(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	plain, _ := f.signIn(t, "admin", false)
	kept, _ := f.signIn(t, "admin", true)

	f.clk.t = f.clk.t.Add(authIdle + time.Second)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: plain}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser session idle 12h+1s: %d, want 401", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: kept}); rec.Code != http.StatusOK {
		t.Fatalf("remembered session idle 12h+1s: %d, want 200 (D-31)", rec.Code)
	}
	f.clk.t = f.clk.t.Add(authRemain)
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: kept}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("remembered session past 30 days: %d, want 401", rec.Code)
	}
}

// --- POST /api/auth/logout -------------------------------------------------

func TestLogoutEndsOnlyThisDevicesSessionClearsTheCookieAndIsAudited(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	phone, _ := f.signIn(t, "admin", false)
	laptop, laptopCSRF := f.signIn(t, "admin", true)

	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: laptop, csrf: laptopCSRF})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	c := sessionCookie(rec)
	if c == nil || c.Value != "" || c.MaxAge >= 0 || !c.HttpOnly {
		t.Fatalf("the cookie was not cleared: %+v", c)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: laptop}); got.Code != http.StatusUnauthorized {
		t.Fatalf("the signed-out session: %d, want 401", got.Code)
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: phone}); got.Code != http.StatusOK {
		t.Fatalf("the other device after logout: %d, want 200 (D-33)", got.Code)
	}
	out := f.audits(t, audit.ActionAuthLogout)
	if len(out) != 1 || out[0].ActorID != "id-admin" || out[0].IP != authIP || out[0].UserAgent != authUA {
		t.Fatalf("auth.logout = %+v", out)
	}
}

func TestLogoutWithoutAValidCSRFTokenIs403AndKeepsTheSession(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	for name, token := range map[string]string{"missing": "", "wrong": "not-the-token", "truncated": csrf[:8]} {
		rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: cookie, csrf: token})
		if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
			t.Fatalf("%s token: %d %q, want 403 csrf_failed", name, rec.Code, problemType(t, rec))
		}
	}
	if got := f.call(t, authReq{method: http.MethodGet, path: "/api/me", cookie: cookie}); got.Code != http.StatusOK {
		t.Fatalf("the session after refused logouts: %d, want 200", got.Code)
	}
	denied := f.audits(t, audit.ActionPermissionDenied)
	if len(denied) != 3 || denied[0].Reason != "csrf" || denied[0].ActorID != "id-admin" {
		t.Fatalf("csrf denials = %+v, want 3 audited with reason csrf", denied)
	}
	if len(f.audits(t, audit.ActionAuthLogout)) != 0 {
		t.Fatal("a refused logout was audited as a logout")
	}
}

func TestLogoutAnotherSessionsTokenIsRefused(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	a, _ := f.signIn(t, "admin", false)
	_, bCSRF := f.signIn(t, "admin", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: a, csrf: bCSRF})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a token from another session: %d, want 403", rec.Code)
	}
}

func TestLogoutWithoutASessionOrWithAnExpiredOneIs401(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d, want 401", rec.Code)
	}
	cookie, csrf := f.signIn(t, "admin", false)
	f.clk.t = f.clk.t.Add(authIdle + time.Minute)
	if rec := f.call(t, authReq{method: http.MethodPost, path: "/api/auth/logout", cookie: cookie, csrf: csrf}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an expired session: %d, want 401", rec.Code)
	}
}

func TestMutationFromAnotherOriginIsRefusedBeforeLogin(t *testing.T) {
	f := newAuthFixture(t, config.Config{PublicURL: "https://bayplan.example"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(loginBody("admin", goodPassword, false)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "origin_mismatch" {
		t.Fatalf("cross-origin login: %d %q, want 403 origin_mismatch", rec.Code, problemType(t, rec))
	}
	if f.sessionCount(t) != 0 {
		t.Fatal("a cross-origin login created a session")
	}
}
