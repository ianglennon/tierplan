// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// liveKeepalivedSpec is the keepalived service's spec as the fake swarm holds it.
func (f *vipDisableFix) liveKeepalivedSpec() swarm.ServiceSpec {
	f.t.Helper()
	d, ok := f.keepalivedService()
	if !ok {
		f.t.Fatal("the swarm holds no keepalived service")
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &spec); err != nil {
		f.t.Fatalf("decode the live spec: %v", err)
	}
	return spec
}

// withOlderImage is spec with the image of an earlier server, which a server upgrade has to roll.
func withOlderImage(spec swarm.ServiceSpec) swarm.ServiceSpec {
	spec.TaskTemplate.ContainerSpec.Image = ingressdeploy.KeepalivedImage("1.2.2")
	return spec
}

// vipRow reads the environment's virtual IP row.
func (f *vipDisableFix) vipRow() ingressstore.VIPConfig {
	f.t.Helper()
	row, err := f.istore.GetVIP(testsupport.Context(f.t), f.envID)
	if err != nil {
		f.t.Fatalf("GetVIP: %v", err)
	}
	return row
}

// wantNoWrites fails the test when the swarm saw a write since the count before.
func (f *vipDisableFix) wantNoWrites(before int, what string) {
	f.t.Helper()
	if got := f.fake.Writes(); len(got) != before {
		f.t.Errorf("%s wrote %v, want nothing", what, got[before:])
	}
}

// TestEnsureVIPRepairs is the tracer of D-18: a pass over a virtual IP that is as wanted writes nothing and records itself on the
// virtual IP's row; a missing secret and service are made again from the sealed key; a service on an older image is updated once
// and then left; and a key secret of an older generation beside the current one is removed once the service names the current.
func TestEnsureVIPRepairs(t *testing.T) {
	f := newVIPIngressFix(t)
	ctx := testsupport.Context(t)
	name1 := ingressdeploy.KeepalivedSecretName(f.envID, 1)

	t.Run("as wanted", func(t *testing.T) {
		before := len(f.fake.Writes())
		f.round()
		f.wantNoWrites(before, "a pass over a virtual IP that is as wanted")
		row := f.vipRow()
		if row.EnsureAt.IsZero() || row.EnsureError != "" {
			t.Errorf("the row records %v %q, want the pass and no error", row.EnsureAt, row.EnsureError)
		}
	})

	t.Run("a missing secret and service are made again", func(t *testing.T) {
		svc, _ := f.keepalivedService()
		if err := f.cli.ServiceRemove(ctx, svc.ID); err != nil {
			t.Fatalf("ServiceRemove: %v", err)
		}
		for _, s := range f.secrets() {
			if s.Name == name1 {
				if err := f.cli.SecretRemove(ctx, s.ID); err != nil {
					t.Fatalf("SecretRemove: %v", err)
				}
			}
		}
		creates := len(f.fake.SecretCreates())
		f.round()
		got := f.fake.SecretCreates()
		// The content is the 32-byte key as the secret form holds it: hex: and 64 digits.
		if len(got) != creates+1 || got[creates].Name != name1 || got[creates].DataBytes != len("hex:")+64 {
			t.Fatalf("secret creates = %+v, want the one of %s with the key", got[creates:], name1)
		}
		if want := f.keepalivedSecrets(); !slices.Equal(want, []string{name1}) {
			t.Errorf("key secrets = %v, want %s", want, name1)
		}
		spec := f.liveKeepalivedSpec()
		refs := spec.TaskTemplate.ContainerSpec.Secrets
		var secretID string
		for _, s := range f.secrets() {
			if s.Name == name1 {
				secretID = s.ID
			}
		}
		if len(refs) != 1 || refs[0].SecretName != name1 || refs[0].SecretID != secretID {
			t.Errorf("the service's secret = %+v, want %s by the new ID %s", refs, name1, secretID)
		}
		if row := f.vipRow(); row.EnsureError != "" {
			t.Errorf("ensure error = %q, want none", row.EnsureError)
		}
	})

	t.Run("an older image is updated once", func(t *testing.T) {
		f.fake.ReplaceServiceSpec(ingress.ServiceNameKeepalived, withOlderImage(f.liveKeepalivedSpec()))
		updates := len(f.fake.ServiceUpdates())
		f.round()
		if got := len(f.fake.ServiceUpdates()); got != updates+1 {
			t.Fatalf("service updates = %d, want the one update", got-updates)
		}
		if got, want := f.liveKeepalivedSpec().TaskTemplate.ContainerSpec.Image, ingressdeploy.KeepalivedImage(testServerVersion); got != want {
			t.Errorf("image = %q, want %q", got, want)
		}
		before := len(f.fake.Writes())
		f.round()
		f.wantNoWrites(before, "the pass after the update")
	})

	t.Run("a stale generation goes once the service names the current one", func(t *testing.T) {
		cfg := f.vipRow()
		cfg.KeyGeneration = 2
		if err := f.istore.PutVIP(ctx, cfg); err != nil {
			t.Fatalf("PutVIP: %v", err)
		}
		name2 := ingressdeploy.KeepalivedSecretName(f.envID, 2)
		f.round()
		if want := f.keepalivedSecrets(); !slices.Equal(want, []string{name1, name2}) {
			t.Fatalf("key secrets after the service moved = %v, want both generations while the pass wrote", want)
		}
		if refs := f.liveKeepalivedSpec().TaskTemplate.ContainerSpec.Secrets; len(refs) != 1 || refs[0].SecretName != name2 {
			t.Fatalf("the service's secret = %+v, want %s", refs, name2)
		}
		f.round()
		if want := f.keepalivedSecrets(); !slices.Equal(want, []string{name2}) {
			t.Errorf("key secrets after the next pass = %v, want only %s", want, name2)
		}
	})

	// The virtual IP's pass is its row's alone: the ingress row records the ingress leg only.
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || st.EnsureError != "" {
		t.Errorf("the ingress row = %q, %v, want no error from the virtual IP's leg", st.EnsureError, err)
	}
}

// TestEnsureVIPKeepsRunningWhenAManagerRefuses is D-19: one manager that no longer allows privileged services ends the pass
// with privileged_lost on the virtual IP's row, writes nothing and removes nothing, even when the service is drifted or gone;
// the ingress row does not say it. When the opt-in is back the next pass repairs.
func TestEnsureVIPKeepsRunningWhenAManagerRefuses(t *testing.T) {
	f := newVIPIngressFix(t)
	ctx := testsupport.Context(t)
	f.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerRefuses, envs.ManagerAllows), nil)

	f.fake.ReplaceServiceSpec(ingress.ServiceNameKeepalived, withOlderImage(f.liveKeepalivedSpec()))
	before := len(f.fake.Writes())
	f.round()
	f.wantNoWrites(before, "a pass with a manager refusing")
	if _, ok := f.keepalivedService(); !ok {
		t.Fatal("the running keepalived service was removed")
	}
	if row := f.vipRow(); row.EnsureError != "privileged_lost" || row.EnsureAt.IsZero() {
		t.Errorf("the row records %q at %v, want privileged_lost", row.EnsureError, row.EnsureAt)
	}
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || st.EnsureError != "" {
		t.Errorf("the ingress row = %q, %v, want it untouched by the virtual IP's gate", st.EnsureError, err)
	}

	// A service that is gone is not made again either.
	svc, _ := f.keepalivedService()
	if err := f.cli.ServiceRemove(ctx, svc.ID); err != nil {
		t.Fatalf("ServiceRemove: %v", err)
	}
	before = len(f.fake.Writes())
	f.round()
	f.wantNoWrites(before, "a pass with a manager refusing and no service")
	if _, ok := f.keepalivedService(); ok {
		t.Error("the service was made while a manager refused")
	}

	// A table that cannot be read says nothing of any manager.
	f.managers.set(nil, errors.New("the managers are not readable"))
	f.round()
	if row := f.vipRow(); row.EnsureError != ingressdeploy.EnsureUnreachable {
		t.Errorf("the row records %q for an unread table, want unreachable", row.EnsureError)
	}

	f.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerAllows, envs.ManagerAllows), nil)
	f.round()
	if _, ok := f.keepalivedService(); !ok {
		t.Error("the service was not made again once every manager allowed")
	}
	if row := f.vipRow(); row.EnsureError != "" {
		t.Errorf("the row records %q once every manager allows, want none", row.EnsureError)
	}
}

// TestEnsureVIPFinishesARemoval is D-16: a row whose switch is off, the leftover of a switch-off that failed, is finished by the
// next pass through removeVIP, gate or no gate; a paused environment is only observed.
func TestEnsureVIPFinishesARemoval(t *testing.T) {
	f := newVIPIngressFix(t)
	ctx := testsupport.Context(t)
	if err := f.istore.SetVIPEnabled(ctx, f.envID, false); err != nil {
		t.Fatalf("SetVIPEnabled: %v", err)
	}
	// Removing a service is no escalation: a refusing manager does not stop it.
	f.managers.set(vipManagerRows(envs.ManagerRefuses, envs.ManagerRefuses, envs.ManagerRefuses), nil)

	t.Run("paused is observed only", func(t *testing.T) {
		if _, err := f.istore.SetPaused(ctx, f.envID, true, "admin", nil); err != nil {
			t.Fatalf("SetPaused: %v", err)
		}
		before := len(f.fake.Writes())
		f.round()
		f.wantNoWrites(before, "a pass over a paused environment")
		if _, ok := f.keepalivedService(); !ok {
			t.Error("the keepalived service was removed while paused")
		}
		if _, err := f.istore.GetVIP(ctx, f.envID); err != nil {
			t.Errorf("GetVIP while paused = %v, want the row kept", err)
		}
		if _, err := f.istore.SetPaused(ctx, f.envID, false, "admin", nil); err != nil {
			t.Fatalf("SetPaused: %v", err)
		}
	})

	f.round()
	if _, ok := f.keepalivedService(); ok {
		t.Error("the keepalived service is still in the swarm")
	}
	if got := f.keepalivedSecrets(); len(got) != 0 {
		t.Errorf("key secrets left = %v, want none", got)
	}
	if _, err := f.istore.GetVIP(ctx, f.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("GetVIP after the pass = %v, want ErrVIPNotFound", err)
	}
	if n := f.auditCount("ingress.vip_disabled"); n != 1 {
		t.Errorf("ingress.vip_disabled audit rows = %d, want 1", n)
	}
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || !st.Enabled {
		t.Errorf("ingress state = %+v, %v, want the ingress itself left on", st, err)
	}
}

// TestEnsureVIPHeldUpdate is WR-07 for the keepalived service: an update the swarm rolled back is not written again every
// pass while the wanted spec is the same one; the pass records update_failed, and after UpdateRetryAfter it is tried once more.
func TestEnsureVIPHeldUpdate(t *testing.T) {
	f := newVIPIngressFix(t)
	older := withOlderImage(f.liveKeepalivedSpec())
	f.fake.ReplaceServiceSpec(ingress.ServiceNameKeepalived, older)

	f.round()
	if got := len(f.fake.ServiceUpdates()); got != 1 {
		t.Fatalf("service updates = %d, want the one update", got)
	}
	// The swarm rolled it back: the old image is live again and the update is over.
	f.fake.ReplaceServiceSpec(ingress.ServiceNameKeepalived, older)
	f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "rollback_completed", "update rolled back")
	for range 3 {
		f.round()
	}
	if got := len(f.fake.ServiceUpdates()); got != 1 {
		t.Fatalf("service updates after three more passes = %d, want the rolled-back update not retried", got)
	}
	if row := f.vipRow(); row.EnsureError != ingressdeploy.EnsureUpdateFailed {
		t.Errorf("the row records %q, want update_failed", row.EnsureError)
	}

	<-f.clock.After(ingressdeploy.UpdateRetryAfter + time.Second)
	f.round()
	if got := len(f.fake.ServiceUpdates()); got != 2 {
		t.Errorf("service updates after the retry time = %d, want the retry", got)
	}
}
