// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"testing"
)

// Phase 20 review CR-01: the server registers the certificate routes without a clock. An upload and a switch back to
// ACME must still answer with the certificate rather than a 500 after the write has committed.
func TestIngressCertUploadAndSwitchBackWithoutAClock(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	f.ingressCertNoClock = true
	f.h = f.build(t) // the routes as the server wires them: IngressCertDeps.Now is nil
	f.register(t, f.adminA, f.envA, "apps.example.com")
	ca := newUploadCA(t, f.clk.now())

	up := f.upload(t, f.adminA, ca, f.envA, "app.apps.example.com")
	if up.ID == "" || up.Hostname != "app.apps.example.com" || up.Source != "upload" {
		t.Errorf("the uploaded certificate = %+v", up)
	}
	if len(up.Warnings) != 1 || up.Warnings[0] != "chain_untrusted" {
		t.Errorf("warnings = %v, want [chain_untrusted]", up.Warnings)
	}

	rec := f.credCall(t, f.adminA, http.MethodPost, certsPath(f.envA, "/", up.ID, "/acme"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("switch back: %d %s, want 200", rec.Code, rec.Body)
	}
	if fresh := decodeCert(t, rec); fresh.ID == "" || fresh.ID == up.ID || fresh.Source != "acme" || fresh.State != "queued" {
		t.Errorf("the new certificate = %+v", fresh)
	}
}
