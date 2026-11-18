// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
)

// Phase 20 review WR-01: D-28 keeps a deleted domain's certificates. Switching a live upload of such a domain back to ACME
// is a 409 ingress_domain_not_found, not a 500, and a retired upload of it is still the documented 404.
func TestIngressCertSwitchBackAfterTheDomainWasDeleted(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	ca := newUploadCA(t, f.clk.now())
	live := f.upload(t, f.adminA, ca, f.envA, "app.apps.example.com")
	retired := f.upload(t, f.adminA, ca, f.envA, "web.apps.example.com")
	switchBack := func(id string) int {
		t.Helper()
		rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA, "/", id, "/acme"), "")
		if rec.Code >= 400 {
			if rec.Code == http.StatusConflict && problemType(t, rec) != "ingress_domain_not_found" {
				t.Errorf("switch back %s: %d %s, want the code ingress_domain_not_found", id, rec.Code, rec.Body)
			}
			if rec.Code == http.StatusNotFound && problemType(t, rec) != "ingress_cert_not_found" {
				t.Errorf("switch back %s: %d %s, want the code ingress_cert_not_found", id, rec.Code, rec.Body)
			}
		}
		return rec.Code
	}
	if code := switchBack(retired.ID); code != http.StatusOK {
		t.Fatalf("switch back while the domain exists: %d, want 200", code)
	}

	if rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), ""); rec.Code >= 300 {
		t.Fatalf("delete the domain: %d %s", rec.Code, rec.Body)
	}

	if code := switchBack(live.ID); code != http.StatusConflict {
		t.Errorf("switch back a live upload of a deleted domain: %d, want 409", code)
	}
	if code := switchBack(retired.ID); code != http.StatusNotFound {
		t.Errorf("switch back a retired upload of a deleted domain: %d, want 404", code)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressCertSwitchedToACME); len(rows) != 1 {
		t.Errorf("ingress.cert_switched_to_acme rows = %+v, want only the switch made while the domain existed", rows)
	}
	if got := f.logs.String(); strings.Contains(got, "read the domain of the certificate") {
		t.Errorf("the server logged a failure for a refusal: %s", got)
	}
}
