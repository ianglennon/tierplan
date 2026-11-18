// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
)

// The install-wide switches of the per-hostname certificates (Phase 20, D-32, D-36, D-37): the acknowledgement of the
// certificate authority's terms and the resume after a restore.

const (
	certAccountPath = "/api/ingress/certificates/account"
	certResumePath  = "/api/ingress/certificates/resume"
	fakeTermsURL    = "https://ca.example.test/terms"
)

// newIngressCAFix is an ingress fixture whose certificate manager orders from a fake directory, so reading the
// directories never reaches Let's Encrypt. Both issuers map to that directory (D-19).
func newIngressCAFix(t *testing.T) (*ingressFix, *acmetest.Fake) {
	t.Helper()
	f := newIngressFix(t)
	fake := acmetest.NewFake(t, acmetest.Options{TermsOfService: fakeTermsURL})
	f.ingressCertCfg = config.Config{ACMEDirectoryURL: fake.DirectoryURL(), ACMECAFile: fake.CAFile(t)}
	f.h = f.build(t) // a restart: the database is kept, the manager is made again over the fake
	return f, fake
}

// accountResp is the install-wide certificate state as the API answers it.
type accountResp struct {
	TermsAccepted   bool   `json:"termsAccepted"`
	TermsAcceptedAt string `json:"termsAcceptedAt"`
	TermsAcceptedBy string `json:"termsAcceptedBy"`
	Directories     []struct {
		Issuer         string `json:"issuer"`
		URL            string `json:"url"`
		TermsOfService string `json:"termsOfService"`
	} `json:"directories"`
	Paused       bool   `json:"paused"`
	PausedReason string `json:"pausedReason"`
}

func decodeAccount(t *testing.T, rec *httptest.ResponseRecorder) accountResp {
	t.Helper()
	var a accountResp
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return a
}

// account reads the state as u and requires 200.
func (f *ingressFix) account(t *testing.T, u credentialUser) accountResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, certAccountPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", certAccountPath, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("GET Cache-Control = %q, want no-store", cc)
	}
	return decodeAccount(t, rec)
}

// D-36 tracer: an instance admin acknowledges the CA's terms once; the state then shows when and by whom, one audit row
// says which directories it covers, and the flag the manager's round reads is set.
func TestIngressTermsAcceptanceThroughTheAPI(t *testing.T) {
	t.Parallel()
	f, fake := newIngressCAFix(t)
	ctx := context.Background()

	before := f.account(t, f.admin)
	if before.TermsAccepted || before.TermsAcceptedAt != "" || before.TermsAcceptedBy != "" {
		t.Fatalf("fresh install: %+v, want terms not accepted", before)
	}
	if before.Paused || before.PausedReason != "" {
		t.Errorf("fresh install is paused: %+v", before)
	}
	if len(before.Directories) != 2 || before.Directories[0].Issuer != "production" || before.Directories[1].Issuer != "staging" {
		t.Fatalf("directories = %+v, want production then staging", before.Directories)
	}
	for _, d := range before.Directories {
		if d.URL != fake.DirectoryURL() || d.TermsOfService != fakeTermsURL {
			t.Errorf("directory %+v, want %s advertising %s", d, fake.DirectoryURL(), fakeTermsURL)
		}
	}
	if st, err := f.ingressCertStore.State(ctx); err != nil || !st.TermsAcceptedAt.IsZero() {
		t.Fatalf("State before = %+v, %v, want no acceptance", st, err)
	}

	now := f.clk.now()
	rec := f.credCall(t, f.admin, http.MethodPut, certAccountPath, `{"agreeTos":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT agreeTos true: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("PUT Cache-Control = %q, want no-store", cc)
	}
	if got := decodeAccount(t, rec); !got.TermsAccepted {
		t.Errorf("PUT answered %+v, want the acceptance", got)
	}

	after := f.account(t, f.admin)
	if !after.TermsAccepted || after.TermsAcceptedBy != "id-admin" {
		t.Fatalf("GET after = %+v, want accepted by id-admin", after)
	}
	if at, err := time.Parse(time.RFC3339Nano, after.TermsAcceptedAt); err != nil || !at.Equal(now) {
		t.Errorf("termsAcceptedAt = %q (%v), want the fixture's now %s", after.TermsAcceptedAt, err, now.Format(time.RFC3339))
	}

	rows := f.ingressAudit(t, audit.ActionHTTPSIngressTermsAccepted)
	want := fake.DirectoryURL() + "," + fake.DirectoryURL()
	if len(rows) != 1 || rows[0].ActorID != "id-admin" || rows[0].Result != string(audit.ResultSuccess) || rows[0].EnvID != "" || rows[0].Details["directories"] != want {
		t.Fatalf("audit rows = %+v, want one https.ingress_terms_accepted covering %s", rows, want)
	}

	// The field the manager's round reads before it orders (20-08 tests that end to end).
	st, err := f.ingressCertStore.State(ctx)
	if err != nil || st.TermsAcceptedAt.IsZero() || st.TermsAcceptedBy != "id-admin" {
		t.Errorf("State after = %+v, %v, want the acceptance by id-admin", st, err)
	}

	// Accepting again changes nothing and writes no second row.
	f.clk.t = f.clk.t.Add(time.Minute)
	if rec := f.credCall(t, f.admin, http.MethodPut, certAccountPath, `{"agreeTos":true}`); rec.Code != http.StatusOK {
		t.Fatalf("repeated PUT: %d %s, want 200", rec.Code, rec.Body)
	}
	if again := f.account(t, f.admin); again.TermsAcceptedAt != after.TermsAcceptedAt {
		t.Errorf("termsAcceptedAt moved to %q on a repeat, was %q", again.TermsAcceptedAt, after.TermsAcceptedAt)
	}
	if n := len(f.ingressAudit(t, audit.ActionHTTPSIngressTermsAccepted)); n != 1 {
		t.Errorf("%d audit rows after a repeat, want 1", n)
	}
}

// D-36: an acknowledgement without agreement is refused whatever way it is left out, nothing is stored and nothing is
// audited.
func TestIngressTermsRefusedWithoutAgreement(t *testing.T) {
	t.Parallel()
	f, _ := newIngressCAFix(t)

	rec := f.credCall(t, f.admin, http.MethodPut, certAccountPath, `{"agreeTos":false}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_terms_required" {
		t.Errorf("PUT agreeTos false: %d %s, want 422 ingress_terms_required", rec.Code, rec.Body)
	}
	// Leaving the field out is refused by the schema before the handler runs; it records nothing either.
	if rec := f.credCall(t, f.admin, http.MethodPut, certAccountPath, `{}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("PUT {}: %d %s, want 422", rec.Code, rec.Body)
	}
	if st, err := f.ingressCertStore.State(context.Background()); err != nil || !st.TermsAcceptedAt.IsZero() || st.TermsAcceptedBy != "" {
		t.Errorf("State = %+v, %v, want nothing stored", st, err)
	}
	if a := f.account(t, f.admin); a.TermsAccepted || a.TermsAcceptedAt != "" {
		t.Errorf("GET = %+v, want terms not accepted", a)
	}
	if n := len(f.ingressAudit(t, audit.ActionHTTPSIngressTermsAccepted)); n != 0 {
		t.Errorf("%d audit rows, want none for a refusal", n)
	}

	// A directory that cannot be read just now leaves its terms empty and does not fail the read.
	g, gfake := newIngressCAFix(t)
	gfake.FailNext(acmetest.EndpointDirectory, acmetest.Problem{Status: 503, Type: "serverInternal", Detail: "maintenance"})
	a := g.account(t, g.admin)
	if len(a.Directories) != 2 {
		t.Fatalf("directories = %+v, want both listed", a.Directories)
	}
	if a.Directories[0].TermsOfService != "" || a.Directories[0].URL != gfake.DirectoryURL() {
		t.Errorf("unreadable production directory = %+v, want its URL and empty terms", a.Directories[0])
	}
}

// D-32: after a restore the state says it is paused and why; resuming lifts it, is audited with the reason that was
// lifted, and a second resume finds nothing to lift.
func TestIngressResumeAfterRestore(t *testing.T) {
	t.Parallel()
	f, _ := newIngressCAFix(t)
	if _, err := f.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if a := f.account(t, f.admin); !a.Paused || a.PausedReason != "restore" {
		t.Fatalf("GET while paused = %+v, want paused with reason restore", a)
	}

	rec := f.credCall(t, f.admin, http.MethodPost, certResumePath, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("resume: %d %s, want 204", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("resume Cache-Control = %q, want no-store", cc)
	}
	if a := f.account(t, f.admin); a.Paused || a.PausedReason != "" {
		t.Errorf("GET after resume = %+v, want not paused", a)
	}
	if st, err := f.ingressCertStore.State(context.Background()); err != nil || st.PausedReason != "" {
		t.Errorf("State after resume = %+v, %v, want the pause cleared", st, err)
	}
	rows := f.ingressAudit(t, audit.ActionHTTPSIngressResumed)
	if len(rows) != 1 || rows[0].ActorID != "id-admin" || rows[0].Details["reason"] != "restore" || rows[0].EnvID != "" || rows[0].Result != string(audit.ResultSuccess) {
		t.Fatalf("audit rows = %+v, want one https.ingress_resumed with reason restore", rows)
	}

	rec = f.credCall(t, f.admin, http.MethodPost, certResumePath, "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_certs_not_paused" {
		t.Errorf("resume when not paused: %d %s, want 409 ingress_certs_not_paused", rec.Code, rec.Body)
	}
	if n := len(f.ingressAudit(t, audit.ActionHTTPSIngressResumed)); n != 1 {
		t.Errorf("%d audit rows after a refused resume, want 1", n)
	}
}

// D-37, T-20-40: the three routes are instance settings. An environment's Admin (ingress.manage) and an Operator are
// refused on every one, and what they tried changes nothing; the instance admin is let through.
func TestIngressInstallRoutesNeedGlobalSettingsPermissions(t *testing.T) {
	t.Parallel()
	f, _ := newIngressCAFix(t)
	if _, err := f.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	routes := []struct {
		name, method, path, body string
		ok                       int
	}{
		{"read", http.MethodGet, certAccountPath, "", http.StatusOK},
		{"accept", http.MethodPut, certAccountPath, `{"agreeTos":true}`, http.StatusOK},
		{"resume", http.MethodPost, certResumePath, "", http.StatusNoContent},
	}
	for _, r := range routes {
		for who, u := range map[string]credentialUser{"the Admin of A": f.adminA, "an Operator of A": f.operatorA, "the Admin of B": f.adminB} {
			rec := f.credCall(t, u, r.method, r.path, r.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s as %s: %d %s, want 403", r.name, who, rec.Code, rec.Body)
			}
		}
	}
	st, err := f.ingressCertStore.State(context.Background())
	if err != nil || !st.TermsAcceptedAt.IsZero() || st.PausedReason != "restore" {
		t.Errorf("State after the refusals = %+v, %v, want nothing changed", st, err)
	}
	for _, action := range []string{audit.ActionHTTPSIngressTermsAccepted, audit.ActionHTTPSIngressResumed} {
		if n := len(f.ingressAudit(t, action)); n != 0 {
			t.Errorf("%d %s rows after the refusals, want none", n, action)
		}
	}

	for _, r := range routes {
		rec := f.credCall(t, f.admin, r.method, r.path, r.body)
		if rec.Code != r.ok {
			t.Errorf("%s as the instance admin: %d %s, want %d", r.name, rec.Code, rec.Body, r.ok)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", r.name, cc)
		}
	}
}
