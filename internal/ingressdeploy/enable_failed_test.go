// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// failingEnableRig is a rig whose swarm refuses to create the proxy service, so a first enable leaves the secret and the
// network behind with ingress still off. base is the swarm's configuration without the refusal.
func failingEnableRig(t *testing.T) (r *rig, base dockertest.FakeConfig) {
	t.Helper()
	r = newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
		base = *c
		c.ServiceCreateError = "the manager cannot create the service"
	}})
	job := r.enable(ingressdeploy.EnableRequest{})
	if it := itemOf(job, ingressdeploy.ItemService); job.State == jobs.JobDone || it.State != jobs.ItemFailed {
		t.Fatalf("the enable ended %q with the service item %q, want it not done", job.State, it.State)
	}
	return r, base
}

// TestFailedFirstEnableIsNotAnIncompleteSwitchOff: an enable that fails part-way leaves its objects with ingress off. The
// status reports them as a failed enable (enableFailed, and the class enable_failed on the last pass), not as an incomplete
// switch-off, and the ensure loop, which skips an environment that is off, leaves them (IN-11).
func TestFailedFirstEnableIsNotAnIncompleteSwitchOff(t *testing.T) {
	r, _ := failingEnableRig(t)
	ctx := testsupport.Context(t)

	st, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Enabled || !st.EnableFailed || st.DisablingIncomplete {
		t.Errorf("status = enabled %v, enableFailed %v, disablingIncomplete %v; want off, a failed enable and no incomplete switch-off", st.Enabled, st.EnableFailed, st.DisablingIncomplete)
	}
	if !st.Objects.Secret || !st.Objects.Network || st.Objects.Service {
		t.Errorf("objects = %+v, want the secret and the network left and no service", st.Objects)
	}
	if st.LastEnsure.Class != ingressdeploy.EnsureEnableFailed {
		t.Errorf("last ensure class = %q, want %q", st.LastEnsure.Class, ingressdeploy.EnsureEnableFailed)
	}
	written := len(r.fake.Writes())
	r.round()
	if got := len(r.fake.Writes()); got != written {
		t.Errorf("the ensure loop wrote %v for an environment whose ingress is off", r.fake.Writes()[written:])
	}
}

// TestFailedEnableIsRetriedByEnable: switching on again starts from what the failed enable left, finishes, and the status
// no longer says the enable failed (IN-11).
func TestFailedEnableIsRetriedByEnable(t *testing.T) {
	r, base := failingEnableRig(t)
	ctx := testsupport.Context(t)
	secrets, networks := len(r.fake.SecretCreates()), len(r.fake.NetworkCreates())

	r.fake.Reconfigure(base)
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if len(r.fake.SecretCreates()) != secrets || len(r.fake.NetworkCreates()) != networks {
		t.Errorf("the retry made %d secrets and %d networks more, want it to reuse the ones the failed enable left",
			len(r.fake.SecretCreates())-secrets, len(r.fake.NetworkCreates())-networks)
	}
	st, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Enabled || st.EnableFailed || st.LastEnsure.Class != "" {
		t.Errorf("status after the retry = enabled %v, enableFailed %v, class %q; want on and clean", st.Enabled, st.EnableFailed, st.LastEnsure.Class)
	}
}

// TestFailedEnableIsCleanedBySwitchOff: a switch-off after a failed enable removes what it left, and the status reads clean:
// not a failed enable, not an incomplete switch-off (IN-11).
func TestFailedEnableIsCleanedBySwitchOff(t *testing.T) {
	r, _ := failingEnableRig(t)
	ctx := testsupport.Context(t)

	id, err := r.svc.Disable(ctx, testActor, testOwner, r.envID, seesAll)
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	wantDisableDone(t, r.wait(id))
	if _, ok := r.network(); ok {
		t.Error("the network the failed enable made is still there")
	}
	if n := len(r.secrets()); n != 0 {
		t.Errorf("secrets = %d, want none", n)
	}
	st, err := r.svc.Status(ctx, r.envID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Enabled || st.EnableFailed || st.DisablingIncomplete || st.LastEnsure.Class != "" {
		t.Errorf("status after the switch-off = %+v, want clean", st)
	}
}
