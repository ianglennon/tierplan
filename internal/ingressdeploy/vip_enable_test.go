// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/vault"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// wantVIPAllDone fails the test unless every item of the virtual IP enable job ended done.
func wantVIPAllDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemVIPKey, ingressdeploy.ItemVIPSecret, ingressdeploy.ItemVIPService, ingressdeploy.ItemVIPSwitch} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// codesOf are the codes of the findings of a severity.
func codesOf(fs []ingressdeploy.Finding, severity string) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == severity {
			out = append(out, f.Code)
		}
	}
	return out
}

// TestEnableVIPOnSwarm is the tracer: on a swarm whose ingress is on and whose three managers allow, the preview refuses
// nothing, shows the router ID, the manager table, the fixed warning and the unverified line, and writes nothing; the
// enable then makes the key, the swarm secret, the keepalived service and the stored row with its audit entry (D-16).
func TestEnableVIPOnSwarm(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	ctx := testsupport.Context(t)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}

	pv, err := v.svc.PreviewVIP(ctx, v.envID, req)
	if err != nil {
		t.Fatalf("PreviewVIP: %v", err)
	}
	if errs := codesOf(pv.Findings, ingressdeploy.SeverityError); len(errs) > 0 {
		t.Fatalf("the preview refuses with %v, want no refusal; findings = %+v", errs, pv.Findings)
	}
	if !contains(codesOf(pv.Findings, ingressdeploy.SeverityWarning), ingressdeploy.CodeVIPUnverified) {
		t.Errorf("the preview has no %s line: %+v", ingressdeploy.CodeVIPUnverified, pv.Findings)
	}
	want := ingressdeploy.DerivedRouterID(testClusterID)
	if pv.RouterID != want || pv.DerivedRouterID != want || !pv.RouterDerived {
		t.Errorf("router ID = %d (derived %d, %v), want the derived %d", pv.RouterID, pv.DerivedRouterID, pv.RouterDerived, want)
	}
	if pv.Address != netip.MustParsePrefix(vipTestAddress) || len(pv.Managers) != 3 || pv.Warning != ingressdeploy.VIPPrivilegeWarning || len(pv.Items) != 4 {
		t.Errorf("preview = address %v, %d managers, %d items, warning %q", pv.Address, len(pv.Managers), len(pv.Items), pv.Warning)
	}
	if w := v.fake.Writes(); len(w) != 0 {
		t.Errorf("the preview wrote %v", w)
	}
	if _, err := v.istore.GetVIP(ctx, v.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("GetVIP after the preview = %v, want ErrVIPNotFound", err)
	}
	if v.auditCount("ingress.vip_enabled") != 0 {
		t.Error("the preview wrote an audit row")
	}

	job := v.enableVIP(req)
	wantVIPAllDone(t, job)
	if job.Kind != ingressdeploy.JobKindVIPEnable || job.EnvID != v.envID || job.Owner != testOwner {
		t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
	}

	// The secret holds the key as the text hex: and 64 digits, under the keepalived labels.
	name := ingressdeploy.KeepalivedSecretName(v.envID, 1)
	var secretID string
	for _, s := range v.secrets() {
		if s.Name == name {
			secretID = s.ID
			if !ingress.OwnsKeepalivedSecret(s.Name, s.Labels) {
				t.Errorf("secret labels = %v, want the keepalived labels", s.Labels)
			}
		}
	}
	if secretID == "" {
		t.Fatalf("the swarm holds no secret %s: %+v", name, v.secrets())
	}
	if creates := v.fake.SecretCreates(); len(creates) != 1 || creates[0].DataBytes != len("hex:")+64 {
		t.Errorf("secret creates = %+v, want one of 68 bytes", creates)
	}

	// The service is what KeepalivedServiceSpec builds, with the secret's ID filled in.
	creates := v.fake.ServiceCreates()
	if len(creates) != 1 {
		t.Fatalf("service creates = %d, want 1", len(creates))
	}
	spec := ingressdeploy.KeepalivedServiceSpec(ingressdeploy.KeepalivedSpecInput{
		Image: ingressdeploy.KeepalivedImage(testServerVersion), Address: netip.MustParsePrefix(vipTestAddress), RouterID: want, SecretName: name,
	})
	spec.TaskTemplate.ContainerSpec.Secrets[0].SecretID = secretID
	if d := firstDiff("spec", asTree(t, spec), asTree(t, creates[0].Spec)); d != "" {
		t.Errorf("the created service differs from KeepalivedServiceSpec at %s", d)
	}

	// The row holds the address, the router ID and the key sealed under its context; the audit row names no key.
	row, err := v.istore.GetVIP(ctx, v.envID)
	if err != nil {
		t.Fatalf("GetVIP: %v", err)
	}
	if row.Address != netip.MustParseAddr("10.9.0.50") || row.PrefixLen != 24 || row.RouterID != want || !row.RouterDerived || row.KeyGeneration != 1 || !row.Enabled {
		t.Errorf("row = %+v", row)
	}
	vl, err := vault.New(bytes.Repeat([]byte{7}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := vl.Open(vault.ContextIngressVIPKey+v.envID, row.KeySealed); err != nil || len(raw) != 32 {
		t.Errorf("the sealed key opens to %d bytes, %v; want 32 under ContextIngressVIPKey", len(raw), err)
	}
	if got := v.auditCount("ingress.vip_enabled"); got != 1 {
		t.Fatalf("ingress.vip_enabled audit rows = %d, want 1", got)
	}
	var details string
	if err := v.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'ingress.vip_enabled'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(details), &d); err != nil || d["address"] != vipTestAddress || d["generation"] != "1" || d["router_id"] == "" {
		t.Errorf("audit details = %q (%v), want address, router_id and generation", details, err)
	}
	if _, held := v.locks.Held(v.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}
