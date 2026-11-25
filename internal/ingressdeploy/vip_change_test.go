// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
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

// wantChangeDone fails the test unless every item of the change ended done.
func wantChangeDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemVIPChangeService, ingressdeploy.ItemVIPChangeWait, ingressdeploy.ItemVIPChangeSave} {
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

// TestChangeVIPRefusedWhileARemovalIsPending pins IN-01: the row of a switch-off that failed part way is off, and the ensure
// loop removes what is left, so a change of it is refused with a stable code before anything is written, instead of
// recreating the service and finishing done only to be removed again.
func TestChangeVIPRefusedWhileARemovalIsPending(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	ctx := testsupport.Context(t)
	f.enabled()
	if err := f.istore.SetVIPEnabled(ctx, f.envID, false); err != nil {
		t.Fatalf("SetVIPEnabled: %v", err)
	}
	wantChangeRefused(t, f, ingressdeploy.VIPRequest{Address: "10.9.0.60/24"}, ingressdeploy.CodeVIPRemovalPending)
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
			pv.Warning != ingressdeploy.VIPPrivilegeWarning || len(pv.Managers) != 3 || len(pv.Items) != 3 {
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
					t.Errorf("a roll of a swarm that settles at once waited %d times, want none", f.clock.waited())
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

// rolling scripts the three nodes' keepalived tasks: running tells, per node, whether it runs the new configuration yet.
func (f *vipDisableFix) rolling(running ...bool) {
	var tasks []dockertest.FakeServiceTask
	for i, now := range running {
		tasks = append(tasks, dockertest.FakeServiceTask{
			ID: "keepalived-" + strconv.Itoa(i+1), Node: "node-" + strconv.Itoa(i+1), State: "running", Old: !now,
		})
	}
	f.fake.SetServiceTasks(ingress.ServiceNameKeepalived, tasks...)
}

// TestChangeVIPWaitsForTheRoll: a change is done, and the new address saved, only once every node runs the new configuration
// (UAT-26 issue 3). Until then the job runs, the row and so the status keep the address that is still served, and the
// operation lock is held, so the ensure loop cannot put the previous address back under the roll.
func TestChangeVIPWaitsForTheRoll(t *testing.T) {
	const newAddress = "10.9.0.60/24"
	f := newVIPDisableFix(t, vipDisableOpts{})
	ctx := testsupport.Context(t)
	f.enabled()
	f.rolling(false, false, false)
	f.clock.hook = func(n int) {
		row, err := f.istore.GetVIP(ctx, f.envID)
		if err != nil || row.Prefix().String() != vipTestAddress {
			t.Errorf("wait %d: the row says %v (%v) while nodes still serve %s", n, row.Prefix(), err, vipTestAddress)
		}
		if _, held := f.locks.Held(f.envID); !held {
			t.Errorf("wait %d: the operation lock is free while the roll runs", n)
		}
		if got := f.auditCount("ingress.vip_changed"); got != 0 {
			t.Errorf("wait %d: the change is audited before it is made", n)
		}
		switch n {
		case 1:
			f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "updating", "update in progress")
			f.rolling(true, false, false)
		case 2:
			f.rolling(true, true, true)
		case 3: // Every task runs; the swarm is still watching the last one.
			f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "completed", "update completed")
		}
	}
	job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
	wantChangeDone(t, job)
	if got := f.clock.waited(); got != 3 {
		t.Errorf("the job waited %d times, want 3", got)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.Result["running"] != "3" || it.Result["wanted"] != "3" {
		t.Errorf("wait result = %v, want 3 of 3", it.Result)
	}
	if row, _ := f.istore.GetVIP(ctx, f.envID); row.Prefix().String() != newAddress {
		t.Errorf("row = %v, want %s", row.Prefix(), newAddress)
	}
	if got := f.auditCount("ingress.vip_changed"); got != 1 {
		t.Errorf("audit rows = %d, want 1", got)
	}
}

// TestChangeVIPThatTakesTooLongIsSaved: a node that does not restart in time ends the wait failed, says so, and the new address
// is saved all the same: the swarm took it and carries on, and a row that kept the old address would have the ensure loop move
// every node back.
func TestChangeVIPThatTakesTooLongIsSaved(t *testing.T) {
	const newAddress = "10.9.0.60/24"
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	f.rolling(true, true, false)
	start := f.clock.Now()
	job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
	if d := f.clock.Now().Sub(start); d < ingressdeploy.VIPChangeWaitDeadline || d > ingressdeploy.VIPChangeWaitDeadline+time.Minute {
		t.Errorf("the job waited %v, want the deadline of %v", d, ingressdeploy.VIPChangeWaitDeadline)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPChangeFailed || it.Message == "" || it.Result["running"] != "2" || it.Result["wanted"] != "3" {
		t.Errorf("wait item = %+v, want failed with 2 of 3", it)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeSave); job.State != jobs.JobPartial || it.State != jobs.ItemDone {
		t.Errorf("job = %q, save item = %+v, want a partial job that saved", job.State, it)
	}
	if row, _ := f.istore.GetVIP(testsupport.Context(t), f.envID); row.Prefix().String() != newAddress {
		t.Errorf("row = %v, want %s", row.Prefix(), newAddress)
	}
	if got := f.auditCount("ingress.vip_changed"); got != 1 {
		t.Errorf("audit rows = %d, want 1", got)
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// TestChangeVIPTheSwarmRolledBackIsNotSaved: an update the swarm rolled back leaves the nodes on the previous address, so the
// row keeps it, nothing is audited, and the ensure loop does not write the refused configuration again.
func TestChangeVIPTheSwarmRolledBackIsNotSaved(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	ctx := testsupport.Context(t)
	f.enabled()
	before, _ := f.istore.GetVIP(ctx, f.envID)
	// The status an earlier update left is not this one's: it must not end the wait.
	f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "paused", "an earlier update was paused")
	f.rolling(false, false, false)
	f.clock.hook = func(n int) {
		if n == 2 {
			f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "rollback_completed", "rollback completed")
		}
	}
	job := f.changeVIP(ingressdeploy.VIPRequest{Address: "10.9.0.60/24"})
	if got := f.clock.waited(); got != 2 {
		t.Errorf("the job waited %d times, want 2: the status the earlier update left is not this one's", got)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPChangeFailed || !strings.Contains(it.Detail, "not saved") {
		t.Errorf("wait item = %+v, want failed, saying the address was not saved", it)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeSave); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
		t.Errorf("save item = %+v, want not run", it)
	}
	if after, _ := f.istore.GetVIP(ctx, f.envID); after != before {
		t.Errorf("a rolled-back change touched the row: %+v, was %+v", after, before)
	}
	if got := f.auditCount("ingress.vip_changed"); got != 0 {
		t.Errorf("a rolled-back change wrote %d audit rows", got)
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// TestChangeVIPCancelledWhileItWaitsIsSaved: a cancel ends the wait and nothing else, because the swarm carries on with what it
// took.
func TestChangeVIPCancelledWhileItWaitsIsSaved(t *testing.T) {
	const newAddress = "10.9.0.60/24"
	f := newVIPDisableFix(t, vipDisableOpts{})
	ctx := testsupport.Context(t)
	f.enabled()
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
	id, err := f.svc.ChangeVIP(ctx, testActor, testOwner, f.envID, ingressdeploy.VIPRequest{Address: newAddress})
	if err != nil {
		t.Fatalf("ChangeVIP: %v", err)
	}
	close(started)
	job := f.wait(id)
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemCancelled {
		t.Errorf("wait item = %+v, want cancelled", it)
	}
	if it := itemOf(job, ingressdeploy.ItemVIPChangeSave); it.State != jobs.ItemDone {
		t.Errorf("save item = %+v, want done", it)
	}
	if row, _ := f.istore.GetVIP(ctx, f.envID); row.Prefix().String() != newAddress {
		t.Errorf("row = %v, want %s", row.Prefix(), newAddress)
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// TestChangeVIPTheSwarmIsRollingBackIsNotSaved: a wait that ends for another reason, a cancel or the deadline, while swarm puts
// the previous configuration back does not save the refused address, or the ensure loop would roll it out again.
func TestChangeVIPTheSwarmIsRollingBackIsNotSaved(t *testing.T) {
	for _, tc := range []struct {
		name, state, detail string
		cancel              bool
	}{
		{"cancelled while it rolls back", "rollback_started", "put back", true},
		{"out of time while it rolls back", "rollback_started", "put back", false},
		{"cancelled as the rollback ends", "rollback_completed", "put back", true},
		{"the rollback stopped too", "rollback_paused", "until TierPlan has put", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVIPDisableFix(t, vipDisableOpts{})
			ctx := testsupport.Context(t)
			f.enabled()
			before, _ := f.istore.GetVIP(ctx, f.envID)
			f.rolling(false, false, false)
			var id string
			started := make(chan struct{})
			f.clock.hook = func(n int) {
				if n != 1 {
					return
				}
				<-started
				f.fake.SetUpdateState(ingress.ServiceNameKeepalived, tc.state, "rollback")
				if tc.cancel {
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
			job := f.wait(id)
			if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemFailed || !strings.Contains(it.Detail, tc.detail) {
				t.Errorf("wait item = %+v, want failed, saying %q", it, tc.detail)
			}
			if it := itemOf(job, ingressdeploy.ItemVIPChangeSave); it.State != jobs.ItemSkipped {
				t.Errorf("save item = %+v, want not run", it)
			}
			if after, _ := f.istore.GetVIP(ctx, f.envID); after != before {
				t.Errorf("the row was touched: %+v, was %+v", after, before)
			}
			if got := f.auditCount("ingress.vip_changed"); got != 0 {
				t.Errorf("audit rows = %d, want none", got)
			}
		})
	}
}

// TestChangeVIPWaitsForANodeThatKeepsFailing: between two tries of a node's task swarm wants nothing of it running, so the
// nodes that do run read as all of them. The wait counts the task that waits for its restart.
func TestChangeVIPWaitsForANodeThatKeepsFailing(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	f.fake.SetServiceTasks(ingress.ServiceNameKeepalived,
		dockertest.FakeServiceTask{ID: "k1", Node: "node-1", State: "running"},
		dockertest.FakeServiceTask{ID: "k2", Node: "node-2", State: "running"},
		dockertest.FakeServiceTask{ID: "k3b", Node: "node-3", State: "ready", Desired: "ready"},
		dockertest.FakeServiceTask{ID: "k3a", Node: "node-3", State: "rejected", Desired: "shutdown", Err: "No such image"})
	job := f.changeVIP(ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(77)})
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.State != jobs.ItemFailed || it.Result["running"] != "2" || it.Result["wanted"] != "3" {
		t.Errorf("wait item = %+v, want failed with 2 of 3", it)
	}
	if row, _ := f.istore.GetVIP(testsupport.Context(t), f.envID); row.RouterID != 77 {
		t.Errorf("router ID = %d, want 77 saved: the swarm carries on", row.RouterID)
	}
}

// TestChangeVIPRolledBackAfterTheSaveIsNotRepeated: a change saved after a wait that was too long, which swarm rolls back later,
// is not written again at once by the ensure loop.
func TestChangeVIPRolledBackAfterTheSaveIsNotRepeated(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	d, ok := f.keepalivedService()
	if !ok {
		t.Fatal("no keepalived service")
	}
	var before swarm.ServiceSpec
	if err := json.Unmarshal(d.Spec, &before); err != nil {
		t.Fatal(err)
	}
	f.rolling(true, true, false)
	f.changeVIP(ingressdeploy.VIPRequest{Address: "10.9.0.60/24"})
	f.fake.ReplaceServiceSpec(ingress.ServiceNameKeepalived, before)
	f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "rollback_completed", "rollback completed")
	ups := len(f.fake.ServiceUpdates())
	f.round()
	if got := len(f.fake.ServiceUpdates()); got != ups {
		t.Errorf("the ensure loop wrote the refused configuration again: %d updates, were %d", got, ups)
	}
	if row, _ := f.istore.GetVIP(testsupport.Context(t), f.envID); row.EnsureError != ingressdeploy.EnsureUpdateFailed {
		t.Errorf("ensure class = %q, want %q", row.EnsureError, ingressdeploy.EnsureUpdateFailed)
	}
}

// TestEnsureGoesOnWithAStoppedRollback: the service names the wanted configuration again, but swarm gave up putting it back, so
// nodes may run the refused one. The loop writes it once more, which makes swarm go on, and does not repeat that every pass when
// it stops again.
func TestEnsureGoesOnWithAStoppedRollback(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{})
	f.enabled()
	f.round()
	ups := len(f.fake.ServiceUpdates())
	f.fake.SetUpdateState(ingress.ServiceNameKeepalived, "rollback_paused", "rollback paused")
	f.round()
	got := f.fake.ServiceUpdates()
	if len(got) != ups+1 || envOf(got[ups].Spec, ingressbundle.EnvVIPAddress) != vipTestAddress {
		t.Fatalf("updates = %d, were %d; want the wanted configuration written once more", len(got), ups)
	}
	for range 3 {
		f.round()
	}
	if n := len(f.fake.ServiceUpdates()); n != ups+1 {
		t.Errorf("updates = %d, want the one: the spec was tried and swarm stopped again", n)
	}
	if row, _ := f.istore.GetVIP(testsupport.Context(t), f.envID); row.EnsureError != ingressdeploy.EnsureUpdateFailed {
		t.Errorf("ensure class = %q, want %q", row.EnsureError, ingressdeploy.EnsureUpdateFailed)
	}
}

// TestChangeVIPWithEveryNodePaused: no node can take the change now, so there is nothing to wait for.
func TestChangeVIPWithEveryNodePaused(t *testing.T) {
	for _, req := range []ingressdeploy.VIPRequest{{Address: "10.9.0.60/24"}, {Address: vipTestAddress, RouterID: intp(77)}} {
		f := newVIPDisableFix(t, vipDisableOpts{config: func(c *dockertest.FakeConfig) {
			for i := range c.NodeRows {
				c.NodeRows[i].Availability = "pause"
			}
		}})
		f.enabled()
		f.rolling(false, false, false)
		if req.RouterID != nil {
			// The service is made again and swarm makes no task on a paused node.
			f.fake.SetServiceTasks(ingress.ServiceNameKeepalived, dockertest.FakeServiceTask{ID: "gone", Node: "node-1", State: "shutdown", Desired: "shutdown", Old: true})
		}
		job := f.changeVIP(req)
		wantChangeDone(t, job)
		lines, _ := f.hub.Output(job.ID)
		if f.clock.waited() != 0 || !slices.ContainsFunc(lines, func(l jobs.Output) bool { return strings.Contains(l.Text, "manager-1, manager-2, manager-3") }) {
			t.Errorf("%d waits, output %v; want none and the three nodes named", f.clock.waited(), lines)
		}
	}
}

// TestChangeVIPDoesNotWaitForAPausedNode: swarm leaves a paused node's task alone, so the change is done without it and the job
// names it. A node that is not paused is waited for.
func TestChangeVIPDoesNotWaitForAPausedNode(t *testing.T) {
	const newAddress = "10.9.0.60/24"
	f := newVIPDisableFix(t, vipDisableOpts{config: func(c *dockertest.FakeConfig) { c.NodeRows[2].Availability = "pause" }})
	f.enabled()
	f.rolling(true, true, false)
	job := f.changeVIP(ingressdeploy.VIPRequest{Address: newAddress})
	wantChangeDone(t, job)
	if it := itemOf(job, ingressdeploy.ItemVIPChangeWait); it.Result["running"] != "2" || it.Result["wanted"] != "2" || f.clock.waited() != 0 {
		t.Errorf("wait item = %+v after %d waits, want 2 of 2 at once", it, f.clock.waited())
	}
	var out []string
	lines, _ := f.hub.Output(job.ID)
	for _, l := range lines {
		out = append(out, l.Text)
	}
	if text := strings.Join(out, "\n"); !strings.Contains(text, "manager-3") || strings.Contains(text, "manager-2") {
		t.Errorf("output = %q, want the paused node named", text)
	}
}
