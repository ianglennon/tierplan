// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingresscert

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// Phase 20 review WR-02: an upload never deletes or takes over another environment's uploaded certificate, even when
// nothing requested it and nothing claims the hostname. The refusal is ingress_host_taken and names nothing of the other
// environment; the other environment's row, its sealed key and the versions are left as they were.
func TestUploadNeverReplacesAnotherEnvironmentsUpload(t *testing.T) {
	ctx := context.Background()
	c := newClockStore(t)
	seedEnv(t, c.st, "env-a", "a")
	seedEnv(t, c.st, "env-b", "b")
	ca := newTestCA(t)

	b, _, err := c.upload(t, ca, "env-b", "app.example.com", uploadWorld("env-b", "dom-b"))
	if err != nil {
		t.Fatalf("env-b's upload: %v", err)
	}
	keyBefore := c.count(t, `SELECT COUNT(*) FROM ingress_certs WHERE id = ? AND key_sealed <> ''`, b.ID)
	aBefore, _ := versions(t, c.st, "env-a")
	bBefore, _ := versions(t, c.st, "env-b")

	_, _, err = c.upload(t, ca, "env-a", "app.example.com", uploadWorld("env-a", "dom-a"))
	if got := refusalCode(err); got != ingress.CodeHostTaken {
		t.Fatalf("env-a's upload = %v, want a refusal with code %s", err, ingress.CodeHostTaken)
	}
	if msg := err.Error(); strings.Contains(msg, "env-b") || strings.Contains(msg, b.ID) {
		t.Fatalf("the refusal names the other environment or its row: %q", msg)
	}

	kept, err := c.Cert(ctx, b.ID)
	if err != nil || kept.EnvID != "env-b" || kept.Source != SourceUpload || !kept.RetiredAt.IsZero() {
		t.Fatalf("env-b's upload changed: %v, %+v", err, kept)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM ingress_certs WHERE id = ? AND key_sealed <> ''`, b.ID); n != keyBefore || n != 1 {
		t.Fatalf("env-b's sealed key rows = %d, want it kept", n)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM ingress_certs WHERE env_id = 'env-a'`); n != 0 {
		t.Fatalf("the refused upload wrote %d rows", n)
	}
	aAfter, _ := versions(t, c.st, "env-a")
	bAfter, _ := versions(t, c.st, "env-b")
	if aAfter != aBefore || bAfter != bBefore {
		t.Fatalf("a refusal moved versions: a %d -> %d, b %d -> %d", aBefore, aAfter, bBefore, bAfter)
	}
	if n := c.count(t, `SELECT COUNT(*) FROM audit_log WHERE action = ?`, audit.ActionIngressCertMoved); n != 0 {
		t.Fatalf("a refusal wrote %d moved rows", n)
	}
}
