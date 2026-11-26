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
	"github.com/ianglennon/tierplan/internal/ingress"
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

// wantSuccessRow fails the test unless rows holds exactly one success row of action with the details named, and no failure row.
func wantSuccessRow(t *testing.T, rows []audit.Row, action string, details ...string) {
	t.Helper()
	if len(rows) != 1 || rows[0].Result != string(audit.ResultSuccess) {
		t.Fatalf("%s rows = %+v, want one success row", action, rows)
	}
	for _, k := range details {
		if rows[0].Details[k] == "" {
			t.Errorf("the %s success row lacks detail %q: %v", action, k, rows[0].Details)
		}
	}
}

// TestAuditProofVIPChangedFailure is the proof of A-01 class F for a virtual IP change: a change whose service update is
// refused logs one ingress.vip_changed failure row with the step and its code, none of the engine's words and no key. A
// change whose wait ends past its deadline is applied and saved, so it logs no failure row, only the success row.
func TestAuditProofVIPChangedFailure(t *testing.T) {
	const newAddress = "10.9.0.60/24"
	t.Run("the service update is refused", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		keyHex := f.vipKeyHex()
		cfg := f.base
		cfg.WriteError = "the manager cannot update the service with " + switchCanary
		f.fake.Reconfigure(cfg)

		job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
		if it := itemOf(job, ingressdeploy.ItemVIPChangeService); job.State == jobs.JobDone || it.State != jobs.ItemFailed {
			t.Fatalf("the change ended %q with the service item %q, want it failed", job.State, it.State)
		}

		rows := audittest.Rows(t, f.st, audit.ActionIngressVIPChanged)
		wantFailureRow(t, rows, f.envID, audit.ActionIngressVIPChanged, ingressdeploy.ItemVIPChangeService, ingressdeploy.CodeVIPChangeFailed)
		if len(rows) != 1 {
			t.Errorf("%d ingress.vip_changed rows, want only the failure: %+v", len(rows), rows)
		}
		audittest.AssertNoSecrets(t, rows, switchCanary, keyHex)
	})
	t.Run("the wait runs out but the change is saved", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		keyHex := f.vipKeyHex()
		f.rolling(true, true, false)

		job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
		if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemFailed {
			t.Fatalf("the wait item = %+v, want failed", it)
		}

		// The swarm took the address and the job saved it, so the change is one success row and never a failure row besides it.
		rows := audittest.Rows(t, f.st, audit.ActionIngressVIPChanged)
		wantSuccessRow(t, rows, audit.ActionIngressVIPChanged, "address", "router_id")
		if n := f.auditCountResult(audit.ActionIngressVIPChanged, audit.ResultFailure); n != 0 {
			t.Errorf("%d failure rows for a change that was applied: %+v", n, rows)
		}
		audittest.AssertNoSecrets(t, rows, keyHex)
	})
}

// TestAuditProofVIPChanged: a successful change has one success row, with the address and router ID, and no failure row, and
// no row holds the key.
func TestAuditProofVIPChanged(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	keyHex := f.vipKeyHex()
	wantChangeDone(t, f.changeVIP(ingressdeploy.VIPRequest{Address: "10.9.0.60/24"}))

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPChanged)
	wantSuccessRow(t, rows, audit.ActionIngressVIPChanged, "address", "router_id")
	if n := f.auditCountResult(audit.ActionIngressVIPChanged, audit.ResultFailure); n != 0 {
		t.Errorf("a successful change wrote %d failure rows", n)
	}
	audittest.AssertNoSecrets(t, rows, keyHex)
	wantNoKeyShape(t, rows)
}

// TestAuditProofVIPDisabledFailure is the proof of A-01 class F for a virtual IP switch-off: a service removal the swarm
// refuses logs one ingress.vip_disabled failure row with the step and its code, none of the engine's words and no key.
func TestAuditProofVIPDisabledFailure(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	keyHex := f.vipKeyHex()
	f.fake.SetServiceRemoveError(ingress.ServiceNameKeepalived, "the manager cannot remove the service "+switchCanary)

	job := f.disableVIP()
	wantSwitchOffLeft(t, f, job, ingressdeploy.ItemVIPRemoveService)

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPDisabled)
	wantFailureRow(t, rows, f.envID, audit.ActionIngressVIPDisabled, ingressdeploy.ItemVIPRemoveService, ingressdeploy.CodeVIPRemoveFailed)
	if len(rows) != 1 {
		t.Errorf("%d ingress.vip_disabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary, keyHex)
}

// TestAuditProofVIPDisabled: a successful switch-off has one success row, with the address, router ID and generation, and no
// failure row, and no row holds the key.
func TestAuditProofVIPDisabled(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	keyHex := f.vipKeyHex()
	wantVIPDisableDone(t, f.disableVIP())

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPDisabled)
	wantSuccessRow(t, rows, audit.ActionIngressVIPDisabled, "address", "router_id", "generation")
	if n := f.auditCountResult(audit.ActionIngressVIPDisabled, audit.ResultFailure); n != 0 {
		t.Errorf("a successful switch-off wrote %d failure rows", n)
	}
	audittest.AssertNoSecrets(t, rows, keyHex)
	wantNoKeyShape(t, rows)
}

// TestCancelledVIPSwitchIsAudited: a change cancelled while it waits for the nodes ends its wait item cancelled, and the swarm
// had taken the address and the job saved it, so its one audit row is the success row and there is no failure row.
func TestCancelledVIPSwitchIsAudited(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	ctx := testsupport.Context(t)
	f.enabled()
	keyHex := f.vipKeyHex()
	f.rolling(false, false, false)
	var id string
	started := make(chan struct{})
	f.clock.hook = func(n int) {
		if n == 1 {
			<-started
			if err := f.hub.Cancel(id); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
	}
	id, err := f.svc.ChangeVIP(ctx, testActor, testOwner, f.envID, ingressdeploy.VIPRequest{Address: "10.9.0.60/24"})
	if err != nil {
		t.Fatalf("ChangeVIP: %v", err)
	}
	close(started)
	if it := itemOf(f.wait(id), ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemCancelled {
		t.Fatalf("wait item = %+v, want cancelled", it)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressVIPChanged)
	wantSuccessRow(t, rows, audit.ActionIngressVIPChanged, "address", "router_id")
	if n := f.auditCountResult(audit.ActionIngressVIPChanged, audit.ResultFailure); n != 0 {
		t.Errorf("%d failure rows for a change that was applied: %+v", n, rows)
	}
	audittest.AssertNoSecrets(t, rows, keyHex)
}
