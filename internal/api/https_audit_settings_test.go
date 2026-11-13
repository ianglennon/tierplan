// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/acme"
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
