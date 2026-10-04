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
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ianglennon/bayplan/internal/api"
	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/auth"
	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/instance"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/internal/store/storetest"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/internal/webui"
)

const testClientAddr = "203.0.113.9:40000"

type setupFixture struct {
	h        http.Handler
	st       *store.Store
	gate     *auth.SetupGate
	token    string
	logs     *bytes.Buffer
	clk      *fakeClock
	inst     *instance.Instance
	sessions *auth.Sessions
	dataDir  string
	// throttle is the AnonAudit throttle when the fixture was built with one.
	throttle *auth.AuditThrottle
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newSetupFixture(t *testing.T, cfg config.Config) *setupFixture {
	t.Helper()
	return newSetupFixtureWith(t, cfg, func(w *audit.Writer) api.SetupAuditor { return w })
}

// newSetupFixtureWith lets a test wrap the audit writer the setup routes use,
// for example to make the completion's audit write fail.
func newSetupFixtureWith(t *testing.T, cfg config.Config, wrap func(*audit.Writer) api.SetupAuditor) *setupFixture {
	t.Helper()
	return newSetupFixtureThrottled(t, cfg, wrap, 0)
}

// newSetupFixtureThrottled also puts an AuditThrottle with the given per-address
// burst in SetupDeps.AnonAudit; a burst of 0 leaves AnonAudit nil.
func newSetupFixtureThrottled(t *testing.T, cfg config.Config, wrap func(*audit.Writer) api.SetupAuditor, anonBurst int) *setupFixture {
	t.Helper()
	st := storetest.New(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	v, err := vault.LoadOrCreate(key, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.NewWriter(st, v, log, nil)
	if err := rbac.SeedAdminRole(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(context.Background(), st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.NewSessions(st, v, 12*time.Hour, 30*24*time.Hour, nil)

	dataDir := t.TempDir()
	gate := auth.NewSetupGate(st, dataDir, slog.New(slog.DiscardHandler))
	if err := gate.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	token, _ := auth.ReadTokenFile(dataDir)

	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	site := fstest.MapFS{"index.html": {Data: []byte(indexHTML)}}
	spa := webui.Handler(site, nil)
	r := httpx.New(httpx.Options{Fallback: spa, Auditor: aud, Logger: log, PublicOrigin: inst.PublicOrigin})
	var throttle *auth.AuditThrottle
	var anonAudit httpx.Auditor
	if anonBurst > 0 {
		throttle = auth.NewAuditThrottle(aud, auth.NewIPLimiter(time.Hour, anonBurst, 10*time.Minute, clk.now), log, clk.now)
		anonAudit = throttle
	}
	api.RegisterSetup(r, api.SetupDeps{
		AnonAudit: anonAudit,
		Gate:      gate,
		Limiter:   auth.NewIPLimiter(12*time.Second, 5, 10*time.Minute, clk.now),
		Audit:     wrap(aud),
		Store:     st,
		Config:    cfg,
		Instance:  inst,
		Sessions:  sessions,
		SPA:       spa,
		Hostname:  func() (string, error) { return "swarm-mgr-1", nil },
		Log:       log,
	})
	if err := httpx.VerifyAllGuarded(r); err != nil {
		t.Fatal(err)
	}
	return &setupFixture{h: r.Handler(), st: st, gate: gate, token: token, logs: logs, clk: clk, inst: inst, sessions: sessions, dataDir: dataDir, throttle: throttle}
}

func (f *setupFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/setup/token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "setup-test/1.0")
	req.RemoteAddr = testClientAddr
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *setupFixture) postToken(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"token": token})
	return f.post(t, string(b))
}

func (f *setupFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = testClientAddr
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *setupFixture) addUser(t *testing.T) {
	t.Helper()
	now := time.Now().UnixNano()
	if err := f.st.Q.InsertUser(context.Background(), sqlc.InsertUserParams{
		ID: "u1", Username: "admin", PasswordHash: "x", CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *setupFixture) auditRows(t *testing.T) []sqlc.AuditLog {
	t.Helper()
	rows, err := f.st.RQ.ListAuditForVerify(context.Background(), sqlc.ListAuditForVerifyParams{Seq: 0, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func problemType(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("not a problem body: %v: %s", err, rec.Body)
	}
	return p.Type
}

func TestSetupInstanceIsPublicAndReportsNeedsSetupOnAFreshStore(t *testing.T) {
	f := newSetupFixture(t, config.Config{SessionRememberLifetime: 720 * time.Hour})
	rec := f.get(t, "/api/instance")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var got struct {
		InstanceName string `json:"instanceName"`
		NeedsSetup   bool   `json:"needsSetup"`
		RememberDays int    `json:"rememberDays"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.InstanceName != "" || !got.NeedsSetup || got.RememberDays != 30 {
		t.Fatalf("got %+v, want {\"\" true 30}", got)
	}
}

func TestSetupInstanceReportsTheStoredNameAfterSetup(t *testing.T) {
	f := newSetupFixture(t, config.Config{SessionRememberLifetime: 24 * time.Hour})
	f.addUser(t)
	f.inst.Apply("Home lab", "")
	rec := f.get(t, "/api/instance")
	var got struct {
		InstanceName string `json:"instanceName"`
		NeedsSetup   bool   `json:"needsSetup"`
		RememberDays int    `json:"rememberDays"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.InstanceName != "Home lab" || got.NeedsSetup || got.RememberDays != 1 {
		t.Fatalf("got %+v, want {Home lab false 1}", got)
	}
}

func TestSetupTokenRightTokenReturnsTheStepThreeDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		url    string
		locked bool
	}{
		"unlocked": {url: "", locked: false},
		"locked":   {url: "https://bayplan.example.com", locked: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t, config.Config{PublicURL: tc.url, SessionRememberLifetime: 720 * time.Hour})
			rec := f.postToken(t, f.token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
			}
			var got struct {
				DefaultInstanceName string `json:"defaultInstanceName"`
				PublicURL           string `json:"publicUrl"`
				PublicURLLocked     bool   `json:"publicUrlLocked"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.DefaultInstanceName != "swarm-mgr-1" || got.PublicURL != tc.url || got.PublicURLLocked != tc.locked {
				t.Fatalf("got %+v", got)
			}
			if n := len(f.auditRows(t)); n != 0 {
				t.Fatalf("a correct token wrote %d audit entries, want 0", n)
			}
		})
	}
}

func TestSetupTokenAcceptsTheTokenWithoutDashes(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	if rec := f.postToken(t, strings.ToUpper(strings.ReplaceAll(f.token, "-", ""))); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
}

func TestSetupTokenWrongTokenIs401AndAuditedWithoutTheValue(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	const attempt = "deadbeef-deadbeef-deadbeef-deadbeef"
	rec := f.postToken(t, attempt)
	if rec.Code != http.StatusUnauthorized || problemType(t, rec) != "invalid_setup_token" {
		t.Fatalf("got %d %s, want 401 invalid_setup_token", rec.Code, rec.Body)
	}
	rows := f.auditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want exactly 1", len(rows))
	}
	r := rows[0]
	if r.Action != audit.ActionSetupTokenFailed || r.ActorType != string(audit.ActorAnonymous) ||
		r.Ip != "203.0.113.9" || r.Reason != "invalid_token" || r.UserAgent != "setup-test/1.0" {
		t.Fatalf("unexpected audit row %+v", r)
	}
	if strings.Contains(f.logs.String(), "203.0.113.9") == false {
		t.Fatal("the container log must carry the source IP")
	}
	for _, secret := range []string{attempt, strings.ReplaceAll(attempt, "-", "")} {
		if strings.Contains(f.logs.String(), secret) || strings.Contains(rec.Body.String(), secret) ||
			strings.Contains(r.Details, secret) || strings.Contains(r.Reason, secret) {
			t.Fatal("the attempted token value leaked into the log, the response or the audit entry")
		}
	}
	// A failed attempt must not burn the real token.
	if rec := f.postToken(t, f.token); rec.Code != http.StatusOK {
		t.Fatalf("the real token after a failure = %d, want 200", rec.Code)
	}
}

func TestSetupTokenSixthAttemptInAMinuteIsRateLimitedAndAudited(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	for i := 1; i <= 5; i++ {
		if rec := f.postToken(t, "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, rec.Code)
		}
	}
	// Even the right token is refused once the budget is spent.
	rec := f.postToken(t, f.token)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("6th attempt = %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	rows := f.auditRows(t)
	if len(rows) != 6 {
		t.Fatalf("audit rows = %d, want 6 (every failure is recorded)", len(rows))
	}
	last := rows[5]
	if last.Action != audit.ActionSetupTokenFailed || last.Reason != "rate_limited" || last.Ip != "203.0.113.9" {
		t.Fatalf("6th audit row %+v, want setup.token_failed / rate_limited / the client IP", last)
	}
	// Another address has its own budget.
	req := httptest.NewRequest(http.MethodPost, "/api/setup/token", strings.NewReader(`{"token":"`+f.token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "198.51.100.4:1234"
	other := httptest.NewRecorder()
	f.h.ServeHTTP(other, req)
	if other.Code != http.StatusOK {
		t.Fatalf("a different IP = %d, want 200", other.Code)
	}
	// And the budget refills over time.
	f.clk.t = f.clk.t.Add(13 * time.Second)
	if rec := f.postToken(t, f.token); rec.Code != http.StatusOK {
		t.Fatalf("after the refill = %d, want 200", rec.Code)
	}
}

// Behind a subpath the browser follows the redirect literally, so it must carry
// the base: a Location of "/login" leaves the application and lands on whatever
// the proxy serves at the root (found by the plan 02-32 end-to-end run behind
// Caddy at /bayplan, where it showed a blank page).
func TestSetupPageRedirectsToTheLoginPageUnderTheBasePath(t *testing.T) {
	for name, tc := range map[string]struct{ url, want string }{
		"no public url":   {url: "", want: "/login"},
		"root":            {url: "https://bayplan.example.com", want: "/login"},
		"under a subpath": {url: "https://example.com/bayplan", want: "/bayplan/login"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(t, config.Config{PublicURL: tc.url})
			f.addUser(t)
			rec := f.get(t, "/setup")
			if rec.Code != http.StatusFound || rec.Header().Get("Location") != tc.want {
				t.Fatalf("GET /setup = %d Location %q, want 302 %s", rec.Code, rec.Header().Get("Location"), tc.want)
			}
		})
	}
}

func TestSetupTokenAfterAUserExistsIs409AndAudited(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	f.addUser(t)

	rec := f.postToken(t, f.token)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "setup_complete" {
		t.Fatalf("got %d %s, want 409 setup_complete", rec.Code, rec.Body)
	}
	rows := f.auditRows(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionSetupAttemptDenied || rows[0].Ip != "203.0.113.9" {
		t.Fatalf("audit rows %+v, want one setup.attempt_denied with the client IP", rows)
	}
	// Even the previously valid token no longer verifies: the gate is shut.
	if f.gate.Verify(f.token) {
		t.Fatal("the gate still verifies the token after a user exists")
	}

	redirect := f.get(t, "/setup")
	if redirect.Code != http.StatusFound || redirect.Header().Get("Location") != "/login" {
		t.Fatalf("GET /setup = %d Location %q, want 302 /login", redirect.Code, redirect.Header().Get("Location"))
	}
	if rec := f.get(t, "/setup/"); rec.Code != http.StatusFound {
		t.Fatalf("GET /setup/ = %d, want 302", rec.Code)
	}
	if n := len(f.auditRows(t)); n != 1 {
		t.Fatalf("navigating to /setup wrote audit entries: %d rows, want 1", n)
	}
	var inst struct {
		NeedsSetup bool `json:"needsSetup"`
	}
	if err := json.Unmarshal(f.get(t, "/api/instance").Body.Bytes(), &inst); err != nil || inst.NeedsSetup {
		t.Fatalf("GET /api/instance needsSetup = %v (err %v), want false", inst.NeedsSetup, err)
	}
}

func TestSetupTokenFloodAfterSetupIsAnsweredButAuditedOnceWithACount(t *testing.T) {
	f := newSetupFixtureThrottled(t, config.Config{}, func(w *audit.Writer) api.SetupAuditor { return w }, 3)
	f.addUser(t)

	for i := range 50 {
		rec := f.postToken(t, "not-the-token")
		if rec.Code != http.StatusConflict || problemType(t, rec) != "setup_complete" {
			t.Fatalf("request %d: got %d %s, want 409 setup_complete", i, rec.Code, rec.Body)
		}
	}
	denied := func() []sqlc.AuditLog {
		var out []sqlc.AuditLog
		for _, r := range f.auditRows(t) {
			if r.Action == audit.ActionSetupAttemptDenied {
				out = append(out, r)
			}
		}
		return out
	}
	if got := len(denied()); got != 3 {
		t.Fatalf("setup.attempt_denied rows = %d, want the burst of 3", got)
	}
	if err := f.throttle.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := denied()
	if len(rows) != 4 {
		t.Fatalf("setup.attempt_denied rows after Flush = %d, want 4", len(rows))
	}
	var det map[string]string
	if err := json.Unmarshal([]byte(rows[3].Details), &det); err != nil || det["suppressed"] != "47" {
		t.Fatalf("summary details = %q (%v), want suppressed 47", rows[3].Details, err)
	}
	if strings.Contains(rows[3].Details, "not-the-token") {
		t.Fatal("a submitted token reached the audit log")
	}
}

func TestSetupPageServesTheSPAWhileSetupIsNeeded(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	rec := f.get(t, "/setup")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("GET /setup = %d %q, want the SPA index", rec.Code, rec.Body)
	}
}

func TestSetupTokenBodyIsCappedAndValidated(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	if rec := f.postToken(t, strings.Repeat("a", 129)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 129 character token = %d, want 422", rec.Code)
	}
	if rec := f.post(t, `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a missing token = %d, want 422", rec.Code)
	}
	if n := len(f.auditRows(t)); n != 0 {
		t.Fatalf("validation failures wrote %d audit entries, want 0", n)
	}
}

func TestSetupTokenCannotBeBypassedWithHeadersOrQueryParameters(t *testing.T) {
	f := newSetupFixture(t, config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/api/setup/token?token="+f.token, strings.NewReader(`{"token":"nope"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Setup-Token", f.token)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.RemoteAddr = testClientAddr
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: only the JSON body token counts", rec.Code)
	}
}
