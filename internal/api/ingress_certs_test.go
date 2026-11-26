// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingresscert"
)

// The per-hostname certificate routes (Phase 20, CRT-03, CRT-05, CRT-06, D-33).

// certsPath is a route of one environment's certificates.
func certsPath(env string, rest ...string) string {
	return "/api/env/" + env + "/ingress/certificates" + strings.Join(rest, "")
}

// certResp is a certificate as the API answers it.
type certResp struct {
	ID                  string `json:"id"`
	Hostname            string `json:"hostname"`
	Issuer              string `json:"issuer"`
	Staging             bool   `json:"staging"`
	Source              string `json:"source"`
	State               string `json:"state"`
	StateText           string `json:"stateText"`
	Ready               bool   `json:"ready"`
	RequestedBy         string `json:"requestedBy"`
	RequestedAt         string `json:"requestedAt"`
	ConsecutiveFailures int    `json:"consecutiveFailures"`
}

func decodeCert(t *testing.T, rec *httptest.ResponseRecorder) certResp {
	t.Helper()
	var c certResp
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return c
}

func decodeCerts(t *testing.T, rec *httptest.ResponseRecorder) []certResp {
	t.Helper()
	var l struct {
		Items []certResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return l.Items
}

func certRequestBody(host string) string {
	b, _ := json.Marshal(map[string]string{"hostname": host})
	return string(b)
}

// request asks for a certificate ahead as u and requires 201.
func (f *ingressFix) requestCert(t *testing.T, u credentialUser, env, host string) certResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, certsPath(env), certRequestBody(host))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request %s in %s: %d %s", host, env, rec.Code, rec.Body)
	}
	return decodeCert(t, rec)
}

// CRT-03 tracer: an Admin asks for a certificate before any stack claims the name, sees it queued and listed, and the
// request is audited in the same transaction; nothing returned or recorded is key material.
func TestIngressCertRequestAheadThenList(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA), certRequestBody("web.apps.example.com"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("request: %d %s, want 201", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	c := decodeCert(t, rec)
	if c.ID == "" || c.Hostname != "web.apps.example.com" || c.State != "queued" || c.StateText != "Queued" || c.Source != "acme" ||
		c.Issuer != "production" || c.Staging || c.Ready || c.RequestedBy != "id-ing-admin-a" || c.RequestedAt == "" {
		t.Errorf("certificate = %+v", c)
	}
	bodies := rec.Body.String()

	rec = f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s, want 200", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("list Cache-Control = %q, want no-store", cc)
	}
	items := decodeCerts(t, rec)
	if len(items) != 1 || items[0].ID != c.ID || items[0].State != "queued" || items[0].Hostname != "web.apps.example.com" {
		t.Errorf("list = %+v, want the one requested certificate", items)
	}
	bodies += rec.Body.String()

	rows := f.ingressAudit(t, audit.ActionIngressCertRequested)
	if len(rows) != 1 {
		t.Fatalf("ingress.cert_requested rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_certificate" || r.TargetID != c.ID || r.EnvID != f.envA ||
		r.Details["hostname"] != "web.apps.example.com" || r.Details["issuer"] != "production" {
		t.Errorf("audit row = %+v", r)
	}
	for name, text := range map[string]string{"the answers": bodies, "the audit log": f.everyAuditText(t)} {
		if strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("%s hold key material", name)
		}
	}
}

// setCert changes columns of a certificate row directly, as the manager's own writes would have left them.
func (f *ingressFix) setCert(t *testing.T, id, set string, args ...any) {
	t.Helper()
	if _, err := f.st.Write.Exec(`UPDATE ingress_certs SET `+set+` WHERE id = ?`, append(args, id)...); err != nil { //nolint:gosec // G202: set is a literal column list from this file
		t.Fatalf("update certificate %s: %v", id, err)
	}
}

// blockedBody is the part of a 429 the tests read.
type blockedBody struct {
	Type   string `json:"type"`
	Errors []struct {
		Message string `json:"message"`
		Value   string `json:"value"`
	} `json:"errors"`
}

// D-13, CRT-05: a retry inside a CA wait or the failure guard is a 429 with the instant it reopens; with a free ledger it
// is a 202 that is audited; a stopped certificate, a pause and another environment's id each have their own answer.
func TestIngressCertRetryAnswersBlockedPausedAndNotFound(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminB, f.envB, "other.example.com")
	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	other := f.requestCert(t, f.adminB, f.envB, "web.other.example.com")
	retry := func(u credentialUser, env, id string) *httptest.ResponseRecorder {
		return f.credCall(t, u, http.MethodPost, certsPath(env, "/", id, "/retry"), "")
	}
	now := f.clk.now()

	// Inside a CA wait: 429, Retry-After in whole seconds, the instant in errors[0].value, the reason told_to_wait.
	until := now.Add(30 * time.Minute)
	f.setCert(t, c.ID, `retry_after_until = ?`, until.UnixNano())
	rec := retry(f.adminA, f.envA, c.ID)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "ingress_cert_blocked" {
		t.Fatalf("retry in a CA wait: %d %s, want 429 ingress_cert_blocked", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1800" {
		t.Errorf("Retry-After = %q, want 1800", got)
	}
	var b blockedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || len(b.Errors) != 1 ||
		b.Errors[0].Message != "told_to_wait" || b.Errors[0].Value != until.UTC().Format(time.RFC3339) {
		t.Errorf("errors = %+v (%v), want told_to_wait at %s", b.Errors, err, until.UTC().Format(time.RFC3339))
	}

	// Three failures inside the hour: held until one hour after the oldest of them.
	f.setCert(t, c.ID, `retry_after_until = 0, consecutive_failures = 3, failure_times = ?`,
		fmt.Sprintf("[%d,%d,%d]", now.Add(-50*time.Minute).UnixNano(), now.Add(-40*time.Minute).UnixNano(), now.Add(-30*time.Minute).UnixNano()))
	rec = retry(f.adminA, f.envA, c.ID)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "ingress_cert_blocked" {
		t.Fatalf("retry after three failures: %d %s, want 429", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After = %q, want 600 (the oldest failure plus one hour)", got)
	}
	b = blockedBody{}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if len(b.Errors) != 1 || b.Errors[0].Message != "failure_guard" || b.Errors[0].Value != now.Add(10*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("errors = %+v, want failure_guard at +10m", b.Errors)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertRetried); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none for refused retries", rows)
	}

	// A free ledger: 202, audited, and the row is due now.
	f.setCert(t, c.ID, `consecutive_failures = 0, failure_times = '[]', next_attempt_at = ?`, now.Add(5*time.Hour).UnixNano())
	rec = retry(f.adminA, f.envA, c.ID)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("retry with a free ledger: %d %s, want 202", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	rows := f.ingressAudit(t, audit.ActionIngressCertRetried)
	if len(rows) != 1 || rows[0].TargetID != c.ID || rows[0].TargetType != "ingress_certificate" || rows[0].ActorID != "id-ing-admin-a" ||
		rows[0].EnvID != f.envA || rows[0].Details["hostname"] != "web.apps.example.com" {
		t.Errorf("audit rows = %+v, want one ingress.cert_retried for the certificate", rows)
	}
	var next int64
	if err := f.st.Read.QueryRow(`SELECT next_attempt_at FROM ingress_certs WHERE id = ?`, c.ID).Scan(&next); err != nil || next != now.UnixNano() {
		t.Errorf("next_attempt_at = %d, %v, want now (%d)", next, err, now.UnixNano())
	}

	// A stopped certificate waits for a claim or a request, not a retry.
	f.setCert(t, c.ID, `phase = 'stopped', stopped_reason = 'unreferenced'`)
	if rec := retry(f.adminA, f.envA, c.ID); rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_cert_stopped" {
		t.Errorf("retry of a stopped certificate: %d %s, want 409 ingress_cert_stopped", rec.Code, rec.Body)
	}
	f.setCert(t, c.ID, `phase = 'queued', stopped_reason = ''`)

	// Paused after a restore.
	if _, err := f.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = 'restore' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if rec := retry(f.adminA, f.envA, c.ID); rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_certs_paused" {
		t.Errorf("retry while paused: %d %s, want 409 ingress_certs_paused", rec.Code, rec.Body)
	}
	if _, err := f.st.Write.Exec(`UPDATE ingress_cert_state SET paused_reason = '' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	// Another environment's id, and one that does not exist, are the same 404; the other environment's path is a 403.
	for _, id := range []string{other.ID, "no-such-certificate"} {
		if rec := retry(f.adminA, f.envA, id); rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_cert_not_found" {
			t.Errorf("retry of %s from A: %d %s, want 404 ingress_cert_not_found", id, rec.Code, rec.Body)
		}
	}
	// An environment the caller has no role on is hidden as not found, as everywhere else; either way nothing happens.
	if rec := retry(f.adminA, f.envB, other.ID); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("retry in B as A's Admin: %d %s, want 403 or 404", rec.Code, rec.Body)
	}
	if n := len(f.ingressAudit(t, audit.ActionIngressCertRetried)); n != 1 {
		t.Errorf("%d retry audit rows at the end, want the one success", n)
	}
}

// budgetResp is a budget row as the API answers it.
type budgetResp struct {
	RegisteredDomain string   `json:"registeredDomain"`
	Issuer           string   `json:"issuer"`
	Used             int      `json:"used"`
	Cap              int      `json:"cap"`
	Limit            int      `json:"limit"`
	Headroom         int      `json:"headroom"`
	NextSlotAt       string   `json:"nextSlotAt"`
	Domains          []string `json:"domains"`
	Shared           bool     `json:"shared"`
}

func (f *ingressFix) budget(t *testing.T, u credentialUser, env string) []budgetResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, certsPath(env, "/budget"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("budget of %s: %d %s", env, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("budget Cache-Control = %q, want no-store", cc)
	}
	var l struct {
		Items []budgetResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return l.Items
}

// D-18, CRT-06: one row per registered domain and issuer with the figures, the next slot once the budget is used, the
// environment's own suffixes and whether another domain shares it, which the answer never names.
func TestIngressCertBudget(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	// An environment with no domain has no rows, and the list is an empty array, not null.
	rec := f.credCall(t, f.adminB, http.MethodGet, certsPath(f.envB, "/budget"), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("budget with no domain: %d %s, want 200 with an empty list", rec.Code, rec.Body)
	}

	f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminA, f.envA, "lab.example.com")
	rows := f.budget(t, f.adminA, f.envA)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one for example.com on production", rows)
	}
	want := budgetResp{
		RegisteredDomain: "example.com", Issuer: "production", Used: 0, Cap: ingresscert.NewCertsCap, Limit: ingresscert.NewCertsLimit,
		Headroom: ingresscert.NewCertsCap, Domains: []string{"apps.example.com", "lab.example.com"},
	}
	if g := rows[0]; g.RegisteredDomain != want.RegisteredDomain || g.Issuer != want.Issuer || g.Used != 0 || g.Cap != want.Cap || g.Limit != want.Limit ||
		g.Headroom != want.Headroom || g.NextSlotAt != "" || g.Shared || strings.Join(g.Domains, ",") != strings.Join(want.Domains, ",") {
		t.Errorf("row = %+v, want %+v", g, want)
	}
	// The rows are exactly what the manager reports.
	got, err := f.ingressCerts.Budget(context.Background(), f.envA)
	if err != nil || len(got) != 1 || got[0].Used != rows[0].Used || got[0].Headroom != rows[0].Headroom {
		t.Errorf("Manager.Budget = %+v, %v", got, err)
	}

	// Another environment's domain under the same registered domain makes the budget shared, and names nothing of it.
	f.register(t, f.adminB, f.envB, "secret-team.example.com")
	rows = f.budget(t, f.adminA, f.envA)
	if len(rows) != 1 || !rows[0].Shared {
		t.Errorf("rows = %+v, want shared", rows)
	}
	if rec := f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA, "/budget"), ""); strings.Contains(rec.Body.String(), "secret-team") {
		t.Errorf("the budget names another environment's domain: %s", rec.Body)
	}

	// Forty new certificates in the window use the budget: no headroom, and the next slot is the oldest plus a week.
	now := f.clk.now()
	oldest := now.Add(-6 * 24 * time.Hour)
	for i := 0; i < ingresscert.NewCertsCap; i++ {
		at := oldest.Add(time.Duration(i) * time.Minute)
		if _, err := f.st.Write.Exec(
			`INSERT INTO ingress_cert_issuances (id, issuer, registered_domain, hostname, kind, issued_at) VALUES (?, 'production', 'example.com', ?, 'new', ?)`,
			fmt.Sprintf("iss-%d", i), fmt.Sprintf("h%d.apps.example.com", i), at.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	rows = f.budget(t, f.adminA, f.envA)
	if len(rows) != 1 || rows[0].Used != ingresscert.NewCertsCap || rows[0].Headroom != 0 {
		t.Fatalf("rows = %+v, want the budget used", rows)
	}
	slot, err := time.Parse(time.RFC3339Nano, rows[0].NextSlotAt)
	if err != nil || !slot.Equal(oldest.Add(ingresscert.Window)) {
		t.Errorf("next slot = %q (%v), want %s", rows[0].NextSlotAt, err, oldest.Add(ingresscert.Window))
	}
}

// D-08, D-33: a request that policy refuses answers with the ingress problem code and nothing is written.
func TestIngressCertRequestAheadRefusals(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	domA := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminA, f.envA, "shared.example.com")
	f.register(t, f.adminB, f.envB, "shared.example.com")

	// B asks first; A is then refused the same name without being told whose it is.
	f.requestCert(t, f.adminB, f.envB, "web.shared.example.com")
	rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA), certRequestBody("web.shared.example.com"))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_host_taken" {
		t.Errorf("a name another environment asked for: %d %s, want 409 ingress_host_taken", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), f.envB) || strings.Contains(rec.Body.String(), "ing-admin-b") {
		t.Errorf("the refusal names the other environment: %s", rec.Body)
	}

	// A reserved name.
	rec = f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA, "/", domA.ID, "/reserved"), certRequestBody("vault.apps.example.com"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	cases := []struct {
		name, host, code string
		status           int
	}{
		{"a reserved name", "vault.apps.example.com", "ingress_host_reserved", http.StatusConflict},
		{"a name under no registered domain", "web.elsewhere.org", "ingress_domain_not_allowed", http.StatusUnprocessableEntity},
		{"the domain itself", "apps.example.com", "ingress_domain_not_allowed", http.StatusUnprocessableEntity},
		{"a wildcard", "*.apps.example.com", "ingress_host_invalid", http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA), certRequestBody(tc.host))
		if rec.Code != tc.status || problemType(t, rec) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, rec.Code, rec.Body, tc.status, tc.code)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM ingress_certs WHERE env_id = ?`, f.envA).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d certificate rows of A, %v, want none written by refusals", n, err)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertRequested); len(rows) != 1 || rows[0].EnvID != f.envB {
		t.Errorf("audit rows = %+v, want only B's request", rows)
	}

	// Asking again for a name A already has reuses its row.
	first := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	again := f.requestCert(t, f.adminA, f.envA, "WEB.apps.example.com.")
	if again.ID != first.ID {
		t.Errorf("a second request made certificate %s, want the first (%s) reused", again.ID, first.ID)
	}
}

// T-20-34, D-33, D-14: the request and the retry need ingress.manage on the environment, so an Operator gets 403 on them;
// the list and the budget need ingress.read, so an Operator reads them; the Admin of another environment cannot see this
// one at all, and every answer is no-store.
func TestIngressCertRoutesNeedTheAdminOfTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	routes := []struct {
		name, method, path, body string
		ok                       int
		read                     bool
	}{
		{"list", http.MethodGet, certsPath(f.envA), "", http.StatusOK, true},
		{"request", http.MethodPost, certsPath(f.envA), certRequestBody("api.apps.example.com"), http.StatusCreated, false},
		{"retry", http.MethodPost, certsPath(f.envA, "/", c.ID, "/retry"), "", http.StatusAccepted, false},
		{"budget", http.MethodGet, certsPath(f.envA, "/budget"), "", http.StatusOK, true},
	}
	for _, r := range routes {
		for who, u := range map[string]credentialUser{"an Operator of A": f.operatorA, "the Admin of B": f.adminB} {
			rec := f.credCall(t, u, r.method, r.path, r.body)
			// The Operator sees the environment: the two reads (ingress.read) answer, the writes are refused (403); the
			// Admin of B cannot see it at all (404).
			switch {
			case who == "an Operator of A" && r.read:
				if rec.Code != http.StatusOK {
					t.Errorf("%s as %s: %d %s, want 200 (a read on ingress.read)", r.name, who, rec.Code, rec.Body)
				}
			case rec.Code != http.StatusForbidden && (who == "an Operator of A" || rec.Code != http.StatusNotFound):
				t.Errorf("%s as %s: %d %s, want 403 (404 for an environment the caller cannot see)", r.name, who, rec.Code, rec.Body)
			}
		}
		rec := f.credCall(t, f.adminA, r.method, r.path, r.body)
		if rec.Code != r.ok {
			t.Errorf("%s as the Admin of A: %d %s, want %d", r.name, rec.Code, rec.Body, r.ok)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", r.name, cc)
		}
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertRequested); len(rows) != 2 {
		t.Errorf("request audit rows = %d, want 2: refused callers write none", len(rows))
	}
}

// D-14 tracer: the certificate list and the weekly budget are readable from Viewer up; the Admin of another environment
// still cannot see this one, and an anonymous caller is 401.
func TestIngressCertificatesReadableByViewers(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	viewer := f.asRoleOn(t, "ing-viewer-a", f.envA, "viewer")

	for _, path := range []string{certsPath(f.envA), certsPath(f.envA, "/budget")} {
		for who, u := range map[string]credentialUser{"a Viewer of A": viewer, "an Operator of A": f.operatorA} {
			rec := f.credCall(t, u, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s as %s: %d %s, want 200", path, who, rec.Code, rec.Body)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("GET %s as %s: Cache-Control = %q, want no-store", path, who, cc)
			}
			var l struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || len(l.Items) != 1 {
				t.Errorf("GET %s as %s: items = %d (%v), want 1: %s", path, who, len(l.Items), err, rec.Body)
			}
		}
		if rec := f.credCall(t, f.adminB, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as the Admin of B: %d %s, want 404 (an environment the caller cannot see)", path, rec.Code, rec.Body)
		}
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s anonymous: %d %s, want 401", path, rec.Code, rec.Body)
		}
	}
}

// A-04, T-27-11: lastError.detail and requestedBy are filled only for a caller who holds ingress.manage on the
// environment; a Viewer gets the same row with those two fields empty and nothing else different.
func TestIngressCertificateDetailOnlyForManagers(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")
	const detail = "ca said: rate limited for example.com"
	f.setCert(t, c.ID, `consecutive_failures = 1, last_error_class = 'rate_limited', last_error_detail = ?, last_error_at = ?`,
		detail, f.clk.now().Add(-time.Minute).UnixNano())
	viewer := f.asRoleOn(t, "ing-viewer-a", f.envA, "viewer")

	row := func(u credentialUser) map[string]any {
		t.Helper()
		rec := f.credCall(t, u, http.MethodGet, certsPath(f.envA), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list: %d %s, want 200", rec.Code, rec.Body)
		}
		var l struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || len(l.Items) != 1 {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
		return l.Items[0]
	}
	admin, view := row(f.adminA), row(viewer)

	ae, _ := admin["lastError"].(map[string]any)
	if ae["detail"] != detail || admin["requestedBy"] != "id-ing-admin-a" {
		t.Fatalf("the Admin's row = %v, want the detail and requestedBy filled", admin)
	}
	ve, _ := view["lastError"].(map[string]any)
	if ve == nil || ve["class"] != "rate_limited" || ve["hint"] == "" || ve["at"] == nil {
		t.Fatalf("the Viewer's lastError = %v, want class, hint and at", view["lastError"])
	}
	if d, ok := ve["detail"]; ok && d != "" {
		t.Errorf("the Viewer read the detail %q", d)
	}
	if rb, ok := view["requestedBy"]; ok && rb != "" {
		t.Errorf("the Viewer read requestedBy %q", rb)
	}
	// Blank the two fields on the Admin's row: what is left is the Viewer's row.
	delete(ae, "detail")
	delete(admin, "requestedBy")
	delete(ve, "detail")
	delete(view, "requestedBy")
	if !reflect.DeepEqual(admin, view) {
		t.Errorf("the rows differ beyond the two fields:\nadmin  %v\nviewer %v", admin, view)
	}
}

// UIX-03 edges: at the injected instant T a certificate that expired one second ago is expired, one due to renew
// exactly at T keeps that renewAt and is not expired, and the list is by hostname whatever the order of creation.
func TestIngressCertificatesClockEdgesAndOrder(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "example.com")
	zz := f.requestCert(t, f.adminA, f.envA, "zz.example.com")
	aa := f.requestCert(t, f.adminA, f.envA, "aa.example.com")
	mm := f.requestCert(t, f.adminA, f.envA, "mm.example.com")
	now := f.clk.now().UTC()
	f.setCert(t, aa.ID, `chain_pem = 'chain', not_after = ?`, now.Add(-time.Second).UnixNano())
	f.setCert(t, mm.ID, `chain_pem = 'chain', not_after = ?, renew_at = ?`, now.Add(30*24*time.Hour).UnixNano(), now.UnixNano())
	_ = zz

	rec := f.credCall(t, f.adminA, http.MethodGet, certsPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s, want 200", rec.Code, rec.Body)
	}
	var l struct {
		Items []struct {
			Hostname string `json:"hostname"`
			State    string `json:"state"`
			RenewAt  string `json:"renewAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil || len(l.Items) != 3 {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	var order []string
	for _, it := range l.Items {
		order = append(order, it.Hostname)
	}
	if want := []string{"aa.example.com", "mm.example.com", "zz.example.com"}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if l.Items[0].State != "expired" {
		t.Errorf("a certificate that expired one second ago is %q, want expired", l.Items[0].State)
	}
	if l.Items[1].State == "expired" || l.Items[1].RenewAt != now.Format(time.RFC3339Nano) {
		t.Errorf("a certificate renewing exactly now: state %q renewAt %q, want not expired and renewAt %s", l.Items[1].State, l.Items[1].RenewAt, now.Format(time.RFC3339Nano))
	}
}
