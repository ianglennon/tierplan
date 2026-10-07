// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
)

const publicOrigin = "https://bayplan.example"

// fakeAuth maps the X-Test-User header to a principal: empty is anonymous.
type fakeAuth struct{}

func (fakeAuth) Authenticate(r *http.Request) (*rbac.Principal, error) {
	switch id := r.Header.Get("X-Test-User"); id {
	case "":
		return nil, nil
	case "boom":
		return nil, errors.New("session store down")
	case "janitor":
		return rbac.System("system:janitor"), nil
	default:
		return &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id, SessionIDHash: []byte("h")}, nil
	}
}

type fakeEnvs map[string]Env

func (f fakeEnvs) Resolve(_ context.Context, id string) (Env, error) {
	e, ok := f[id]
	switch {
	case !ok:
		return Env{}, ErrEnvNotFound
	case !e.Enabled:
		return e, ErrEnvDisabled
	}
	return e, nil
}

// captureAuditor records entries in memory; with err set every Record fails.
type captureAuditor struct {
	mu      sync.Mutex
	entries []audit.Entry
	err     error
}

func (c *captureAuditor) Record(_ context.Context, e audit.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.entries = append(c.entries, e)
	return nil
}

func (c *captureAuditor) all() []audit.Entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]audit.Entry(nil), c.entries...)
}

type echoOut struct {
	Body struct {
		Principal string `json:"principal"`
		Env       string `json:"env"`
		IP        string `json:"ip"`
	}
}

func echo(ctx context.Context, _ *struct{}) (*echoOut, error) {
	out := &echoOut{}
	if p := PrincipalFrom(ctx); p != nil {
		out.Body.Principal = p.ID
	}
	if e := EnvFrom(ctx); e != nil {
		out.Body.Env = e.ID
	}
	out.Body.IP = ClientIP(ctx)
	return out, nil
}

type envIn struct {
	EnvID string `path:"envId"`
}

func echoEnv(ctx context.Context, _ *envIn) (*echoOut, error) { return echo(ctx, nil) }

type fixture struct {
	r     *Router
	s     *store.Store
	audit *captureAuditor
}

// newFixture builds a router over a real store with "admin-user" an instance
// admin. The Auditor is a capturing fake unless real is set.
func newFixture(t *testing.T, tweak func(*Options)) *fixture {
	t.Helper()
	s := storetest.New(t)
	ctx := context.Background()
	tx, err := s.Write.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The grant needs a real user row: an ID with no row is an inactive user.
	if err := s.Q.WithTx(tx).InsertUser(ctx, sqlc.InsertUserParams{
		ID: "admin-user", Username: "admin-user", PasswordHash: "x", CreatedAt: 1, UpdatedAt: 1, PasswordChangedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rbac.GrantInstanceAdminTx(ctx, tx, s.Q, "admin-user"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	f := &fixture{s: s, audit: &captureAuditor{}}
	opts := Options{
		Checker:       rbac.NewChecker(s),
		Authenticator: fakeAuth{},
		EnvResolver: fakeEnvs{
			"env-on":  {ID: "env-on", Name: "On", Enabled: true},
			"env-off": {ID: "env-off", Name: "Off", Enabled: false},
		},
		Auditor:      f.audit,
		PublicOrigin: func() string { return publicOrigin },
	}
	if tweak != nil {
		tweak(&opts)
	}
	f.r = New(opts)

	Register(f.r, huma.Operation{OperationID: "readAudit", Method: http.MethodGet, Path: "/api/audit"},
		rbac.Require(rbac.PermAuditRead), echo)
	Register(f.r, huma.Operation{OperationID: "getMe", Method: http.MethodGet, Path: "/api/me"},
		rbac.SelfService, echo)
	Register(f.r, huma.Operation{OperationID: "getPublic", Method: http.MethodGet, Path: "/api/public"},
		rbac.Public, echo)
	Register(f.r, huma.Operation{OperationID: "postPublic", Method: http.MethodPost, Path: "/api/public"},
		rbac.Public, echo)
	Register(f.r, huma.Operation{OperationID: "writeSettings", Method: http.MethodPut, Path: "/api/settings"},
		rbac.Require(rbac.PermSettingsWrite), echo)
	Register(f.r, huma.Operation{OperationID: "listContainers", Method: http.MethodGet, Path: "/api/env/{envId}/containers"},
		rbac.Require(rbac.PermContainersRead), echoEnv)
	f.r.Guarded(http.MethodGet, "/api/stream", rbac.Require(rbac.PermAuditRead),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("stream")) }))
	f.r.Exempt(http.MethodGet, "/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	return f
}

func do(h http.Handler, method, path string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("User-Agent", "policy-test")
	for _, m := range mods {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func as(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("X-Test-User", id) }
}

func header(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func problemType(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem document: %v; %s", err, rec.Body)
	}
	return p.Type
}

func one(t *testing.T, c *captureAuditor) audit.Entry {
	t.Helper()
	got := c.all()
	if len(got) != 1 {
		t.Fatalf("audit entries = %d (%+v), want exactly 1", len(got), got)
	}
	return got[0]
}

func TestAnonymousReadOfAProtectedRouteIs401AndAudited(t *testing.T) {
	f := newFixture(t, nil)
	rec := do(f.r.Handler(), http.MethodGet, "/api/audit")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; %s", rec.Code, rec.Body)
	}
	e := one(t, f.audit)
	if e.Action != audit.ActionPermissionDenied || e.Result != audit.ResultDenied {
		t.Errorf("entry = %+v, want permission.denied / denied", e)
	}
	if e.Actor.Type != audit.ActorAnonymous {
		t.Errorf("actor = %+v, want anonymous", e.Actor)
	}
	if e.TargetType != "route" || e.TargetID != "GET /api/audit" {
		t.Errorf("target = %q %q, want route / GET /api/audit", e.TargetType, e.TargetID)
	}
	if e.Reason != "unauthenticated" {
		t.Errorf("reason = %q, want unauthenticated", e.Reason)
	}
	if e.IP != "192.0.2.1" || e.UserAgent != "policy-test" {
		t.Errorf("ip/ua = %q %q, want 192.0.2.1 / policy-test", e.IP, e.UserAgent)
	}
}

func TestUserWithoutThePermissionGets403AndIsAudited(t *testing.T) {
	f := newFixture(t, nil)
	rec := do(f.r.Handler(), http.MethodGet, "/api/audit", as("plain-user"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; %s", rec.Code, rec.Body)
	}
	e := one(t, f.audit)
	if e.Actor.Type != audit.ActorUser || e.Actor.ID != "plain-user" || e.Reason != "forbidden" {
		t.Errorf("entry = %+v, want the user as actor with reason forbidden", e)
	}
}

func TestAdminReachesTheHandlerAndAnAllowedReadIsNotAudited(t *testing.T) {
	f := newFixture(t, nil)
	rec := do(f.r.Handler(), http.MethodGet, "/api/audit", as("admin-user"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"principal":"admin-user"`) {
		t.Errorf("the handler did not see the principal: %s", rec.Body)
	}
	if n := len(f.audit.all()); n != 0 {
		t.Errorf("allowed read wrote %d audit entries, want 0", n)
	}
}

func TestSelfServiceNeedsAnyPrincipalAndPublicNeedsNone(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	if rec := do(h, http.MethodGet, "/api/me"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous SelfService = %d, want 401", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/me", as("plain-user")); rec.Code != http.StatusOK {
		t.Errorf("signed-in SelfService = %d, want 200", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/public"); rec.Code != http.StatusOK {
		t.Errorf("anonymous Public = %d, want 200", rec.Code)
	}
}

func TestEnvironmentRoutesAnswer404ForUnknownAnd409ForDisabled(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()

	rec := do(h, http.MethodGet, "/api/env/nope/containers", as("admin-user"))
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
		t.Errorf("unknown env = %d %s, want 404 environment_not_found", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodGet, "/api/env/env-off/containers", as("admin-user"))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "environment_disabled" {
		t.Errorf("disabled env = %d %s, want 409 environment_disabled", rec.Code, rec.Body)
	}
	rec = do(h, http.MethodGet, "/api/env/env-on/containers", as("admin-user"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"env":"env-on"`) {
		t.Errorf("valid env = %d %s, want 200 with EnvFrom set", rec.Code, rec.Body)
	}
}

func TestTheEnvironmentIsResolvedBeforeThePermissionIsChecked(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	// A signed-in user without the permission still gets the environment answer.
	if rec := do(h, http.MethodGet, "/api/env/nope/containers", as("plain-user")); rec.Code != http.StatusNotFound {
		t.Errorf("forbidden user, unknown env = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/env/env-off/containers", as("plain-user")); rec.Code != http.StatusConflict {
		t.Errorf("forbidden user, disabled env = %d, want 409", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/env/env-on/containers", as("plain-user")); rec.Code != http.StatusForbidden {
		t.Errorf("forbidden user, valid env = %d, want 403", rec.Code)
	}
	// An anonymous caller must not learn which environments exist.
	if rec := do(h, http.MethodGet, "/api/env/nope/containers"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous, unknown env = %d, want 401", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/env/env-on/containers"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous, valid env = %d, want 401", rec.Code)
	}
}

func TestACrossSiteOriginIsRefusedAndAudited(t *testing.T) {
	f := newFixture(t, nil)
	rec := do(f.r.Handler(), http.MethodPost, "/api/public", header("Origin", "https://evil.example"))
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "origin_mismatch" {
		t.Fatalf("cross-site POST = %d %s, want 403 origin_mismatch", rec.Code, rec.Body)
	}
	e := one(t, f.audit)
	if e.Action != audit.ActionPermissionDenied || e.Reason != "origin_mismatch" || e.IP != "192.0.2.1" {
		t.Errorf("entry = %+v, want permission.denied / origin_mismatch with the IP", e)
	}
}

func TestOriginGuardCoversEveryMutatingMethodAndNoReads(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	evil := header("Origin", "https://evil.example")
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		path := "/api/public"
		if m == http.MethodPut {
			path = "/api/settings"
		}
		if rec := do(h, m, path, as("admin-user"), evil); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a cross-site Origin = %d, want 403", m, rec.Code)
		}
	}
	// A read is not mutating: a foreign Origin does not stop it.
	if rec := do(h, http.MethodGet, "/api/public", evil); rec.Code != http.StatusOK {
		t.Errorf("GET with a cross-site Origin = %d, want 200", rec.Code)
	}
}

func TestAMatchingOriginPassesAndNoOriginFollowsSecFetchSite(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	cases := []struct {
		name string
		mods []func(*http.Request)
		want int
	}{
		{"matching origin", []func(*http.Request){header("Origin", publicOrigin)}, http.StatusOK},
		{"matching origin, other case", []func(*http.Request){header("Origin", "HTTPS://Bayplan.Example")}, http.StatusOK},
		{"matching origin with default port", []func(*http.Request){header("Origin", "https://bayplan.example:443")}, http.StatusOK},
		{"wrong scheme", []func(*http.Request){header("Origin", "http://bayplan.example")}, http.StatusForbidden},
		{"origin null", []func(*http.Request){header("Origin", "null")}, http.StatusForbidden},
		{"origin with a path", []func(*http.Request){header("Origin", "https://evil.example/https://bayplan.example")}, http.StatusForbidden},
		{"no origin, no fetch metadata", nil, http.StatusOK},
		{"no origin, same-origin", []func(*http.Request){header("Sec-Fetch-Site", "same-origin")}, http.StatusOK},
		{"no origin, none", []func(*http.Request){header("Sec-Fetch-Site", "none")}, http.StatusOK},
		{"no origin, cross-site", []func(*http.Request){header("Sec-Fetch-Site", "cross-site")}, http.StatusForbidden},
		{"no origin, same-site", []func(*http.Request){header("Sec-Fetch-Site", "same-site")}, http.StatusForbidden},
	}
	for _, tc := range cases {
		if rec := do(h, http.MethodPost, "/api/public", tc.mods...); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}

func TestBeforeAPublicURLIsSetOriginMustMatchTheRequestHost(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.PublicOrigin = func() string { return "" } })
	h := f.r.Handler()
	own := func(r *http.Request) { r.Host = "box.lan:8080" }
	if rec := do(h, http.MethodPost, "/api/public", own, header("Origin", "http://box.lan:8080")); rec.Code != http.StatusOK {
		t.Errorf("same host = %d, want 200; %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodPost, "/api/public", own, header("Origin", "http://evil.example")); rec.Code != http.StatusForbidden {
		t.Errorf("other host = %d, want 403", rec.Code)
	}
}

func TestBeforeAPublicURLIsSetAnHTTPSProxyIsBelievedOnlyWhenTrusted(t *testing.T) {
	trusted := netip.MustParsePrefix("10.0.0.0/8")
	f := newFixture(t, func(o *Options) {
		o.PublicOrigin = func() string { return "" }
		o.TrustedProxies = []netip.Prefix{trusted}
	})
	h := f.r.Handler()
	via := func(peer string) []func(*http.Request) {
		return []func(*http.Request){
			func(r *http.Request) { r.Host = "bayplan.example"; r.RemoteAddr = peer },
			header("X-Forwarded-Proto", "https"), header("Origin", "https://bayplan.example"),
		}
	}
	if rec := do(h, http.MethodPost, "/api/public", via("10.1.2.3:4000")...); rec.Code != http.StatusOK {
		t.Errorf("trusted proxy saying https = %d, want 200; %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodPost, "/api/public", via("198.51.100.7:4000")...); rec.Code != http.StatusForbidden {
		t.Errorf("untrusted peer saying https = %d, want 403", rec.Code)
	}
}

func TestForwardedForIsBelievedOnlyFromATrustedProxy(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	h := f.r.Handler()
	peer := func(addr string) func(*http.Request) { return func(r *http.Request) { r.RemoteAddr = addr } }

	do(h, http.MethodGet, "/api/audit", peer("10.1.2.3:5555"), header("X-Forwarded-For", "203.0.113.9"))
	do(h, http.MethodGet, "/api/audit", peer("198.51.100.7:5555"), header("X-Forwarded-For", "203.0.113.9"))
	// A trusted proxy that appends to a forged client value: the rightmost
	// untrusted address wins, never the leftmost.
	do(h, http.MethodGet, "/api/audit", peer("10.1.2.3:5555"), header("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 10.9.9.9"))
	// A trusted proxy with no usable header falls back to the peer.
	do(h, http.MethodGet, "/api/audit", peer("10.1.2.3:5555"))
	do(h, http.MethodGet, "/api/audit", peer("10.1.2.3:5555"), header("X-Forwarded-For", "not-an-ip"))

	want := []string{"203.0.113.9", "198.51.100.7", "203.0.113.9", "10.1.2.3", "10.1.2.3"}
	got := f.audit.all()
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].IP != w {
			t.Errorf("request %d: IP = %q, want %q", i, got[i].IP, w)
		}
	}
}

func TestForwardedForIsIgnoredWithNoTrustedProxiesConfigured(t *testing.T) {
	f := newFixture(t, nil)
	do(f.r.Handler(), http.MethodGet, "/api/audit", header("X-Forwarded-For", "203.0.113.9"))
	if e := one(t, f.audit); e.IP != "192.0.2.1" {
		t.Fatalf("IP = %q, want the peer 192.0.2.1", e.IP)
	}
}

func TestTheHandlerSeesTheResolvedClientIP(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} })
	rec := do(f.r.Handler(), http.MethodGet, "/api/public",
		func(r *http.Request) { r.RemoteAddr = "10.1.2.3:1" }, header("X-Forwarded-For", "203.0.113.9"))
	if !strings.Contains(rec.Body.String(), `"ip":"203.0.113.9"`) {
		t.Fatalf("handler IP wrong: %s", rec.Body)
	}
}

func TestGuardedRoutesAreHeldToTheSamePolicy(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	if rec := do(h, http.MethodGet, "/api/stream"); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/stream", as("plain-user")); rec.Code != http.StatusForbidden {
		t.Errorf("no permission = %d, want 403", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/stream", as("admin-user")); rec.Code != http.StatusOK || rec.Body.String() != "stream" {
		t.Errorf("admin = %d %q, want 200 stream", rec.Code, rec.Body)
	}
	if n := len(f.audit.all()); n != 2 {
		t.Errorf("audit entries = %d, want 2 (the two denials)", n)
	}
}

func TestASystemPrincipalWithAnEmptySetIsDeniedUnderItsOwnName(t *testing.T) {
	f := newFixture(t, nil)
	if rec := do(f.r.Handler(), http.MethodGet, "/api/audit", as("janitor")); rec.Code != http.StatusForbidden {
		t.Fatalf("system:janitor = %d, want 403", rec.Code)
	}
	if e := one(t, f.audit); e.Actor.Type != audit.ActorSystem || e.Actor.Name != "system:janitor" {
		t.Errorf("actor = %+v, want system:janitor", e.Actor)
	}
}

func TestAMutationGuardRefusalIs403CSRFFailedAndAudited(t *testing.T) {
	var sawPrincipal *rbac.Principal
	f := newFixture(t, func(o *Options) {
		o.MutationGuard = guardFunc(func(_ *http.Request, p *rbac.Principal) error {
			sawPrincipal = p
			return errors.New("bad token")
		})
	})
	rec := do(f.r.Handler(), http.MethodPut, "/api/settings", as("admin-user"))
	if rec.Code != http.StatusForbidden || problemType(t, rec) != "csrf_failed" {
		t.Fatalf("status = %d %s, want 403 csrf_failed", rec.Code, rec.Body)
	}
	if sawPrincipal == nil || sawPrincipal.ID != "admin-user" {
		t.Errorf("the guard saw principal %+v", sawPrincipal)
	}
	if e := one(t, f.audit); e.Reason != "csrf" || e.Actor.ID != "admin-user" {
		t.Errorf("entry = %+v, want reason csrf for admin-user", e)
	}
	// Reads and anonymous callers are not vetted by the guard.
	if rec := do(f.r.Handler(), http.MethodGet, "/api/audit", as("admin-user")); rec.Code != http.StatusOK {
		t.Errorf("GET behind a refusing guard = %d, want 200", rec.Code)
	}
	if rec := do(f.r.Handler(), http.MethodPost, "/api/public"); rec.Code != http.StatusOK {
		t.Errorf("anonymous POST to a Public route = %d, want 200", rec.Code)
	}
}

type guardFunc func(*http.Request, *rbac.Principal) error

func (g guardFunc) Check(r *http.Request, p *rbac.Principal) error { return g(r, p) }

func TestWhenTheDenialCannotBeAuditedTheRequestGets500AndIsStillRefused(t *testing.T) {
	f := newFixture(t, nil)
	f.audit.err = errors.New("disk full")
	h := f.r.Handler()
	for _, tc := range []struct {
		name string
		mods []func(*http.Request)
		path string
		meth string
	}{
		{"unauthenticated", nil, "/api/audit", http.MethodGet},
		{"forbidden", []func(*http.Request){as("plain-user")}, "/api/audit", http.MethodGet},
		{"origin", []func(*http.Request){header("Origin", "https://evil.example")}, "/api/public", http.MethodPost},
	} {
		rec := do(h, tc.meth, tc.path, tc.mods...)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", tc.name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "disk full") {
			t.Errorf("%s: the audit error leaked to the caller: %s", tc.name, rec.Body)
		}
	}
}

func TestAnAuthenticatorFailureFailsClosedExceptOnPublicRoutes(t *testing.T) {
	f := newFixture(t, nil)
	h := f.r.Handler()
	for _, path := range []string{"/api/audit", "/api/me", "/api/env/env-on/containers"} {
		if rec := do(h, http.MethodGet, path, as("boom")); rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500 when the caller cannot be authenticated", path, rec.Code)
		}
	}
	// A Public route needs no identity, so a broken session store does not take
	// it down; the caller is simply anonymous.
	rec := do(h, http.MethodGet, "/api/public", as("boom"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"principal":""`) {
		t.Errorf("Public route = %d %s, want 200 as anonymous", rec.Code, rec.Body)
	}
}

func TestRealAuditWriterReceivesTheDenial(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s := storetest.New(t)
	r := New(Options{
		Checker: rbac.NewChecker(s), Authenticator: fakeAuth{},
		Auditor:      audit.NewWriter(s, v, slog.New(slog.DiscardHandler), nil),
		PublicOrigin: func() string { return publicOrigin },
	})
	Register(r, huma.Operation{OperationID: "readAudit", Method: http.MethodGet, Path: "/api/audit"},
		rbac.Require(rbac.PermAuditRead), echo)

	if rec := do(r.Handler(), http.MethodGet, "/api/audit"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var actor, action, target, result, reason, ip, ua string
	err = s.Read.QueryRowContext(context.Background(),
		`SELECT actor_type, action, target_id, result, reason, ip, user_agent FROM audit_log`).
		Scan(&actor, &action, &target, &result, &reason, &ip, &ua)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("no audit entry was written")
	} else if err != nil {
		t.Fatal(err)
	}
	if actor != "anonymous" || action != "permission.denied" || target != "GET /api/audit" ||
		result != "denied" || reason != "unauthenticated" || ip != "192.0.2.1" || ua != "policy-test" {
		t.Errorf("row = %s %s %s %s %s %s %s", actor, action, target, result, reason, ip, ua)
	}
}

func TestRegisterAndGuardedPanicWhenScopeAndPathDisagree(t *testing.T) {
	r := New(Options{})
	expectPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: want a panic", name)
			}
		}()
		fn()
	}
	noop := func(context.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil }
	expectPanic("environment permission outside /api/env/{envId}", func() {
		Register(r, huma.Operation{OperationID: "a", Method: http.MethodGet, Path: "/api/containers"},
			rbac.Require(rbac.PermContainersRead), noop)
	})
	expectPanic("global permission inside /api/env/{envId}", func() {
		Register(r, huma.Operation{OperationID: "b", Method: http.MethodGet, Path: "/api/env/{envId}/audit"},
			rbac.Require(rbac.PermAuditRead), noop)
	})
	expectPanic("public route inside /api/env/{envId}", func() {
		Register(r, huma.Operation{OperationID: "c", Method: http.MethodGet, Path: "/api/env/{envId}/open"},
			rbac.Public, noop)
	})
	expectPanic("guarded zero requirement", func() {
		r.Guarded(http.MethodGet, "/api/x", rbac.Requirement{}, http.NotFoundHandler())
	})
}

func TestVerifyAllGuardedAcceptsRegisterGuardedAndExempt(t *testing.T) {
	f := newFixture(t, nil)
	if err := VerifyAllGuarded(f.r); err != nil {
		t.Fatalf("VerifyAllGuarded: %v", err)
	}
}

func TestVerifyAllGuardedNamesARouteMountedDirectlyOnTheMux(t *testing.T) {
	f := newFixture(t, nil)
	f.r.mux.Get("/api/sneaky", func(http.ResponseWriter, *http.Request) {})
	err := VerifyAllGuarded(f.r)
	if err == nil || !strings.Contains(err.Error(), "GET /api/sneaky") {
		t.Fatalf("error = %v, want one naming GET /api/sneaky", err)
	}
}

func TestVerifyAllGuardedNamesEveryUnguardedRoute(t *testing.T) {
	f := newFixture(t, nil)
	f.r.mux.Get("/api/one", func(http.ResponseWriter, *http.Request) {})
	f.r.mux.Post("/api/two", func(http.ResponseWriter, *http.Request) {})
	f.r.mux.Mount("/api/mounted", chi.NewRouter())
	f.r.mux.Mount("/api/sub", func() http.Handler {
		sub := chi.NewRouter()
		sub.Get("/inner", func(http.ResponseWriter, *http.Request) {})
		return sub
	}())
	err := VerifyAllGuarded(f.r)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"GET /api/one", "POST /api/two", "/api/sub/inner"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestVerifyAllGuardedRejectsAnExemptRouteThatIsNotHealthzOrReadyz(t *testing.T) {
	f := newFixture(t, nil)
	f.r.Exempt(http.MethodGet, "/api/backdoor", func(http.ResponseWriter, *http.Request) {})
	err := VerifyAllGuarded(f.r)
	if err == nil || !strings.Contains(err.Error(), "/api/backdoor") {
		t.Fatalf("error = %v, want one naming the exempt /api/backdoor", err)
	}
}

func TestVerifyAllGuardedAllowsTheTwoProbeRoutes(t *testing.T) {
	r := New(Options{})
	r.Exempt(http.MethodGet, "/healthz", func(http.ResponseWriter, *http.Request) {})
	r.Exempt(http.MethodGet, "/readyz", func(http.ResponseWriter, *http.Request) {})
	if err := VerifyAllGuarded(r); err != nil {
		t.Fatalf("VerifyAllGuarded: %v", err)
	}
}

func TestVerifyAllGuardedAllowsTheSetupPageRoutes(t *testing.T) {
	r := New(Options{})
	r.Exempt(http.MethodGet, "/setup", func(http.ResponseWriter, *http.Request) {})
	r.Exempt(http.MethodGet, "/setup/*", func(http.ResponseWriter, *http.Request) {})
	if err := VerifyAllGuarded(r); err != nil {
		t.Fatalf("VerifyAllGuarded: %v", err)
	}
}

func TestOriginOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://Bayplan.example/base": "https://bayplan.example",
		"http://localhost:5173":        "http://localhost:5173",
		"":                             "",
		"not a url":                    "",
	} {
		if got := OriginOf(in); got != want {
			t.Errorf("OriginOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// recheckAuth is a fakeAuth whose Recheck answers from a switch the test flips.
type recheckAuth struct {
	fakeAuth
	mu   sync.Mutex
	gone bool
	err  error
}

func (a *recheckAuth) Recheck(context.Context, *rbac.Principal) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.gone && a.err == nil, a.err
}

func (a *recheckAuth) set(gone bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gone, a.err = gone, err
}

func TestStillAuthorizedFollowsTheSessionTheEnvironmentAndThePermission(t *testing.T) {
	ra := &recheckAuth{}
	envs := fakeEnvs{"env-on": {ID: "env-on", Name: "On", Enabled: true}}
	f := newFixture(t, func(o *Options) { o.Authenticator = ra; o.EnvResolver = envs })

	var calls []bool
	f.r.Guarded(http.MethodGet, "/api/env/{envId}/probe", rbac.Require(rbac.PermContainersRead),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, StillAuthorized(r.Context()), StillAuthorized(r.Context()))
			_, _ = w.Write([]byte("ok"))
		}))
	h := f.r.Handler()
	probe := func() []bool {
		calls = nil
		if rec := do(h, http.MethodGet, "/api/env/env-on/probe", as("admin-user")); rec.Code != http.StatusOK {
			t.Fatalf("probe = %d, want 200: %s", rec.Code, rec.Body)
		}
		return calls
	}
	want := func(got []bool, v bool, why string) {
		t.Helper()
		if len(got) != 2 || got[0] != v || got[1] != v {
			t.Fatalf("%s: StillAuthorized = %v, want %v twice", why, got, v)
		}
	}
	want(probe(), true, "nothing changed")

	// The closure re-checks live state, so flip things between the two calls.
	flip := func(change func(f2 *fixture), why string, after bool) {
		t.Helper()
		var got []bool
		f2 := newFixture(t, func(o *Options) { o.Authenticator = ra; o.EnvResolver = envs })
		f2.r.Guarded(http.MethodGet, "/api/env/{envId}/probe", rbac.Require(rbac.PermContainersRead),
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, StillAuthorized(r.Context()))
				change(f2)
				got = append(got, StillAuthorized(r.Context()))
			}))
		if rec := do(f2.r.Handler(), http.MethodGet, "/api/env/env-on/probe", as("admin-user")); rec.Code != http.StatusOK {
			t.Fatalf("%s: probe = %d: %s", why, rec.Code, rec.Body)
		}
		if len(got) != 2 || !got[0] || got[1] != after {
			t.Fatalf("%s: StillAuthorized = %v, want true then %v", why, got, after)
		}
		if n := len(f2.audit.all()); n != 0 {
			t.Fatalf("%s: re-checking audited %d entries, want none", why, n)
		}
		ra.set(false, nil)
		envs["env-on"] = Env{ID: "env-on", Name: "On", Enabled: true}
	}
	flip(func(*fixture) { ra.set(true, nil) }, "the session is gone", false)
	flip(func(*fixture) { ra.set(false, errors.New("store down")) }, "the recheck errors", false)
	flip(func(*fixture) { delete(envs, "env-on") }, "the environment is removed", false)
	flip(func(*fixture) { envs["env-on"] = Env{ID: "env-on", Name: "On", Enabled: false} }, "the environment is disabled", false)
	flip(func(f2 *fixture) {
		if _, err := f2.s.Write.Exec(`UPDATE users SET instance_admin = 0 WHERE id = 'admin-user'`); err != nil {
			t.Fatal(err)
		}
	}, "the permission is withdrawn", false)
	flip(func(*fixture) {}, "nothing changes", true)

	if !StillAuthorized(context.Background()) {
		t.Fatal("StillAuthorized on a context that did not come through Guarded must be true")
	}
	if n := len(f.audit.all()); n != 0 {
		t.Fatalf("re-checking audited %d entries, want none", n)
	}
}
