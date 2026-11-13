// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/config"
)

// The audit rows of the HTTPS settings an admin saves and of the Test token press (Phase 17, SET-04, D-09 bullets 2
// and 6, D-11, D-13, D-14).

// requireNoSecretInRows fails when any https.* row, in whole, holds one of the texts: a typed token or a typed entry.
func requireNoSecretInRows(t *testing.T, rows []httpsRow, secrets ...string) {
	t.Helper()
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if strings.Contains(string(b), s) {
				t.Errorf("row %s/%s holds %q: %s", r.Action, r.Result, s, b)
			}
		}
	}
}

// settingsRows is the https.settings_updated rows of the log, oldest first.
func (f *acmeFix) settingsRows(t *testing.T) []httpsRow {
	t.Helper()
	return rowsOf(f.httpsRows(t), audit.ActionHTTPSSettingsUpdated)
}

// wantSettingsRows requires exactly n settings rows and returns them.
func (f *acmeFix) wantSettingsRows(t *testing.T, n int) []httpsRow {
	t.Helper()
	rows := f.settingsRows(t)
	if len(rows) != n {
		t.Fatalf("%d https.settings_updated rows, want %d: %+v", len(rows), n, rows)
	}
	return rows
}

func TestHTTPSAuditSettings(t *testing.T) {
	t.Parallel()

	t.Run("one save of several fields is one row, then a token-only save, then a no-op", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["`+f.srv.Addr()+`"]}`))
		if !got.Saved || len(got.Resolvers) != 1 {
			t.Fatalf("answer = %+v, want saved with one resolver", got)
		}

		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "success")
		d := rows[0].Details
		if d["section"] != "validation" || d["fields"] != "challenge,resolvers,dns_credential" {
			t.Errorf("details = %+v, want section validation and every changed field in order", d)
		}
		if d["challenge_from"] != "http-01" || d["challenge_to"] != "dns-01" || d["resolvers_from"] != "" || d["resolvers_to"] != got.Resolvers[0] {
			t.Errorf("details = %+v, want http-01 to dns-01 and no resolvers to %s", d, got.Resolvers[0])
		}
		if d["dns_credential"] != "set" {
			t.Errorf("dns_credential = %q, want set: no token was saved before", d["dns_credential"])
		}

		// The same token typed again replaces the saved one and nothing else changes.
		decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
		rows = f.wantSettingsRows(t, 2)
		requireAdminRow(t, rows[1], audit.ActionHTTPSSettingsUpdated, "success")
		d = rows[1].Details
		if d["section"] != "dns_credential" || d["fields"] != "dns_credential" || d["dns_credential"] != "replaced" {
			t.Errorf("details = %+v, want section dns_credential with the credential replaced", d)
		}
		for _, k := range []string{"challenge_from", "challenge_to", "resolvers_from", "resolvers_to"} {
			if _, ok := d[k]; ok {
				t.Errorf("details hold %s although that field did not change: %+v", k, d)
			}
		}

		// An empty token field and nothing else changed leaves the stored token and writes no row.
		decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01"}`))
		f.wantSettingsRows(t, 2)
		if !f.status(t).ACME.DNS.TokenSaved {
			t.Error("the stored token is gone after a save with an empty token field")
		}
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIToken)
	})

	t.Run("a challenge-only save names only the challenge", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
		decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01"}`))

		rows := f.wantSettingsRows(t, 2)
		d := rows[1].Details
		if d["section"] != "validation" || d["fields"] != "challenge" || d["challenge_from"] != "dns-01" || d["challenge_to"] != "http-01" {
			t.Errorf("details = %+v, want only the challenge, dns-01 to http-01", d)
		}
		if _, ok := d["dns_credential"]; ok {
			t.Errorf("details = %+v, want no credential entry for a save that typed no token", d)
		}
	})

	t.Run("a typed token that fails its test is one failure row with the classes and no value", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIWrongToken+`"}`))
		if got.Saved {
			t.Fatalf("answer = %+v, want nothing saved", got)
		}

		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "token_test_failed" || rows[0].Details["section"] != "dns_credential" || rows[0].Details["failed_classes"] != "dns_token_invalid" {
			t.Errorf("row = reason %q details %+v, want token_test_failed, section dns_credential, class dns_token_invalid", rows[0].Reason, rows[0].Details)
		}
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIWrongToken)
	})

	t.Run("a resolver the server rejects is one failure row that never holds the typed entry", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		rec := f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`","resolvers":["bad resolver ;;7709"]}`)
		acmeProblemOf(t, rec, http.StatusUnprocessableEntity, "acme_dns_resolver_invalid")

		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "acme_dns_resolver_invalid" || len(rows[0].Details) != 0 {
			t.Errorf("row = reason %q details %+v, want acme_dns_resolver_invalid and no details", rows[0].Reason, rows[0].Details)
		}
		requireNoSecretInRows(t, f.httpsRows(t), "7709", "bad resolver", dnsAPIToken)
	})

	t.Run("a refused save: DNS-01 with no token anywhere is a failure row with the problem type", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		acmeProblemOf(t, f.putValidation(t, `{"challengeType":"dns-01"}`), http.StatusUnprocessableEntity, "acme_dns_token_required")

		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "acme_dns_token_required" {
			t.Errorf("reason = %q, want acme_dns_token_required", rows[0].Reason)
		}
	})

	t.Run("an echo of TIERPLAN_ACME_CHALLENGE is not a change and writes no row", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{Challenge: acme.ChallengeDNS01}})
		got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01"}`))
		if !got.Saved {
			t.Fatalf("answer = %+v, want the echo accepted", got)
		}
		f.wantSettingsRows(t, 0)

		// Any other value is refused under the lock: that is a failure row.
		acmeProblemOf(t, f.putValidation(t, `{"challengeType":"http-01"}`), http.StatusConflict, "acme_challenge_locked")
		rows := f.wantSettingsRows(t, 1)
		if rows[0].Result != "failure" || rows[0].Reason != "acme_challenge_locked" {
			t.Errorf("row = %s/%s, want a failure for acme_challenge_locked", rows[0].Result, rows[0].Reason)
		}
	})
}

// profileFix is the fixture over a CA that offers the profiles classic and shortlived.
func profileFix(t *testing.T, env config.ACMEEnv) *acmeFix {
	t.Helper()
	return newACMEFix(t, acmeOpts{env: env, fake: acmetest.Options{Profiles: map[string]time.Duration{
		"classic": 90 * 24 * time.Hour, "shortlived": 160 * time.Hour,
	}}})
}

func TestHTTPSAuditSettingsProfile(t *testing.T) {
	t.Parallel()

	t.Run("a change is one row with the old and new profile, the same profile again is none", func(t *testing.T) {
		t.Parallel()
		f := profileFix(t, config.ACMEEnv{})
		put := func(profile string) *httptest.ResponseRecorder {
			return f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"`+profile+`"}`)
		}
		if rec := put("shortlived"); rec.Code != http.StatusOK {
			t.Fatalf("PUT shortlived: %d %s", rec.Code, rec.Body)
		}
		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "success")
		d := rows[0].Details
		if d["section"] != "profile" || d["fields"] != "profile" || d["profile_from"] != "" || d["profile_to"] != "shortlived" {
			t.Errorf("details = %+v, want section profile and the CA default to shortlived", d)
		}

		if rec := put("shortlived"); rec.Code != http.StatusOK {
			t.Fatalf("PUT shortlived again: %d %s", rec.Code, rec.Body)
		}
		f.wantSettingsRows(t, 1)

		if rec := put("classic"); rec.Code != http.StatusOK {
			t.Fatalf("PUT classic: %d %s", rec.Code, rec.Body)
		}
		rows = f.wantSettingsRows(t, 2)
		if d := rows[1].Details; d["profile_from"] != "shortlived" || d["profile_to"] != "classic" {
			t.Errorf("details = %+v, want shortlived to classic", d)
		}
	})

	t.Run("a profile the CA does not offer is a failure row with the problem type and not the typed name", func(t *testing.T) {
		t.Parallel()
		f := profileFix(t, config.ACMEEnv{})
		acmeProblemOf(t, f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"typed-profile-4417"}`), http.StatusUnprocessableEntity, "acme_profile_unknown")
		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "acme_profile_unknown" || len(rows[0].Details) != 0 {
			t.Errorf("row = reason %q details %+v, want acme_profile_unknown and no details", rows[0].Reason, rows[0].Details)
		}
		requireNoSecretInRows(t, f.httpsRows(t), "typed-profile-4417")
	})

	t.Run("an echo of TIERPLAN_ACME_PROFILE writes no row and another value is a locked failure", func(t *testing.T) {
		t.Parallel()
		f := profileFix(t, config.ACMEEnv{Profile: "shortlived"})
		if rec := f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"shortlived"}`); rec.Code != http.StatusOK {
			t.Fatalf("PUT the variable's profile: %d %s", rec.Code, rec.Body)
		}
		f.wantSettingsRows(t, 0)

		acmeProblemOf(t, f.send(t, http.MethodPut, "/api/https/acme/profile", `{"profile":"classic"}`), http.StatusConflict, "acme_profile_locked")
		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "acme_profile_locked" {
			t.Errorf("reason = %q, want acme_profile_locked", rows[0].Reason)
		}
	})
}

func TestHTTPSAuditSettingsHSTS(t *testing.T) {
	t.Parallel()
	put := func(f *acmeFix, on string) *httptest.ResponseRecorder {
		return f.send(t, http.MethodPut, "/api/https/hsts", `{"enabled":`+on+`}`)
	}

	t.Run("off to on and on to off are one row each, on to on is none", func(t *testing.T) {
		t.Parallel()
		f := newHSTSFix(t, &stubHSTS{})
		if rec := put(f, "true"); rec.Code != http.StatusOK {
			t.Fatalf("PUT on: %d %s", rec.Code, rec.Body)
		}
		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "success")
		if d := rows[0].Details; d["section"] != "hsts" || d["fields"] != "hsts" || d["hsts_from"] != "off" || d["hsts_to"] != "on" {
			t.Errorf("details = %+v, want section hsts, off to on", d)
		}
		if rec := put(f, "true"); rec.Code != http.StatusOK {
			t.Fatalf("PUT on again: %d %s", rec.Code, rec.Body)
		}
		f.wantSettingsRows(t, 1)
		if rec := put(f, "false"); rec.Code != http.StatusOK {
			t.Fatalf("PUT off: %d %s", rec.Code, rec.Body)
		}
		rows = f.wantSettingsRows(t, 2)
		if d := rows[1].Details; d["hsts_from"] != "on" || d["hsts_to"] != "off" {
			t.Errorf("details = %+v, want on to off", d)
		}
	})

	t.Run("an echo of TIERPLAN_HSTS writes no row", func(t *testing.T) {
		t.Parallel()
		lock := &api.HTTPSLockBody{Var: "TIERPLAN_HSTS", Value: "on", Stored: "off"}
		f := newHSTSFix(t, &stubHSTS{view: api.HSTSView{Enabled: true, Lock: lock}})
		if rec := put(f, "true"); rec.Code != http.StatusOK {
			t.Fatalf("PUT the variable's value: %d %s", rec.Code, rec.Body)
		}
		f.wantSettingsRows(t, 0)
	})

	t.Run("the other value under the lock is a failure row", func(t *testing.T) {
		t.Parallel()
		f := newHSTSFix(t, &stubHSTS{setErr: &api.ErrHSTSLocked{Var: "TIERPLAN_HSTS"}})
		acmeProblemOf(t, put(f, "false"), http.StatusConflict, "hsts_locked")
		rows := f.wantSettingsRows(t, 1)
		requireAdminRow(t, rows[0], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[0].Reason != "hsts_locked" || len(rows[0].Details) != 0 {
			t.Errorf("row = reason %q details %+v, want hsts_locked and no details", rows[0].Reason, rows[0].Details)
		}
	})

	t.Run("a storage failure is a failure row with the reason internal", func(t *testing.T) {
		t.Parallel()
		f := newHSTSFix(t, &stubHSTS{setErr: errors.New("disk is full")})
		if rec := put(f, "true"); rec.Code != http.StatusInternalServerError {
			t.Fatalf("PUT: %d %s, want 500", rec.Code, rec.Body)
		}
		rows := f.wantSettingsRows(t, 1)
		if rows[0].Result != "failure" || rows[0].Reason != "internal" {
			t.Errorf("row = %s/%s, want failure/internal", rows[0].Result, rows[0].Reason)
		}
		requireNoSecretInRows(t, f.httpsRows(t), "disk is full")
	})
}

func TestHTTPSAuditSettingsTokenRemoval(t *testing.T) {
	t.Parallel()
	del := func(f *dnsFix) *httptest.ResponseRecorder {
		return f.sendDNS(t, http.MethodDelete, "/api/https/acme/dns/token", "")
	}

	t.Run("removing the saved token is one row that says removed and holds no value", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01","token":"`+dnsAPIToken+`"}`))
		f.wantSettingsRows(t, 1)

		if rec := del(f); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE token: %d %s, want 204", rec.Code, rec.Body)
		}
		rows := f.wantSettingsRows(t, 2)
		requireAdminRow(t, rows[1], audit.ActionHTTPSSettingsUpdated, "success")
		if d := rows[1].Details; d["section"] != "dns_credential" || d["fields"] != "dns_credential" || d["dns_credential"] != "removed" {
			t.Errorf("details = %+v, want the credential removed", d)
		}

		// Nothing is saved now, so removing again changes nothing and writes no row.
		if rec := del(f); rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE token again: %d %s, want 204", rec.Code, rec.Body)
		}
		f.wantSettingsRows(t, 2)
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIToken)
	})

	t.Run("removing the token while DNS-01 is saved is a failure row", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+dnsAPIToken+`"}`))
		acmeProblemOf(t, del(f), http.StatusConflict, "acme_dns_token_in_use")

		rows := f.wantSettingsRows(t, 2)
		requireAdminRow(t, rows[1], audit.ActionHTTPSSettingsUpdated, "failure")
		if rows[1].Reason != "acme_dns_token_in_use" || len(rows[1].Details) != 0 {
			t.Errorf("row = reason %q details %+v, want acme_dns_token_in_use and no details", rows[1].Reason, rows[1].Details)
		}
	})
}

// dnsTestedRows is the https.dns_tested rows of the log, oldest first.
func (f *acmeFix) dnsTestedRows(t *testing.T) []httpsRow {
	t.Helper()
	return rowsOf(f.httpsRows(t), audit.ActionHTTPSDNSTested)
}

func TestHTTPSAuditTokenTest(t *testing.T) {
	t.Parallel()
	test := func(f *dnsFix, body string) *httptest.ResponseRecorder {
		return f.sendDNS(t, http.MethodPost, "/api/https/acme/dns/test", body)
	}
	// withAgent gives the certificate a second name in the same zone.
	withAgent := func(f *dnsFix) *dnsFix {
		f.inst.ApplyAgentURL("https://agent.example.com")
		return f
	}

	t.Run("a typed token that passes both names", func(t *testing.T) {
		t.Parallel()
		f := withAgent(newDNSFix(t, "", acmeOpts{}))
		if rec := test(f, `{"token":"`+dnsAPIToken+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("test: %d %s", rec.Code, rec.Body)
		}
		rows := f.dnsTestedRows(t)
		if len(rows) != 1 {
			t.Fatalf("%d https.dns_tested rows, want 1: %+v", len(rows), rows)
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSDNSTested, "success")
		d := rows[0].Details
		if d["provider"] != "cloudflare" || d["names"] != "2" || d["passed"] != "2" || d["credential_source"] != "typed" {
			t.Errorf("details = %+v, want cloudflare, 2 names, 2 passed, typed", d)
		}
		if _, ok := d["failed_classes"]; ok {
			t.Errorf("details = %+v, want no failed_classes for a pass", d)
		}
		f.wantSettingsRows(t, 0) // a test stores nothing and is not a settings change
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIToken)
	})

	t.Run("the saved token", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01","token":"`+dnsAPIToken+`"}`))
		if rec := test(f, `{}`); rec.Code != http.StatusOK {
			t.Fatalf("test: %d %s", rec.Code, rec.Body)
		}
		rows := f.dnsTestedRows(t)
		if len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["credential_source"] != "saved" || rows[0].Details["names"] != "1" {
			t.Fatalf("rows = %+v, want one success from the saved token over one name", rows)
		}
	})

	t.Run("the environment's token", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{env: config.ACMEEnv{
			DNSProvider: "cloudflare", CloudflareToken: dnsAPIToken, TokenVar: config.EnvCloudflareToken,
		}})
		if rec := test(f, `{}`); rec.Code != http.StatusOK {
			t.Fatalf("test: %d %s", rec.Code, rec.Body)
		}
		rows := f.dnsTestedRows(t)
		if len(rows) != 1 || rows[0].Result != "success" || rows[0].Details["credential_source"] != "env" {
			t.Fatalf("rows = %+v, want one success from the environment's token", rows)
		}
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIToken)
	})

	t.Run("a name that fails is a failure with its class and none of what was typed", func(t *testing.T) {
		t.Parallel()
		f := withAgent(newDNSFix(t, "", acmeOpts{}))
		rec := test(f, `{"token":"`+dnsAPIWrongToken+`"}`)
		var got api.HTTPSACMETokenTestBody
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Passed {
			t.Fatalf("test: %d %s, want 200 and not passed", rec.Code, rec.Body)
		}
		rows := f.dnsTestedRows(t)
		if len(rows) != 1 {
			t.Fatalf("%d https.dns_tested rows, want 1", len(rows))
		}
		requireAdminRow(t, rows[0], audit.ActionHTTPSDNSTested, "failure")
		d := rows[0].Details
		if rows[0].Reason != "token_test_failed" || d["failed_classes"] != "dns_token_invalid" || d["names"] != "2" || d["passed"] != "0" || d["credential_source"] != "typed" {
			t.Errorf("row = reason %q details %+v, want token_test_failed, dns_token_invalid, 2 names, 0 passed, typed", rows[0].Reason, d)
		}
		requireNoSecretInRows(t, f.httpsRows(t), dnsAPIWrongToken)
	})

	t.Run("a refused test is a failure row with the problem type and no details", func(t *testing.T) {
		t.Parallel()
		f := newDNSFix(t, "", acmeOpts{})
		acmeProblemOf(t, test(f, `{"token":"`+dnsAPIToken+`","resolvers":["bad resolver ;;7709"]}`), http.StatusUnprocessableEntity, "acme_dns_resolver_invalid")
		acmeProblemOf(t, test(f, `{}`), http.StatusUnprocessableEntity, "acme_dns_token_required")

		rows := f.dnsTestedRows(t)
		if len(rows) != 2 {
			t.Fatalf("%d https.dns_tested rows, want 2: %+v", len(rows), rows)
		}
		for i, reason := range []string{"acme_dns_resolver_invalid", "acme_dns_token_required"} {
			requireAdminRow(t, rows[i], audit.ActionHTTPSDNSTested, "failure")
			if rows[i].Reason != reason || len(rows[i].Details) != 0 {
				t.Errorf("row %d = reason %q details %+v, want %s and no details", i, rows[i].Reason, rows[i].Details, reason)
			}
		}
		requireNoSecretInRows(t, f.httpsRows(t), "7709", "bad resolver", dnsAPIToken)
	})
}
