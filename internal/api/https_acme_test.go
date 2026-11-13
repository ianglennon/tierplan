// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The Let's Encrypt routes of /api/https (phase 14, ACME-01, ACME-02, ACME-09, ACME-10, SET-03, D-08, D-10).

const (
	acmePublicURL = "https://tierplan.example.com"
	acmeTermsURL  = "https://ca.example/terms"
)

var acmeNow = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// noInstaller accepts a certificate and serves nothing: the API tests never look at the listener.
type noInstaller struct{}

func (noInstaller) ServeACME(_, _ []byte) error { return nil }
func (noInstaller) ServeSelfSigned() error      { return nil }

// acmeFix is the API over a real acme.Manager, a fake CA and the fixture's store and vault.
type acmeFix struct {
	*authFixture
	fake  *acmetest.Fake
	clock *acmetest.Clock
	hub   *jobs.Hub
	mgr   *acme.Manager
	store *acme.Store
	dir   acme.Directory

	mu    sync.Mutex
	dials []string

	cookie, csrf string
}

type acmeOpts struct {
	cfg       config.Config
	fake      acmetest.Options
	certFiles bool
	// realDirectory keeps the production directory URL instead of the fake's.
	realDirectory bool
	// served is what the listener serves; nil leaves a self-signed certificate.
	served func(t *testing.T) api.ServedCertificate
	// dns replaces the manager's DNS-01 dependencies; nil keeps the production ones.
	dns *acme.DNSDeps
	// env is what the TIERPLAN_ACME* variables say.
	env config.ACMEEnv
	// hstsOn is the manager's HSTS setting; nil is off.
	hstsOn func() bool
}

// dnsDeps is d, or the zero value when a test gives none.
func dnsDeps(d *acme.DNSDeps) acme.DNSDeps {
	if d == nil {
		return acme.DNSDeps{}
	}
	return *d
}

func (f *acmeFix) dialHosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...)
}

func (f *acmeFix) setDialHosts(h ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = h
}

// newACMEFix builds the router with the Let's Encrypt routes and signs the admin in.
func newACMEFix(t *testing.T, o acmeOpts) *acmeFix {
	t.Helper()
	if o.cfg.PublicURL == "" {
		o.cfg.PublicURL = acmePublicURL
	}
	if o.fake.TermsOfService == "" {
		o.fake.TermsOfService = acmeTermsURL
	}
	clock := acmetest.NewClock(acmeNow)
	o.fake.Clock = clock
	fake := acmetest.NewFake(t, o.fake)

	f := &acmeFix{authFixture: newAuthFixture(t, o.cfg), fake: fake, clock: clock}
	dirCfg := config.Config{ACMEDirectoryURL: fake.DirectoryURL(), ACMECAFile: fake.CAFile(t)}
	if o.realDirectory {
		dirCfg = config.Config{}
	}
	dir, err := acme.NewDirectory(dirCfg)
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}
	f.dir = dir
	f.hub = jobs.New(jobs.Options{Now: clock.Now})
	t.Cleanup(f.hub.Close)
	as := acme.NewStore(f.st, f.v)
	f.store = as
	f.mgr = acme.NewManager(acme.Deps{
		Store: as, Client: acme.NewClient(dir, as, stackops.Clock(clock), "tierplan-test/0"), Challenges: acme.NewChallenges(),
		Jobs: f.hub, Installer: noInstaller{}, Clock: clock,
		PublicURL: f.inst.PublicURL, AgentURL: f.inst.AgentURL, BasePath: f.inst.BasePath,
		DialHosts: f.dialHosts, CertFiles: o.certFiles, Log: slog.New(slog.DiscardHandler),
		Jitter: func() float64 { return 0 }, Rand: func(int64) int64 { return 0 },
		DNS: dnsDeps(o.dns), Env: o.env, HSTSOn: o.hstsOn, Audit: f.aud.Record,
	})

	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"tierplan.example.com"}, nil)
	served := api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf}
	if o.served != nil {
		served = o.served(t)
	}
	f.https = api.HTTPSDeps{
		Served:    func() api.ServedCertificate { return served },
		CertFiles: o.certFiles, ACME: f.mgr, Jobs: f.hub, ListenPort: 7480, Log: slog.New(slog.DiscardHandler), Audit: f.aud,
	}
	f.httpsAlerts = func(ctx context.Context) ([]acme.Alert, error) {
		sv := acme.Served{Source: served.Source}
		if served.Leaf != nil {
			sv.NotBefore, sv.NotAfter = served.Leaf.NotBefore, served.Leaf.NotAfter
		}
		return f.mgr.Alerts(ctx, sv)
	}
	f.h = f.build(t)
	f.cookie, f.csrf = f.signIn(t, "admin", false)
	return f
}

// get is an admin GET.
func (f *acmeFix) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodGet, path: path, cookie: f.cookie})
}

// send is an admin mutation with the CSRF proof.
func (f *acmeFix) send(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: method, path: path, body: body, cookie: f.cookie, csrf: f.csrf})
}

// status reads the HTTPS status and decodes it.
func (f *acmeFix) status(t *testing.T) api.HTTPSStatusBody {
	t.Helper()
	rec := f.get(t, "/api/https/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/https/status: %d %s", rec.Code, rec.Body)
	}
	var got api.HTTPSStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode the status: %v", err)
	}
	if got.ACME == nil {
		t.Fatalf("the status has no acme block: %s", rec.Body)
	}
	return got
}

func TestRegisterACMEAccountThroughTheAPI(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})

	before := f.status(t)
	if before.ACME.Account != nil {
		t.Fatalf("an account is shown before registration: %+v", before.ACME.Account)
	}

	rec := f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST account: %d %s, want 201", rec.Code, rec.Body)
	}
	if got := f.fake.AccountsSeen(); len(got) != 1 || !got[0].TermsOfServiceAgreed || len(got[0].Contact) != 0 {
		t.Errorf("the CA saw %+v, want one account with the terms agreed and no contact", got)
	}

	got := f.status(t)
	a := got.ACME.Account
	if a == nil {
		t.Fatalf("the status shows no account after registration: %+v", got.ACME)
	}
	if a.Contact != "" || a.TermsOfServiceURL != acmeTermsURL || a.RegisteredAt.IsZero() {
		t.Errorf("account = %+v, want no contact, the terms URL and a time", a)
	}
	if got.ACME.Mode != "off" || got.ACME.DirectoryURL != f.fake.DirectoryURL() || got.ACME.ListenPort != 7480 {
		t.Errorf("mode %q directory %q port %d", got.ACME.Mode, got.ACME.DirectoryURL, got.ACME.ListenPort)
	}
	for _, body := range []string{rec.Body.String(), f.get(t, "/api/https/status").Body.String()} {
		if strings.Contains(body, "PRIVATE KEY") {
			t.Errorf("a response holds key material: %s", body)
		}
	}
}

// register registers the account as the admin and requires a 201.
func (f *acmeFix) register(t *testing.T, email string) {
	t.Helper()
	body := `{"agreeTos":true}`
	if email != "" {
		body = `{"agreeTos":true,"email":"` + email + `"}`
	}
	if rec := f.send(t, http.MethodPost, "/api/https/acme/account", body); rec.Code != http.StatusCreated {
		t.Fatalf("POST account: %d %s, want 201", rec.Code, rec.Body)
	}
}

// pause marks Let's Encrypt as paused after a restore, as a restore does.
func (f *acmeFix) pause(t *testing.T) {
	t.Helper()
	st, err := f.store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st.PausedReason = acme.PausedRestore
	if err := f.store.SaveState(context.Background(), st); err != nil {
		t.Fatal(err)
	}
}

// problemOf requires the answer to be status with the problem type, and returns the problem.
func acmeProblemOf(t *testing.T, rec *httptest.ResponseRecorder, status int, ptype string) problemBody {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d %s, want %d %s", rec.Code, rec.Body, status, ptype)
	}
	p := decodeProblem(t, rec)
	if p.Type != ptype {
		t.Fatalf("problem type = %q (%s), want %q", p.Type, rec.Body, ptype)
	}
	return p
}

func TestRegisterACMEAccountProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   acmeOpts
		setup  func(t *testing.T, f *acmeFix)
		body   string
		status int
		ptype  string
		// location and message are the first error's, when it has one.
		location, message string
		// accounts is how many accounts the CA must have created.
		accounts int
	}{
		{name: "terms not accepted", body: `{"agreeTos":false}`, status: 422, ptype: "acme_tos_required", location: "body.agreeTos"},
		{name: "terms left out", body: `{}`, status: 422, ptype: "acme_tos_required", location: "body.agreeTos"},
		{name: "email is not an address", body: `{"agreeTos":true,"email":"x"}`, status: 422, ptype: "acme_contact_invalid", location: "body.email"},
		{name: "email with a display name", body: `{"agreeTos":true,"email":"Me <me@example.org>"}`, status: 422, ptype: "acme_contact_invalid", location: "body.email"},
		{
			name: "the CA refuses the contact", body: `{"agreeTos":true,"email":"me@example.org"}`,
			setup: func(_ *testing.T, f *acmeFix) {
				f.fake.FailNext(acmetest.EndpointNewAccount, acmetest.Problem{Status: 400, Type: "invalidContact", Detail: "no such mailbox"})
			},
			status: 422, ptype: "acme_contact_invalid", location: "body.email",
		},
		{
			name: "an account exists", body: `{"agreeTos":true}`,
			setup:  func(t *testing.T, f *acmeFix) { f.register(t, "") },
			status: 409, ptype: "acme_account_exists", accounts: 1,
		},
		{
			name: "the CA is unreachable", body: `{"agreeTos":true}`,
			setup: func(_ *testing.T, f *acmeFix) {
				for range 4 {
					f.fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal", Detail: "maintenance"})
				}
			},
			status: 502, ptype: "acme_ca_error", location: "class", message: acme.ClassCAUnavailable,
		},
		{
			name: "paused after a restore", body: `{"agreeTos":true}`,
			setup:  func(t *testing.T, f *acmeFix) { f.pause(t) },
			status: 409, ptype: "acme_paused",
		},
		{name: "certificate files", opts: acmeOpts{certFiles: true}, body: `{"agreeTos":true}`, status: 409, ptype: "acme_cert_files"},
		{name: "email longer than an address can be", body: `{"agreeTos":true,"email":"` + strings.Repeat("a", 250) + `@example.org"}`, status: 422},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, tc.opts)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			rec := f.send(t, http.MethodPost, "/api/https/acme/account", tc.body)
			if tc.ptype == "" {
				if rec.Code != tc.status {
					t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tc.status)
				}
			} else {
				p := acmeProblemOf(t, rec, tc.status, tc.ptype)
				if tc.location != "" {
					if len(p.Errors) == 0 || p.Errors[0].Location != tc.location {
						t.Errorf("errors = %+v, want the first at %q", p.Errors, tc.location)
					}
					if tc.message != "" && p.Errors[0].Message != tc.message {
						t.Errorf("first error message = %q, want %q", p.Errors[0].Message, tc.message)
					}
				}
			}
			if got := len(f.fake.AccountsSeen()); got != tc.accounts {
				t.Errorf("the CA created %d accounts, want %d", got, tc.accounts)
			}
			if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
				t.Error("the answer holds key material")
			}
		})
	}
}

// ACME-01 at the API: two registrations at once make one account and answer one 201 and one 409.
func TestRegisterACMEAccountConcurrently(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	start := make(chan struct{})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i] = f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`).Code
		}()
	}
	close(start)
	wg.Wait()

	created, exists := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			exists++
		}
	}
	if created != 1 || exists != 1 {
		t.Errorf("codes = %v, want one 201 and one 409", codes)
	}
	if got := len(f.fake.AccountsSeen()); got != 1 {
		t.Errorf("the CA created %d accounts, want 1", got)
	}
}

func TestACMEDirectoryEndpoint(t *testing.T) {
	t.Parallel()
	t.Run("terms and profiles sorted by name", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{fake: acmetest.Options{Profiles: map[string]time.Duration{
			"tlsserver": 90 * 24 * time.Hour, "classic": 90 * 24 * time.Hour, "shortlived": 160 * time.Hour,
		}}})
		rec := f.get(t, "/api/https/acme/directory")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET directory: %d %s", rec.Code, rec.Body)
		}
		var got api.HTTPSACMEDirectoryBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range got.Profiles {
			names = append(names, p.Name)
		}
		if strings.Join(names, ",") != "classic,shortlived,tlsserver" {
			t.Errorf("profiles = %v, want them sorted by name", names)
		}
		if got.TermsOfServiceURL != acmeTermsURL || got.URL != f.fake.DirectoryURL() || !got.Overridden {
			t.Errorf("directory = %+v", got)
		}
	})
	t.Run("paused makes no request to the CA", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.pause(t)
		before := f.fake.Count(acmetest.EndpointDirectory)
		acmeProblemOf(t, f.get(t, "/api/https/acme/directory"), 409, "acme_paused")
		if after := f.fake.Count(acmetest.EndpointDirectory); after != before {
			t.Errorf("directory requests %d -> %d, want none while paused", before, after)
		}
	})
	t.Run("certificate files", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{certFiles: true})
		acmeProblemOf(t, f.get(t, "/api/https/acme/directory"), 409, "acme_cert_files")
		if n := f.fake.Count(acmetest.EndpointDirectory); n != 0 {
			t.Errorf("directory requests = %d, want none with certificate files", n)
		}
	})
	t.Run("the CA cannot be read", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		for range 4 {
			f.fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal"})
		}
		p := acmeProblemOf(t, f.get(t, "/api/https/acme/directory"), 502, "acme_ca_error")
		if len(p.Errors) == 0 || p.Errors[0].Message != acme.ClassCAUnavailable {
			t.Errorf("errors = %+v, want the class ca_unavailable", p.Errors)
		}
	})
}

func TestPutACMEProfile(t *testing.T) {
	t.Parallel()
	put := func(f *acmeFix, profile string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"profile": profile})
		return f.send(t, http.MethodPut, "/api/https/acme/profile", string(b))
	}
	t.Run("an offered profile, then the CA default", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		if rec := put(f, "shortlived"); rec.Code != http.StatusOK {
			t.Fatalf("PUT shortlived: %d %s", rec.Code, rec.Body)
		}
		if got := f.status(t).ACME.Profile; got != "shortlived" {
			t.Errorf("profile = %q, want shortlived", got)
		}
		if rec := put(f, ""); rec.Code != http.StatusOK {
			t.Fatalf("PUT empty: %d %s", rec.Code, rec.Body)
		}
		if got := f.status(t).ACME.Profile; got != "" {
			t.Errorf("profile = %q, want the CA default", got)
		}
	})
	t.Run("an unknown profile is refused", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		p := acmeProblemOf(t, put(f, "nope"), 422, "acme_profile_unknown")
		if len(p.Errors) == 0 || p.Errors[0].Location != "body.profile" {
			t.Errorf("errors = %+v, want the first at body.profile", p.Errors)
		}
		if got := f.status(t).ACME.Profile; got != "" {
			t.Errorf("profile = %q after a refusal, want it unchanged", got)
		}
	})
	t.Run("a name the schema forbids", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		for _, bad := range []string{"Bad Name!", "UPPER", strings.Repeat("a", 65)} {
			if rec := put(f, bad); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("PUT %q: %d %s, want 422", bad, rec.Code, rec.Body)
			}
		}
		if n := f.fake.Count(acmetest.EndpointDirectory); n != 0 {
			t.Errorf("directory requests = %d, want none for a name the schema refuses", n)
		}
	})
	t.Run("the given name is kept when the list cannot be read", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		for range 4 {
			f.fake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal"})
		}
		if rec := put(f, "custom"); rec.Code != http.StatusOK {
			t.Fatalf("PUT custom: %d %s, want 200", rec.Code, rec.Body)
		}
		if got := f.status(t).ACME.Profile; got != "custom" {
			t.Errorf("profile = %q, want custom", got)
		}
	})
	t.Run("paused and certificate files", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.pause(t)
		acmeProblemOf(t, put(f, "classic"), 409, "acme_paused")
		g := newACMEFix(t, acmeOpts{certFiles: true})
		acmeProblemOf(t, put(g, "classic"), 409, "acme_cert_files")
	})
}

func TestHTTPSACMERoutesNeedSettingsWrite(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.addUser(t, "viewer", false) // no role at all, so neither settings.read nor settings.write
	viewer, viewerCSRF := f.signIn(t, "viewer", false)

	type route struct{ method, path, body string }
	reads := []route{{http.MethodGet, "/api/https/status", ""}, {http.MethodGet, "/api/https/acme/directory", ""}}
	writes := []route{
		{http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`},
		{http.MethodPut, "/api/https/acme/profile", `{"profile":"shortlived"}`},
		{http.MethodPut, "/api/https/acme/validation", `{"challengeType":"dns-01","token":"cf-token-7f3a91c2d04b8e65"}`},
		{http.MethodPost, "/api/https/acme/dns/test", `{"token":"cf-token-7f3a91c2d04b8e65"}`},
		{http.MethodDelete, "/api/https/acme/dns/token", ""},
	}
	for _, r := range append(append([]route{}, reads...), writes...) {
		if rec := f.call(t, authReq{method: r.method, path: r.path, body: r.body}); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d, want 401", r.method, r.path, rec.Code)
		}
		if rec := f.call(t, authReq{method: r.method, path: r.path, body: r.body, cookie: viewer, csrf: viewerCSRF}); rec.Code != http.StatusForbidden {
			t.Errorf("a user without settings.%s on %s %s: %d, want 403", map[bool]string{true: "read", false: "write"}[r.method == http.MethodGet], r.method, r.path, rec.Code)
		}
	}
	for _, r := range reads {
		if rec := f.get(t, r.path); rec.Code != http.StatusOK {
			t.Errorf("admin %s: %d %s, want 200", r.path, rec.Code, rec.Body)
		}
	}

	// Mutations need the CSRF proof and the page's own origin on top of the session.
	for _, r := range writes {
		if rec := f.call(t, authReq{method: r.method, path: r.path, body: r.body, cookie: f.cookie}); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without the CSRF token: %d, want 403", r.method, r.path, rec.Code)
		}
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-CSRF-Token", f.csrf)
		req.AddCookie(&http.Cookie{Name: auth.CookieName(f.inst.CookieScope()), Value: f.cookie}) //nolint:gosec // a request cookie as a browser sends it
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from a foreign origin: %d %s, want 403", r.method, r.path, rec.Code, rec.Body)
		}
	}
	if n := f.fake.Count(acmetest.EndpointNewAccount); n != 0 {
		t.Errorf("the CA saw %d registrations from refused requests, want none", n)
	}
	if got := f.status(t).ACME; got.Account != nil || got.Profile != "" {
		t.Errorf("a refused request changed the state: %+v", got)
	}
}

func TestHTTPSStatusListsNamesAndRefusals(t *testing.T) {
	t.Parallel()
	t.Run("an IP public address and a named agent address", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{cfg: config.Config{PublicURL: "https://192.0.2.1"}})
		f.inst.ApplyAgentURL("https://tierplan.example.com")
		f.setDialHosts("old.example.com", "old.example.com", "tierplan.example.com")

		a := f.status(t).ACME
		if a.PublicHost != "192.0.2.1" {
			t.Errorf("publicHost = %q, want 192.0.2.1", a.PublicHost)
		}
		want := []api.HTTPSACMENameBody{{Name: "192.0.2.1", Refused: true}, {Name: "tierplan.example.com"}}
		if len(a.Names) != len(want) || a.Names[0] != want[0] || a.Names[1] != want[1] {
			t.Errorf("names = %+v, want %+v", a.Names, want)
		}
		var ip, dial *api.HTTPSACMERefusalBody
		for i := range a.Refusals {
			switch a.Refusals[i].Code {
			case "names_ip":
				ip = &a.Refusals[i]
			case "agent_dial_host":
				dial = &a.Refusals[i]
			}
		}
		if ip == nil || ip.Host != "192.0.2.1" {
			t.Errorf("refusals = %+v, want names_ip for 192.0.2.1", a.Refusals)
		}
		if dial == nil || dial.Count != 2 || len(dial.Hosts) != 1 || dial.Hosts[0] != "old.example.com" {
			t.Errorf("refusals = %+v, want agent_dial_host with count 2 and old.example.com", a.Refusals)
		}
		if a.ChallengeType != acme.ChallengeHTTP01 {
			t.Errorf("challengeType = %q", a.ChallengeType)
		}
	})
	t.Run("one public name and nothing refused", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		a := f.status(t).ACME
		if len(a.Refusals) != 0 || len(a.Names) != 1 || a.Names[0].Name != "tierplan.example.com" || a.Names[0].Refused || a.PublicHost != "tierplan.example.com" {
			t.Errorf("names %+v refusals %+v publicHost %q", a.Names, a.Refusals, a.PublicHost)
		}
		if a.Mode != "off" || a.PausedReason != "" || a.Account != nil || a.Certificate != nil || a.Renewal != nil || a.LastError != nil || a.Blocked != nil || a.ActiveRun != nil || a.LastRun != nil {
			t.Errorf("a fresh install shows state: %+v", a)
		}
	})
	t.Run("the paused reason follows a restore and certificate files", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{})
		f.pause(t)
		if got := f.status(t).ACME.PausedReason; got != "restore" {
			t.Errorf("pausedReason = %q, want restore", got)
		}
	})
}

func TestHTTPSStatusFlagsTheTestDirectory(t *testing.T) {
	t.Parallel()
	letsEncrypt := func(t *testing.T) api.ServedCertificate {
		return api.ServedCertificate{Source: api.SourceLetsEncrypt, Leaf: httpsLeaf(t, pkix.Name{CommonName: "Pebble Intermediate"}, []string{"tierplan.example.com"}, nil)}
	}
	tests := []struct {
		name string
		opts acmeOpts
		test bool
	}{
		{"the test directory with a Let's Encrypt certificate", acmeOpts{served: letsEncrypt}, true},
		{"the real directory with a Let's Encrypt certificate", acmeOpts{served: letsEncrypt, realDirectory: true}, false},
		{"the test directory with the self-signed certificate", acmeOpts{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, tc.opts)
			got := f.status(t)
			if got.Certificate.TestCA != tc.test {
				t.Errorf("certificate.testCA = %v, want %v", got.Certificate.TestCA, tc.test)
			}
			if got.ACME.DirectoryOverridden != !tc.opts.realDirectory {
				t.Errorf("acme.directoryOverridden = %v, want %v", got.ACME.DirectoryOverridden, !tc.opts.realDirectory)
			}
		})
	}
}

// envDNSToken is the token an environment variable supplies in the tests below; no response may hold it.
const envDNSToken = "cf-env-token-b19e04d7a6c3825f" //nolint:gosec // G101: a recognisable fake token, so a test can prove it never leaks

func TestHTTPSResponsesNeverCarryKeys(t *testing.T) {
	t.Parallel()
	// The DNS provider and its token come from the environment, so the locks are in every answer.
	f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{
		DNSProvider: "cloudflare", CloudflareToken: envDNSToken, TokenVar: config.EnvCloudflareTokenFile,
	}})
	f.register(t, "ops@example.org")
	ctx := context.Background()
	rs, err := f.mgr.Issue(ctx, "user:test")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := f.hub.Wait(ctx, rs.JobID); err != nil {
			t.Errorf("Wait: %v", err)
		}
	}()
	testsupport.Await(t, "the certificate run to end", done)

	// Everything secret the server holds: the account key and the certificate key, in every form a
	// response could carry them.
	_, acctKey, found, err := f.store.Account(ctx, f.dir.URL)
	if err != nil || !found {
		t.Fatalf("Account = found %v, %v", found, err)
	}
	_, certKey, found, err := f.store.Certificate(ctx)
	if err != nil || !found {
		t.Fatalf("Certificate = found %v, %v", found, err)
	}
	var secrets []string
	for _, k := range []*ecdsa.PrivateKey{acctKey, certKey} {
		b, err := k.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b))
	}
	for _, k := range []any{acctKey, certKey} {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, base64.StdEncoding.EncodeToString(der))
	}

	bodies := map[string]string{
		"status":    f.get(t, "/api/https/status").Body.String(),
		"directory": f.get(t, "/api/https/acme/directory").Body.String(),
		"register":  f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`).Body.String(),
		"profile":   f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"classic"}`).Body.String(),
		"refused":   f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":false}`).Body.String(),
		// A token is sent with a resolver that is refused first, so no request leaves the test.
		"save refused": f.send(t, http.MethodPut, "/api/https/acme/validation", `{"challengeType":"dns-01","token":"cf-token-7f3a91c2d04b8e65","resolvers":["bad/entry"]}`).Body.String(),
		"test refused": f.send(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"cf-token-7f3a91c2d04b8e65","resolvers":["bad/entry"]}`).Body.String(),
		// The environment's token is locked: a typed token and a removal are refused, and the answers name the
		// variable, never the token.
		"save locked":   f.send(t, http.MethodPut, "/api/https/acme/validation", `{"challengeType":"dns-01","token":"cf-token-7f3a91c2d04b8e65"}`).Body.String(),
		"test locked":   f.send(t, http.MethodPost, "/api/https/acme/dns/test", `{"token":"cf-token-7f3a91c2d04b8e65"}`).Body.String(),
		"remove locked": f.send(t, http.MethodDelete, "/api/https/acme/dns/token", "").Body.String(),
	}
	for name, body := range bodies {
		if body == "" {
			t.Errorf("%s: empty body", name)
		}
		for _, bad := range []string{"PRIVATE KEY", "BEGIN ", "keyAuthorization", "key_authorization"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s holds %q: %s", name, bad, body)
			}
		}
		if strings.Contains(body, "cf-token-7f3a91c2d04b8e65") || strings.Contains(body, envDNSToken) {
			t.Errorf("%s holds a DNS token: %s", name, body)
		}
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s holds key material %q", name, secret)
			}
		}
	}

	// The same run is described without its secrets.
	var got api.HTTPSStatusBody
	if err := json.Unmarshal([]byte(bodies["status"]), &got); err != nil {
		t.Fatal(err)
	}
	a := got.ACME
	if a.Mode != "active" || a.Certificate == nil || len(a.Certificate.Names) != 1 || a.Certificate.Names[0] != "tierplan.example.com" {
		t.Fatalf("after the issue: mode %q certificate %+v", a.Mode, a.Certificate)
	}
	if a.Renewal == nil || a.Renewal.Basis == "" || !a.Renewal.NextAt.After(a.Certificate.IssuedAt) {
		t.Errorf("renewal = %+v, want a later time and a basis", a.Renewal)
	}
	if a.LastRun == nil || a.LastRun.Outcome != "succeeded" || a.LastRun.Kind != "issue" || a.LastRun.Trigger != "admin" || a.LastRun.JobID != rs.JobID || a.LastRun.FinishedAt == nil {
		t.Errorf("lastRun = %+v, want a finished admin issue that succeeded", a.LastRun)
	}
	if a.Account == nil || a.Account.Contact != "ops@example.org" {
		t.Errorf("account = %+v, want the contact without its mailto: prefix", a.Account)
	}
	// The environment's token is in use and locked, and the status says so without a trace of it.
	if a.DNS.TokenSource != "env" || a.DNS.TokenSaved {
		t.Errorf("dns = %+v, want the token from env and none saved", a.DNS)
	}
	if l := a.Locks.DNSToken; l == nil || l.Var != "TIERPLAN_CLOUDFLARE_API_TOKEN_FILE" || l.Value != "" || l.Stored != "" {
		t.Errorf("locks.dnsToken = %+v, want only the variable's name", l)
	}
}

// A server built without the manager still publishes the routes and answers them 503; the status has no
// acme block.
func TestHTTPSACMERoutesWithoutAManager(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"localhost"}, nil)
	f.https = api.HTTPSDeps{Served: func() api.ServedCertificate { return api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf} }}
	f.h = f.build(t)
	cookie, csrf := f.signIn(t, "admin", false)

	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/https/status", cookie: cookie})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"acme"`) {
		t.Errorf("status = %d %s, want 200 with no acme block", rec.Code, rec.Body)
	}
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/https/acme/directory", ""},
		{http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`},
		{http.MethodPut, "/api/https/acme/profile", `{"profile":""}`},
	} {
		rec := f.call(t, authReq{method: r.method, path: r.path, body: r.body, cookie: cookie, csrf: csrf})
		acmeProblemOf(t, rec, http.StatusServiceUnavailable, "https_unavailable")
	}
}

// Reads need settings.read and every change settings.write, and no other permission is involved (D-10).
// Both permissions belong to the instance admin alone, so a request cannot tell them apart; the route
// table can.
func TestHTTPSACMERoutesDeclareTheirPermissions(t *testing.T) {
	t.Parallel()
	r := httpx.New(httpx.Options{})
	api.RegisterHTTPS(r, api.HTTPSDeps{})
	want := map[string]rbac.Perm{
		"GET /api/https/status":         rbac.PermSettingsRead,
		"GET /api/https/acme/directory": rbac.PermSettingsRead,
		"POST /api/https/acme/account":  rbac.PermSettingsWrite,
		"PUT /api/https/acme/profile":   rbac.PermSettingsWrite,

		"PUT /api/https/acme/validation":   rbac.PermSettingsWrite,
		"POST /api/https/acme/dns/test":    rbac.PermSettingsWrite,
		"DELETE /api/https/acme/dns/token": rbac.PermSettingsWrite,
	}
	got := map[string]rbac.Perm{}
	for _, rt := range r.Routes() {
		got[rt.Method+" "+rt.Path] = rt.Req.Perm
	}
	for route, perm := range want {
		if got[route] != perm {
			t.Errorf("%s needs %q, want %q", route, got[route], perm)
		}
	}
}

// The profile is not changed under a run in progress.
func TestPutACMEProfileDuringARunIsRefused(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{})
	f.register(t, "")
	f.fake.HoldFinalize() // the run stays in progress until the fixture's hub is closed
	if _, err := f.mgr.Issue(context.Background(), "user:test"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	acmeProblemOf(t, f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"shortlived"}`), 409, "acme_run_active")
	if got := f.status(t).ACME; got.Profile != "" || got.ActiveRun == nil || got.ActiveRun.Outcome != "running" {
		t.Errorf("profile %q active run %+v, want the profile unchanged and the run shown as running", got.Profile, got.ActiveRun)
	}
}

// The actions TIERPLAN_ACME fixes answer 409 acme_locked with the variable's name and nothing changed
// (phase 16, D-04, D-17): Issue and Renew now with the variable off, Stop with either value; Issue stays
// allowed with it on.
func TestHTTPSACMEActionsLockedByTheEnvironment(t *testing.T) {
	t.Parallel()
	lockedDetail := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		acmeProblemOf(t, rec, http.StatusConflict, "acme_locked")
		var p struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Detail != "Set by the TIERPLAN_ACME environment variable." {
			t.Errorf("detail = %q, want it to name the variable and nothing else", p.Detail)
		}
	}
	on, off := true, false

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Enabled: &off}})
		f.register(t, "")
		for _, path := range []string{"/api/https/acme/issue", "/api/https/acme/renew", "/api/https/acme/stop"} {
			lockedDetail(t, f.send(t, http.MethodPost, path, ""))
		}
		if n := f.ordersSeen(); n != 0 {
			t.Errorf("the CA saw %d orders from refused actions, want none", n)
		}
	})
	t.Run("on", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Enabled: &on}})
		f.register(t, "")
		lockedDetail(t, f.send(t, http.MethodPost, "/api/https/acme/stop", ""))
		// Issue is the manual retry and stays allowed.
		f.awaitRun(t, startRun(t, f.issue(t)))
		if n := f.ordersSeen(); n != 1 {
			t.Errorf("the CA saw %d orders, want the one the allowed Issue placed", n)
		}
		// With a certificate stored, Renew now is allowed and Stop is still refused.
		lockedDetail(t, f.send(t, http.MethodPost, "/api/https/acme/stop", ""))
	})
}

// problemDetail is the detail of a problem answer.
func problemDetail(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var p struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p.Detail
}

// A registration against TIERPLAN_ACME_EMAIL or TIERPLAN_ACME_ACCEPT_TOS answers 409 with the field's own
// problem type and creates no account; the variable's address is used when none is typed (phase 16, D-17,
// UI-SPEC A2, A3). The detail names the variable and never an address.
func TestRegisterACMEAccountLockedByTheEnvironment(t *testing.T) {
	t.Parallel()
	env := config.ACMEEnv{Email: "ops@example.com", AcceptTOS: true}

	for _, tc := range []struct {
		name, body, ptype, variable string
	}{
		{"a different email", `{"agreeTos":true,"email":"other@example.org"}`, "acme_email_locked", "TIERPLAN_ACME_EMAIL"},
		{"terms not agreed", `{"agreeTos":false,"email":"ops@example.com"}`, "acme_tos_locked", "TIERPLAN_ACME_ACCEPT_TOS"},
		{"terms left out", `{}`, "acme_tos_locked", "TIERPLAN_ACME_ACCEPT_TOS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newACMEFix(t, acmeOpts{env: env})
			rec := f.send(t, http.MethodPost, "/api/https/acme/account", tc.body)
			acmeProblemOf(t, rec, http.StatusConflict, tc.ptype)
			if got, want := problemDetail(t, rec), "Set by the "+tc.variable+" environment variable."; got != want {
				t.Errorf("detail = %q, want %q", got, want)
			}
			if strings.Contains(rec.Body.String(), "ops@example.com") || strings.Contains(rec.Body.String(), "other@example.org") {
				t.Errorf("the answer shows an address: %s", rec.Body)
			}
			if n := len(f.fake.AccountsSeen()); n != 0 {
				t.Errorf("the CA created %d accounts from a refused registration, want none", n)
			}
			if f.status(t).ACME.Account != nil {
				t.Error("the status shows an account after a refused registration")
			}
		})
	}

	t.Run("no email typed takes the variable's", func(t *testing.T) {
		t.Parallel()
		f := newACMEFix(t, acmeOpts{env: env})
		if rec := f.send(t, http.MethodPost, "/api/https/acme/account", `{"agreeTos":true}`); rec.Code != http.StatusCreated {
			t.Fatalf("POST account: %d %s, want 201", rec.Code, rec.Body)
		}
		if got := f.fake.AccountsSeen(); len(got) != 1 || !slices.Equal(got[0].Contact, []string{"mailto:ops@example.com"}) {
			t.Errorf("the CA saw %+v, want one account with the variable's address", got)
		}
		if a := f.status(t).ACME.Account; a == nil || a.Contact != "ops@example.com" {
			t.Errorf("account = %+v, want the contact ops@example.com", a)
		}
	})
}

// While TIERPLAN_ACME_PROFILE is set, a different profile answers 409 acme_profile_locked and stores
// nothing, and the variable's own value is accepted without a write.
func TestPutACMEProfileLockedByTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Profile: "shortlived"}})

	rec := f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"classic"}`)
	acmeProblemOf(t, rec, http.StatusConflict, "acme_profile_locked")
	if got, want := problemDetail(t, rec), "Set by the TIERPLAN_ACME_PROFILE environment variable."; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	rec = f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":""}`)
	acmeProblemOf(t, rec, http.StatusConflict, "acme_profile_locked")

	rec = f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"shortlived"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT the variable's own profile: %d %s, want 200", rec.Code, rec.Body)
	}
	st, err := f.store.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Profile != "" {
		t.Errorf("stored profile = %q, want nothing stored by a refused write or an echo", st.Profile)
	}
}

// The status carries one entry for each field a variable fixes, with the variable's name, its value and
// the stored value beneath it; a field no variable fixes has none. The terms agreement has no stored value.
func TestHTTPSACMEStatusCarriesTheLocks(t *testing.T) {
	t.Parallel()
	off := false
	f := newACMEFix(t, acmeOpts{env: config.ACMEEnv{Enabled: &off, Email: "ops@example.com", AcceptTOS: true, Profile: "shortlived"}})
	f.register(t, "")

	l := f.status(t).ACME.Locks
	check := func(name string, got *api.HTTPSLockBody, want api.HTTPSLockBody) {
		t.Helper()
		if got == nil || *got != want {
			t.Errorf("locks.%s = %+v, want %+v", name, got, want)
		}
	}
	check("acme", l.ACME, api.HTTPSLockBody{Var: "TIERPLAN_ACME", Value: "off", Stored: "off"})
	check("email", l.Email, api.HTTPSLockBody{Var: "TIERPLAN_ACME_EMAIL", Value: "ops@example.com"})
	check("acceptTos", l.AcceptTOS, api.HTTPSLockBody{Var: "TIERPLAN_ACME_ACCEPT_TOS", Value: "true"})
	check("profile", l.Profile, api.HTTPSLockBody{Var: "TIERPLAN_ACME_PROFILE", Value: "shortlived"})

	none := newACMEFix(t, acmeOpts{}).status(t).ACME.Locks
	if none != (api.HTTPSLocksBody{}) {
		t.Errorf("locks = %+v with no variable set, want no entry", none)
	}
}

// Stop using Let's Encrypt is refused while HSTS is on: 409 hsts_on telling the admin to turn HSTS off first,
// with nothing stopped. Once HSTS is off the request reaches the manager's other checks (D-26, T-16-41).
func TestStopACMEIsRefusedWhileHSTSIsOnThroughTheAPI(t *testing.T) {
	t.Parallel()
	var on atomic.Bool
	on.Store(true)
	f := newACMEFix(t, acmeOpts{hstsOn: on.Load})

	rec := f.send(t, http.MethodPost, "/api/https/acme/stop", "")
	acmeProblemOf(t, rec, http.StatusConflict, "hsts_on")
	if !strings.Contains(rec.Body.String(), "HSTS is on. Turn it off first.") {
		t.Errorf("body = %s, want the instruction to turn HSTS off first", rec.Body)
	}

	on.Store(false)
	acmeProblemOf(t, f.send(t, http.MethodPost, "/api/https/acme/stop", ""), http.StatusConflict, "acme_not_active")
}
