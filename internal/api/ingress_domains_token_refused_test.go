// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedRefusedChallengeRecord puts a DNS ledger row made with domain domainID's token whose last delete the provider
// refused the token for.
func (f *ingressFix) seedRefusedChallengeRecord(t *testing.T, id, domainID string) {
	t.Helper()
	if _, err := f.st.Write.Exec(
		`INSERT INTO acme_dns_records (id, provider, zone_id, record_id, name, value, owner_kind, owner_id, created_at, credential_ref,
		                               attempts, next_try_at, last_error_class)
		 VALUES (?, 'cloudflare', 'zone-1', ?, '_acme-challenge.web.apps.example.com', 'v', 'order', 'order-1', 1, ?, 5, 9000000000000000000, 'dns_token_invalid')`,
		id, "rec-"+id, domainID); err != nil {
		t.Fatalf("seed the ledger row: %v", err)
	}
}

// Phase 20 review WR-03: when the DNS provider refuses a domain's token for the records it made, the delete's 409 says
// so and tells the Admin to replace the token, without promising that waiting is enough; the replacement is accepted,
// starts a sweep and makes the records due for the new token. A refusal for records still being removed never
// promises success either.
func TestIngressDomainTokenReplaceClearsTheWayForRecordsTheOldTokenCannotRemove(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.seedRefusedChallengeRecord(t, "ledger-refused", d.ID)

	rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), "")
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_domain_cleanup_pending" {
		t.Fatalf("delete: %d %s, want 409 ingress_domain_cleanup_pending", rec.Code, rec.Body)
	}
	detail := problemDetail(t, rec)
	if !strings.Contains(detail, "1 DNS record") || !strings.Contains(detail, "refuses the token") || !strings.Contains(detail, "Replace the token") {
		t.Errorf("delete detail = %q, want the record count, that the provider refuses the token and to replace it", detail)
	}
	for _, promise := range []string{"Try again in a minute", "shortly"} {
		if strings.Contains(detail, promise) {
			t.Errorf("delete detail = %q promises %q", detail, promise)
		}
	}
	before := f.ingressSweeps.Load()

	body, _ := json.Marshal(map[string]string{"token": ingressNextToken})
	rec = f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("token replace with only records the old token cannot remove: %d %s, want 200", rec.Code, rec.Body)
	}
	if got := f.ingressSweeps.Load(); got != before+1 {
		t.Errorf("sweeps asked for = %d after the replacement, want %d", got, before+1)
	}
	var attempts, next int64
	if err := f.st.Read.QueryRow(`SELECT attempts, next_try_at FROM acme_dns_records WHERE id = 'ledger-refused'`).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || next != 0 {
		t.Errorf("the record's backoff after the replacement = %d attempts, next try %d; want it due now", attempts, next)
	}

	// A record the new token may still remove (it was never tried) holds a further replacement back, in words that
	// promise nothing.
	f.seedChallengeRecord(t, d.ID)
	body, _ = json.Marshal(map[string]string{"token": ingressToken})
	rec = f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), string(body))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_domain_cleanup_pending" {
		t.Fatalf("token replace with a record still to remove: %d %s, want 409", rec.Code, rec.Body)
	}
	detail = problemDetail(t, rec)
	if !strings.Contains(detail, "1 DNS record") || strings.Contains(detail, "Try again in a minute") || strings.Contains(detail, "shortly") {
		t.Errorf("replace detail = %q, want the record count and no promise", detail)
	}
}
