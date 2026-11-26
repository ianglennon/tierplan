// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipKeyHex is the stored VRRP key of the environment in hex, as the key secret carries it, for AssertNoSecrets.
func (v *vipEnableRig) vipKeyHex() string {
	v.t.Helper()
	ctx := testsupport.Context(v.t)
	row, err := v.istore.GetVIP(ctx, v.envID)
	if err != nil {
		v.t.Fatalf("read the virtual IP row: %v", err)
	}
	vl, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		v.t.Fatal(err)
	}
	raw, err := vl.Open(vault.ContextIngressVIPKey+v.envID, row.KeySealed)
	if err != nil || len(raw) != 32 {
		v.t.Fatalf("the sealed key = %d bytes, %v", len(raw), err)
	}
	return hex.EncodeToString(raw)
}

// wantNoKeyShape fails the test when a row holds 64 hex digits anywhere in its reason or details: the shape of a VRRP key, for
// the runs that failed before the key was stored and whose key no test can read back.
func wantNoKeyShape(t *testing.T, rows []audit.Row) {
	t.Helper()
	for _, r := range rows {
		if keyShape.MatchString(r.Reason) {
			t.Errorf("row %s reason holds a 64-digit hex string", r.ID)
		}
		for k, v := range r.Details {
			if keyShape.MatchString(v) {
				t.Errorf("row %s detail %q holds a 64-digit hex string", r.ID, k)
			}
		}
	}
}

// TestAuditProofVIPEnabledFailure is the proof of A-01 class F for a virtual IP switch-on: the keepalived service step is
// refused with an answer that holds a join token, and the log gets one ingress.vip_enabled failure row with the step and its
// code, none of the engine's words and no key.
func TestAuditProofVIPEnabledFailure(t *testing.T) {
	v := newVIPEnableRig(t, func(c *dockertest.FakeConfig) {
		c.ServiceCreateError = "the manager cannot create the service with " + switchCanary
	}, nil)
	job := v.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress})
	if it := itemOf(job, ingressdeploy.ItemVIPService); job.State == jobs.JobDone || it.State != jobs.ItemFailed {
		t.Fatalf("the enable ended %q with the service item %q, want it failed", job.State, it.State)
	}

	rows := audittest.Rows(t, v.st, audit.ActionIngressVIPEnabled)
	wantFailureRow(t, rows, v.envID, audit.ActionIngressVIPEnabled, ingressdeploy.ItemVIPService, ingressdeploy.CodeVIPServiceFailed)
	if len(rows) != 1 {
		t.Errorf("%d ingress.vip_enabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
	wantNoKeyShape(t, rows)
}

// TestAuditProofVIPEnabled: a successful switch-on has one success row, with the address, router ID and generation, and no
// failure row, and no row holds the key.
func TestAuditProofVIPEnabled(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	wantVIPAllDone(t, v.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress}))

	rows := audittest.Rows(t, v.st, audit.ActionIngressVIPEnabled)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) {
		t.Fatalf("ingress.vip_enabled rows = %+v, want one success row", rows)
	}
	for _, k := range []string{"address", "router_id", "generation"} {
		if rows[0].Details[k] == "" {
			t.Errorf("the success row lacks detail %q: %v", k, rows[0].Details)
		}
	}
	if n := v.auditCountResult(audit.ActionIngressVIPEnabled, audit.ResultFailure); n != 0 {
		t.Errorf("a successful enable wrote %d failure rows", n)
	}
	audittest.AssertNoSecrets(t, rows, v.vipKeyHex())
	wantNoKeyShape(t, rows)
}
