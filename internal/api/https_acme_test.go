// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"crypto/x509/pkix"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/config"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
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
	f.mgr = acme.NewManager(acme.Deps{
		Store: as, Client: acme.NewClient(dir, as, stackops.Clock(clock), "tierplan-test/0"), Challenges: acme.NewChallenges(),
		Jobs: f.hub, Installer: noInstaller{}, Clock: clock,
		PublicURL: f.inst.PublicURL, AgentURL: f.inst.AgentURL, BasePath: f.inst.BasePath,
		DialHosts: f.dialHosts, CertFiles: o.certFiles, Log: slog.New(slog.DiscardHandler),
		Jitter: func() float64 { return 0 }, Rand: func(int64) int64 { return 0 },
	})

	leaf := httpsLeaf(t, pkix.Name{CommonName: "TierPlan self-signed"}, []string{"tierplan.example.com"}, nil)
	served := api.ServedCertificate{Source: api.SourceSelfSigned, Leaf: leaf}
	if o.served != nil {
		served = o.served(t)
	}
	f.https = api.HTTPSDeps{
		Served:    func() api.ServedCertificate { return served },
		CertFiles: o.certFiles, ACME: f.mgr, Jobs: f.hub, ListenPort: 7480, Log: slog.New(slog.DiscardHandler),
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
