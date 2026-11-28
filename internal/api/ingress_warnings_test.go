// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
)

// The ingress_ items of GET /api/warnings (Phase 20, CRT-04, D-10, D-41).

// warningItems reads GET /api/warnings as u and returns its items.
func (f *ingressFix) warningItems(t *testing.T, u credentialUser) []api.WarningBody {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, "/api/warnings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/warnings: %d %s", rec.Code, rec.Body)
	}
	var w api.WarningsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatalf("decode the warnings: %v", err)
	}
	return w.Items
}

// CRT-04 tracer: a referenced production certificate 10 days from its end reaches the instance admin's bell with its
// hostname, its end and the first warning (14), and an environment Admin does not see it.
func TestIngressExpiringCertificateReachesTheBell(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.register(t, f.adminA, f.envA, "apps.example.com")
	c := f.requestCert(t, f.adminA, f.envA, "web.apps.example.com")

	now := f.clk.now()
	end := now.Add(10 * 24 * time.Hour)
	f.setCert(t, c.ID, `chain_pem = 'chain', not_before = ?, not_after = ?, phase = 'idle'`,
		now.Add(-80*24*time.Hour).UnixNano(), end.UnixNano())

	var got *api.WarningBody
	for _, it := range f.warningItems(t, f.admin) {
		if it.ID == "ingress_cert_expiring" {
			got = &it
		}
	}
	if got == nil || got.Ingress == nil {
		t.Fatalf("no ingress_cert_expiring item for the instance admin")
	}
	in := got.Ingress
	if in.Hostname != "web.apps.example.com" || in.Days != 14 || in.NotAfter == nil || !in.NotAfter.Equal(end) || in.More != 0 {
		t.Errorf("the item is %+v, want the hostname, days 14, the end and no others", in)
	}
	if in.EnvID != f.envA {
		t.Errorf("the item names environment %q, want %q (the bell opens its Certificates tab)", in.EnvID, f.envA)
	}

	// An environment Admin lacks the global settings.read the feed needs, so they are refused, and nothing of
	// the certificates reaches them (A8: they see their certificates through the API list).
	rec := f.credCall(t, f.adminA, http.MethodGet, "/api/warnings", "")
	if strings.Contains(rec.Body.String(), "ingress") || strings.Contains(rec.Body.String(), "web.apps.example.com") {
		t.Errorf("an environment Admin was told: %d %s", rec.Code, rec.Body)
	}
}
