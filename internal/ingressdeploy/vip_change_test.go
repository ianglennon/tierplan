// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// changeVIP starts a change and waits for its job to end. The job is returned whatever its outcome.
func (f *vipDisableFix) changeVIP(req ingressdeploy.VIPRequest) jobs.Job {
	f.t.Helper()
	id, err := f.svc.ChangeVIP(testsupport.Context(f.t), testActor, testOwner, f.envID, req)
	if err != nil {
		f.t.Fatalf("ChangeVIP: %v", err)
	}
	return f.wait(id)
}

// wantChangeDone fails the test unless both items of the change ended done.
func wantChangeDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemVIPChangeService, ingressdeploy.ItemVIPChangeSave} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// envOf reads the value of a variable of the keepalived container's environment, "" when there is none.
func envOf(spec swarm.ServiceSpec, key string) string {
	for _, kv := range spec.TaskTemplate.ContainerSpec.Env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// wantChangeRefused fails the test unless a change and its preview are refused with code, and the swarm and the row are as
// they were.
func wantChangeRefused(t *testing.T, f *vipDisableFix, req ingressdeploy.VIPRequest, code string) {
	t.Helper()
	ctx := testsupport.Context(t)
	rowBefore, _ := f.istore.GetVIP(ctx, f.envID)
	writes := len(f.fake.Writes())

	pv, err := f.svc.PreviewVIPChange(ctx, f.envID, req)
	if err != nil {
		t.Fatalf("PreviewVIPChange: %v", err)
	}
	if !slices.Contains(codesOf(pv.Findings, ingressdeploy.SeverityError), code) {
		t.Errorf("the preview refuses with %v, want %s; findings = %+v", codesOf(pv.Findings, ingressdeploy.SeverityError), code, pv.Findings)
	}
	_, err = f.svc.ChangeVIP(ctx, testActor, testOwner, f.envID, req)
	var pe *ingressdeploy.PreflightError
	if !errors.As(err, &pe) || !slices.Contains(codesOf(pe.Findings, ingressdeploy.SeverityError), code) {
		t.Fatalf("ChangeVIP = %v, want a PreflightError with %s", err, code)
	}
	if rowAfter, _ := f.istore.GetVIP(ctx, f.envID); rowAfter != rowBefore {
		t.Errorf("a refused change touched the row: %+v, was %+v", rowAfter, rowBefore)
	}
	if got := len(f.fake.Writes()); got != writes {
		t.Errorf("a refused change wrote %v", f.fake.Writes()[writes:])
	}
	if got := f.auditCount("ingress.vip_changed"); got != 0 {
		t.Errorf("a refused change wrote %d audit rows", got)
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("a refused change kept the operation lock")
	}
}

// TestChangeVIP: a new address in the nodes' subnet updates the service's environment and the row, keeps the key's
// generation, sealed key and secret, and records ingress.vip_changed with the address and the router ID; the same checks as the
// enable refuse a colliding router ID and a manager that does not allow; no virtual IP is ErrVIPNotEnabled; and a request that
// changes nothing is a job that writes nothing (D-03, D-16).
func TestChangeVIP(t *testing.T) {
	const newAddress = "10.9.0.60/24"

	t.Run("a new address", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		ctx := testsupport.Context(t)
		f.enabled()
		before, err := f.istore.GetVIP(ctx, f.envID)
		if err != nil {
			t.Fatal(err)
		}
		secrets := f.keepalivedSecrets()
		writes := len(f.fake.Writes())

		pv, err := f.svc.PreviewVIPChange(ctx, f.envID, ingressdeploy.VIPRequest{Address: newAddress})
		if err != nil {
			t.Fatalf("PreviewVIPChange: %v", err)
		}
		if errs := codesOf(pv.Findings, ingressdeploy.SeverityError); len(errs) > 0 {
			t.Fatalf("the preview refuses with %v", errs)
		}
		if pv.Strategy != ingressdeploy.VIPChangeRoll || pv.Address.String() != newAddress || pv.Current.String() != vipTestAddress ||
			pv.Warning != ingressdeploy.VIPPrivilegeWarning || len(pv.Managers) != 3 || len(pv.Items) != 2 {
			t.Errorf("preview = strategy %q address %v current %v, %d managers, %d items", pv.Strategy, pv.Address, pv.Current, len(pv.Managers), len(pv.Items))
		}
		if !strings.Contains(pv.Notice, "moves") || !strings.Contains(pv.Notice, "briefly unanswered") {
			t.Errorf("the notice %q does not say the address moves and may be briefly unanswered", pv.Notice)
		}
		if w := f.fake.Writes(); len(w) != writes {
			t.Errorf("the preview wrote: %v", w[writes:])
		}

		job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
		wantChangeDone(t, job)
		if job.Kind != ingressdeploy.JobKindVIPChange || job.EnvID != f.envID || job.Owner != testOwner {
			t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
		}

		ups := f.fake.ServiceUpdates()
		if len(ups) != 1 {
			t.Fatalf("service updates = %d, want 1", len(ups))
		}
		if got := envOf(ups[0].Spec, ingressbundle.EnvVIPAddress); got != newAddress {
			t.Errorf("the updated service's address = %q, want %s", got, newAddress)
		}
		if got := envOf(ups[0].Spec, ingressbundle.EnvVIPRouterID); got != strconv.Itoa(before.RouterID) {
			t.Errorf("the updated service's router ID = %q, want the unchanged %d", got, before.RouterID)
		}
		if secs := ups[0].Spec.TaskTemplate.ContainerSpec.Secrets; len(secs) != 1 || secs[0].SecretID == "" || secs[0].SecretName != ingressdeploy.KeepalivedSecretName(f.envID, 1) {
			t.Errorf("the updated service's secrets = %+v, want the generation 1 secret with its ID", secs)
		}

		after, err := f.istore.GetVIP(ctx, f.envID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Address != mustAddr("10.9.0.60") || after.PrefixLen != 24 || after.RouterID != before.RouterID || !after.RouterDerived || !after.Enabled {
			t.Errorf("row = %+v", after)
		}
		if after.KeyGeneration != before.KeyGeneration || after.KeySealed != before.KeySealed {
			t.Errorf("the change rotated the key: generation %d -> %d, sealed key changed %v", before.KeyGeneration, after.KeyGeneration, after.KeySealed != before.KeySealed)
		}
		if got := f.keepalivedSecrets(); !slices.Equal(got, secrets) || len(f.fake.SecretCreates()) != 1 {
			t.Errorf("secrets = %v after %d creates, want %v after the enable's one", got, len(f.fake.SecretCreates()), secrets)
		}
		d := f.auditDetails("ingress.vip_changed")
		if d["address"] != newAddress || d["router_id"] != strconv.Itoa(before.RouterID) {
			t.Errorf("audit details = %v, want address %s and router_id %d", d, newAddress, before.RouterID)
		}
		if _, held := f.locks.Held(f.envID); held {
			t.Error("the operation lock is still held after the job")
		}
	})

	t.Run("a router ID another environment uses", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		seedOtherVIP(t, f.vipEnableRig, "10.9.0.70", 24, 77)
		wantChangeRefused(t, f, ingressdeploy.VIPRequest{Address: newAddress, RouterID: intp(77)}, ingressdeploy.CodeVIPRouterIDInUse)
	})

	t.Run("a manager that does not allow", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		f.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerRefuses, envs.ManagerAllows), nil)
		wantChangeRefused(t, f, ingressdeploy.VIPRequest{Address: newAddress}, ingressdeploy.CodeVIPPrivilegedNotAllowed)
	})

	t.Run("a manager with no agent", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		f.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerUnknown, envs.ManagerAllows), nil)
		wantChangeRefused(t, f, ingressdeploy.VIPRequest{Address: newAddress}, ingressdeploy.CodeVIPPrivilegedNotAllowed)
	})

	t.Run("an address outside the nodes' subnet", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		wantChangeRefused(t, f, ingressdeploy.VIPRequest{Address: "192.168.77.5/24"}, ingressdeploy.CodeVIPOutsideNodeSubnet)
	})

	t.Run("no virtual IP", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		ctx := testsupport.Context(t)
		req := ingressdeploy.VIPRequest{Address: newAddress}
		if _, err := f.svc.PreviewVIPChange(ctx, f.envID, req); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			t.Errorf("PreviewVIPChange = %v, want ErrVIPNotEnabled", err)
		}
		if _, err := f.svc.ChangeVIP(ctx, testActor, testOwner, f.envID, req); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			t.Errorf("ChangeVIP = %v, want ErrVIPNotEnabled", err)
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("wrote %v", w)
		}
		if _, held := f.locks.Held(f.envID); held {
			t.Error("the operation lock is still held")
		}
	})

	t.Run("nothing changes", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		ctx := testsupport.Context(t)
		f.enabled()
		before, _ := f.istore.GetVIP(ctx, f.envID)
		writes := len(f.fake.Writes())

		pv, err := f.svc.PreviewVIPChange(ctx, f.envID, ingressdeploy.VIPRequest{Address: vipTestAddress})
		if err != nil || pv.Strategy != ingressdeploy.VIPChangeNone {
			t.Fatalf("preview of an unchanged request = strategy %q, %v; want %q", pv.Strategy, err, ingressdeploy.VIPChangeNone)
		}
		job := f.changeVIP(ingressdeploy.VIPRequest{Address: vipTestAddress})
		wantChangeDone(t, job)
		if got := f.fake.Writes(); len(got) != writes {
			t.Errorf("an unchanged request wrote %v", got[writes:])
		}
		if after, _ := f.istore.GetVIP(ctx, f.envID); after != before {
			t.Errorf("an unchanged request touched the row: %+v, was %+v", after, before)
		}
		if got := f.auditCount("ingress.vip_changed"); got != 0 {
			t.Errorf("an unchanged request wrote %d audit rows", got)
		}
	})
}

// TestChangeVIPStrategy pins the strategy of each kind of change to what S3 measured (A-08, A-16). The two literals below are
// the lines of 26-S3-RESULT.md "Decisions for the code":
//
//	S3 change-address decision: roll
//	S3 change-router-id decision: replace
//
// An address change is one ServiceUpdate with the update delay S3 chose; a router-ID change, alone or with an address, removes
// the service, waits for its tasks to end and creates it again, so two groups of nodes never hold the address under two router
// IDs at once.
func TestChangeVIPStrategy(t *testing.T) {
	const (
		changeAddress  = "roll"    // S3 change-address decision: roll
		changeRouterID = "replace" // S3 change-router-id decision: replace
	)
	const newAddress = "10.9.0.60/24"
	cases := []struct {
		name string
		req  ingressdeploy.VIPRequest
		want string
	}{
		{name: "the address", req: ingressdeploy.VIPRequest{Address: newAddress}, want: changeAddress},
		{name: "the router ID", req: ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(77)}, want: changeRouterID},
		{name: "both", req: ingressdeploy.VIPRequest{Address: newAddress, RouterID: intp(77)}, want: "replace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newVIPDisableFix(t, vipDisableOpts{tasks: true})
			ctx := testsupport.Context(t)
			f.enabled()
			before := len(f.fake.Writes())

			pv, err := f.svc.PreviewVIPChange(ctx, f.envID, tc.req)
			if err != nil {
				t.Fatalf("PreviewVIPChange: %v", err)
			}
			if pv.Strategy != tc.want {
				t.Fatalf("preview strategy = %q, want %q", pv.Strategy, tc.want)
			}
			if tc.want == "replace" && !strings.Contains(pv.Notice, "unanswered for a few seconds") {
				t.Errorf("the notice %q does not say the address is unanswered for a few seconds", pv.Notice)
			}

			// The old task outlives the removal for one poll, as a node takes a moment to stop its container.
			var atWait []string
			f.clock.hook = func(n int) {
				if n == 1 {
					atWait = f.fake.Writes()[before:]
					f.fake.Reconfigure(f.base)
				}
			}
			job := f.changeVIP(tc.req)
			wantChangeDone(t, job)
			writes := f.fake.Writes()[before:]

			switch tc.want {
			case "roll":
				if len(writes) != 1 || !strings.HasPrefix(writes[0], "POST /services/") || !strings.Contains(writes[0], "/update") {
					t.Fatalf("writes = %v, want one service update", writes)
				}
				ups := f.fake.ServiceUpdates()
				if len(ups) != 1 || len(f.fake.ServiceCreates()) != 1 {
					t.Fatalf("updates = %d, creates = %d; want one update and only the enable's create", len(ups), len(f.fake.ServiceCreates()))
				}
				uc := ups[0].Spec.UpdateConfig
				if uc == nil || uc.Parallelism != 1 || uc.Order != swarm.UpdateOrderStopFirst || uc.Delay != 10*time.Second { // S3 update-delay decision: 10s
					t.Errorf("update config = %+v, want parallelism 1, stop-first, delay 10s", uc)
				}
				if f.clock.waited() != 0 {
					t.Errorf("a roll waited %d times for tasks, want none", f.clock.waited())
				}
			case "replace":
				if len(writes) != 2 || !strings.HasPrefix(writes[0], "DELETE /services/") || writes[1] != "POST /services/create" {
					t.Fatalf("writes = %v, want the service's removal and then its creation", writes)
				}
				if len(atWait) != 1 || !strings.HasPrefix(atWait[0], "DELETE /services/") {
					t.Errorf("writes while the tasks were awaited = %v, want only the removal, so the create comes after the tasks ended", atWait)
				}
				if len(f.fake.ServiceUpdates()) != 0 {
					t.Errorf("a replace rolled the service: %d updates", len(f.fake.ServiceUpdates()))
				}
				creates := f.fake.ServiceCreates()
				if len(creates) != 2 {
					t.Fatalf("service creates = %d, want the enable's and the replacement", len(creates))
				}
				spec := creates[1].Spec
				if got := envOf(spec, ingressbundle.EnvVIPRouterID); got != strconv.Itoa(rowRouterID(t, f)) {
					t.Errorf("the replacement's router ID = %q, want the row's", got)
				}
				if secs := spec.TaskTemplate.ContainerSpec.Secrets; len(secs) != 1 || secs[0].SecretID == "" {
					t.Errorf("the replacement's secrets = %+v, want the key secret with its ID", secs)
				}
			}
			if got := f.keepalivedSecrets(); len(got) != 1 || len(f.fake.SecretCreates()) != 1 {
				t.Errorf("secrets = %v after %d creates, want the one the enable made", got, len(f.fake.SecretCreates()))
			}
		})
	}
}

// rowRouterID is the router ID of the environment's virtual IP row.
func rowRouterID(t *testing.T, f *vipDisableFix) int {
	t.Helper()
	row, err := f.istore.GetVIP(testsupport.Context(t), f.envID)
	if err != nil {
		t.Fatal(err)
	}
	return row.RouterID
}
