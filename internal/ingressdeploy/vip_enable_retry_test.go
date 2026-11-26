// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// failVIPSwitch makes the row insert of the switch step fail, as a full disk or a lost database would, and returns the
// function that lets it succeed again.
func (v *vipEnableRig) failVIPSwitch() (restore func()) {
	v.t.Helper()
	if _, err := v.st.Write.Exec(`CREATE TRIGGER vip_switch_fails BEFORE INSERT ON ingress_vip BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		v.t.Fatalf("create the trigger: %v", err)
	}
	return func() {
		if _, err := v.st.Write.Exec(`DROP TRIGGER vip_switch_fails`); err != nil {
			v.t.Fatalf("drop the trigger: %v", err)
		}
	}
}

// keepalivedState counts the keepalived services the fake swarm holds, and the secrets under the environment's key name.
func (v *vipEnableRig) keepalivedState() (services, secrets int) {
	v.t.Helper()
	if _, _, err := v.cli.ServiceInspect(testsupport.Context(v.t), ingress.ServiceNameKeepalived); err == nil {
		services = 1
	}
	name := ingressdeploy.KeepalivedSecretName(v.envID, 1)
	for _, s := range v.secrets() {
		if s.Name == name {
			secrets++
		}
	}
	return services, secrets
}

// TestEnableVIPStartsOverAfterAFailedSwitch pins WR-02: an enable that fails after the service was created (the row could
// not be stored) leaves a running service that references the key secret. Switching on again must remove that leftover
// service, make a new key, secret and service, and finish, instead of failing on the secret that is in use for ever.
func TestEnableVIPStartsOverAfterAFailedSwitch(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}

	restore := v.failVIPSwitch()
	job := v.enableVIP(req)
	if it := itemOf(job, ingressdeploy.ItemVIPSwitch); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPSwitchFailed {
		t.Fatalf("switch item = %q (%q), want failed with vip_switch_failed; items = %+v", it.State, it.Code, job.Items)
	}
	if svcs, secs := v.keepalivedState(); svcs != 1 || secs != 1 {
		t.Fatalf("after the failed switch the swarm holds %d services and %d secrets, want the leftover 1 and 1", svcs, secs)
	}
	if _, err := v.istore.GetVIP(testsupport.Context(t), v.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Fatalf("a failed switch stored a row: %v", err)
	}

	restore()
	wantVIPAllDone(t, v.enableVIP(req))
	if svcs, secs := v.keepalivedState(); svcs != 1 || secs != 1 {
		t.Errorf("after the second enable the swarm holds %d services and %d secrets, want 1 and 1", svcs, secs)
	}
	if n := len(v.fake.ServiceCreates()); n != 2 {
		t.Errorf("%d services were created, want 2: the leftover removed and a new one made, not the old one reused with a key nobody has", n)
	}
	if _, err := v.istore.GetVIP(testsupport.Context(t), v.envID); err != nil {
		t.Errorf("GetVIP after the second enable: %v", err)
	}
	if n := v.auditCountResult("ingress.vip_enabled", audit.ResultSuccess); n != 1 {
		t.Errorf("%d enable audit rows, want 1", n)
	}
}

// TestEnableVIPLeavesAForeignServiceAlone pins the scope of that removal: when the secret is held by a service that is not
// TierPlan's keepalived service, the step fails and the service is not touched.
func TestEnableVIPLeavesAForeignServiceAlone(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	ctx := testsupport.Context(t)
	name := ingressdeploy.KeepalivedSecretName(v.envID, 1)
	id, err := v.cli.SecretCreate(ctx, dockerx.SecretSpec{Name: name, Labels: ingress.KeepalivedLabels(), Data: []byte("hex:x")})
	if err != nil {
		t.Fatalf("SecretCreate: %v", err)
	}
	raw, err := json.Marshal(swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "someone-elses"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   "example.com/other:1",
			Secrets: []*swarm.SecretReference{{SecretID: id, SecretName: name, File: &swarm.SecretReferenceFileTarget{Name: "k", UID: "0", GID: "0", Mode: 0o400}}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, _, err := v.cli.ServiceCreate(ctx, raw, ""); err != nil {
		t.Fatalf("ServiceCreate: %v", err)
	}

	job := v.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress})
	if it := itemOf(job, ingressdeploy.ItemVIPSecret); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPSecretFailed {
		t.Fatalf("secret item = %q (%q), want failed with vip_secret_failed", it.State, it.Code)
	}
	if _, _, err := v.cli.ServiceInspect(ctx, "someone-elses"); err != nil {
		t.Errorf("the service that is not TierPlan's was removed: %v", err)
	}
}
