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
