// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/acme/acmetest"
	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
)

// D-13: no audit row of the HTTPS feature holds a token, a key, a PEM block or the CA's own words.

// secretFlowToken is the 40-character token the admin types in the flow. The fake provider accepts it
// (secretFlowFactory maps it to the token the fake knows), so every save and test with it succeeds.
const secretFlowToken = "sx9Qm4TzK2vRb7LpW3nDh8Jc5YfG1eUa6OtVi0Xk" //nolint:gosec // G101: a recognisable fake token, so the scan can look for it

// secretFlowCAText is the CA problem text seeded into the failing run.
const secretFlowCAText = "CA-PROBLEM-TEXT-zq81-unique-detail" //nolint:gosec // G101: a recognisable fake CA message, not a credential

// secretRow is one stored audit row with every text column the log keeps.
type secretRow struct {
	Seq                                                      int64
	ActorID, ActorName, Action, TargetType, TargetID, Result string
	Reason, IP, UserAgent, Details                           string
	Parsed                                                   map[string]string
}

// allAuditRows reads every audit row straight from the table, which is what the log keeps.
func (f *acmeFix) allAuditRows(t *testing.T) []secretRow {
	t.Helper()
	rs, err := f.st.Read.QueryContext(context.Background(), `SELECT seq, actor_id, actor_name, action, target_type, target_id, result,
		reason, ip, user_agent, details FROM audit_log ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var out []secretRow
	for rs.Next() {
		var r secretRow
		if err := rs.Scan(&r.Seq, &r.ActorID, &r.ActorName, &r.Action, &r.TargetType, &r.TargetID, &r.Result, &r.Reason, &r.IP, &r.UserAgent, &r.Details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(r.Details), &r.Parsed); err != nil {
			t.Fatalf("row %d (%s) details are not a string map: %v: %s", r.Seq, r.Action, err, r.Details)
		}
		out = append(out, r)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fields is every place a value can sit in a row, named for the failure message: the plain columns, each
// detail key and value, and the stored details text as a whole.
func (r secretRow) fields() map[string]string {
	m := map[string]string{
		"actor id": r.ActorID, "actor name": r.ActorName, "action": r.Action, "target type": r.TargetType,
		"target id": r.TargetID, "reason": r.Reason, "ip": r.IP, "user agent": r.UserAgent, "details": r.Details,
	}
	for k, v := range r.Parsed {
		m["detail key "+k] = k
		m["detail "+k] = v
	}
	return m
}

// forbidden is a thing no row may contain, with the words the failure message uses for it.
type forbidden struct{ name, needle string }

// derEncodings is a key's DER in the four encodings an encoder reaches for, and in hex.
func derEncodings(name string, der []byte) []forbidden {
	return []forbidden{
		{name + " as standard base64", base64.StdEncoding.EncodeToString(der)},
		{name + " as raw base64", base64.RawStdEncoding.EncodeToString(der)},
		{name + " as URL base64", base64.URLEncoding.EncodeToString(der)},
		{name + " as raw URL base64", base64.RawURLEncoding.EncodeToString(der)},
		{name + " as hex", hex.EncodeToString(der)},
	}
}

// keyForbiddens is the PKCS #8 and SEC 1 DER of key in every encoding, and its private scalar in hex.
func keyForbiddens(t *testing.T, name string, key *ecdsa.PrivateKey) []forbidden {
	t.Helper()
	p8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	out := derEncodings(name+" (PKCS #8)", p8)
	out = append(out, derEncodings(name+" (SEC 1)", ec)...)
	scalar, err := key.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return append(out, forbidden{name + " scalar as hex", hex.EncodeToString(scalar)})
}

// secretFlowForbidden builds everything the scan looks for.
func secretFlowForbidden(t *testing.T, f *acmeFix) []forbidden {
	t.Helper()
	ctx := context.Background()
	_, accountKey, found, err := f.store.Account(ctx, f.dir.URL)
	if err != nil || !found || accountKey == nil {
		t.Fatalf("the account key: found %v, err %v", found, err)
	}
	_, certKey, found, err := f.store.Certificate(ctx)
	if err != nil || !found || certKey == nil {
		t.Fatalf("the certificate key: found %v, err %v", found, err)
	}

	out := []forbidden{{"the typed token", secretFlowToken}}
	for i := 0; i+8 <= len(secretFlowToken); i++ {
		out = append(out, forbidden{"the 8-character piece " + secretFlowToken[i:i+8] + " of the typed token", secretFlowToken[i : i+8]})
	}
	out = append(out,
		forbidden{"a PEM marker", "-----BEGIN"},
		forbidden{"a private key label", "PRIVATE KEY"},
		forbidden{"the CA's problem text", secretFlowCAText},
	)
	out = append(out, keyForbiddens(t, "the account key", accountKey)...)
	return append(out, keyForbiddens(t, "the certificate key", certKey)...)
}

// secretFlowAuditActions is what the flow must have written, so the scan cannot pass on an empty or thin log.
var secretFlowAuditActions = []string{
	audit.ActionHTTPSAccountRegistered,
	audit.ActionHTTPSSettingsUpdated,
	audit.ActionHTTPSDNSTested,
	audit.ActionHTTPSIssueRequested,
	audit.ActionHTTPSRenewRequested,
	audit.ActionHTTPSIssued,
	audit.ActionHTTPSRenewed,
	audit.ActionHTTPSRunFailed,
	audit.ActionHTTPSStopped,
	audit.ActionHTTPSResumed,
}

// TestACMEAuditNeverHoldsASecret drives the whole admin and run flow over the API and scans every audit
// row it left for the token, PEM text, key bytes and the CA's problem text (D-13, SET-04, T-17-21).
func TestACMEAuditNeverHoldsASecret(t *testing.T) {
	t.Parallel()
	f := newDNSFixWith(t, acme.ChallengeDNS01, acmeOpts{}, func(inner dns.Factory) dns.Factory {
		// The provider knows one token; the admin types a different, longer one. The mapping lives in the
		// factory, so the token the manager hands over is the typed one and only the fake sees the other.
		return func(token string) dns.Provider {
			if token == secretFlowToken {
				token = dnsAPIToken
			}
			return inner(token)
		}
	})
	const email = "ops@example.org"

	// Register with an email, save DNS-01 with the typed token and one resolver.
	f.register(t, email)
	if got := decodeValidation(t, f.putValidation(t, `{"challengeType":"dns-01","token":"`+secretFlowToken+`","resolvers":["`+f.srv.Addr()+`"]}`)); !got.Saved {
		t.Fatalf("the validation save: %+v, want saved", got)
	}

	// Test token, typed and then the saved one.
	for _, body := range []string{`{"token":"` + secretFlowToken + `"}`, `{}`} {
		rec := f.send(t, http.MethodPost, "/api/https/acme/dns/test", body)
		var res struct {
			Passed bool `json:"passed"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &res) != nil || !res.Passed {
			t.Fatalf("POST dns/test %s: %d %s, want 200 and passed", body[:min(len(body), 12)], rec.Code, rec.Body)
		}
	}

	// Issue and Renew now both succeed.
	f.awaitRun(t, startRun(t, f.issue(t)))
	f.clock.Advance(time.Minute)
	f.awaitRun(t, startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", "")))

	// A renewal the CA refuses, with a problem text of its own.
	f.fake.FailValidation("tierplan.example.com", acmetest.Problem{Type: "connection", Detail: secretFlowCAText})
	f.clock.Advance(time.Minute)
	f.awaitRun(t, startRun(t, f.send(t, http.MethodPost, "/api/https/acme/renew", "")))
	if run := f.status(t).ACME.LastRun; run == nil || run.Outcome == "succeeded" {
		t.Fatalf("lastRun = %+v, want the failing run", run)
	}

	// Stop, the restore pause and Resume.
	if rec := f.send(t, http.MethodPost, "/api/https/acme/stop", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s, want 204", rec.Code, rec.Body)
	}
	f.pause(t)
	if rec := f.send(t, http.MethodPost, "/api/https/acme/resume", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("resume: %d %s, want 204", rec.Code, rec.Body)
	}

	// The token cannot be removed while DNS-01 is the saved method; with http-01 saved it can.
	acmeProblemOf(t, f.send(t, http.MethodDelete, "/api/https/acme/dns/token", ""), http.StatusConflict, "acme_dns_token_in_use")
	decodeValidation(t, f.putValidation(t, `{"challengeType":"http-01"}`))
	if rec := f.send(t, http.MethodDelete, "/api/https/acme/dns/token", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove the token: %d %s, want 204", rec.Code, rec.Body)
	}

	rows := f.allAuditRows(t)

	// The flow's actions are all in the log, and the refused removal is a failure row.
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.Action]++
	}
	for _, a := range secretFlowAuditActions {
		if seen[a] == 0 {
			t.Errorf("the flow wrote no %s row, so the scan would pass without looking at one: saw %v", a, seen)
		}
	}
	var refused bool
	for _, r := range rows {
		if r.Action == audit.ActionHTTPSSettingsUpdated && r.Result == string(audit.ResultFailure) && r.Reason == "acme_dns_token_in_use" {
			refused = true
		}
	}
	if !refused {
		t.Error("the refused token removal wrote no failure row")
	}
	var found []string
	for a := range seen {
		if strings.HasPrefix(a, "https.") {
			found = append(found, a)
		}
	}
	slices.Sort(found)
	t.Logf("scanning %d audit rows; https actions found: %s", len(rows), strings.Join(found, ", "))

	// Not one field of one row holds a secret.
	bad := secretFlowForbidden(t, f.acmeFix)
	for _, r := range rows {
		for field, value := range r.fields() {
			lower := strings.ToLower(value)
			for _, b := range bad {
				hexHit := strings.HasSuffix(b.name, "as hex") && strings.Contains(lower, strings.ToLower(b.needle))
				if strings.Contains(value, b.needle) || hexHit {
					t.Errorf("audit row %d (%s) field %q holds %s", r.Seq, r.Action, field, b.name)
				}
			}
		}
	}
}
