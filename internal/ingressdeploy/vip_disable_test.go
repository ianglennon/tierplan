// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipKeepalivedTask is a running task of the keepalived service, as the swarm keeps one after its service is removed.
func vipKeepalivedTask() swarm.Task {
	return swarm.Task{
		ID: "task-keepalived", ServiceID: "gone", NodeID: "node-1", DesiredState: swarm.TaskStateRunning,
		Annotations: swarm.Annotations{Labels: ingress.KeepalivedLabels()},
		Status:      swarm.TaskStatus{State: swarm.TaskStateRunning},
	}
}

// vipDisableOpts tune a vipDisableFix.
type vipDisableOpts struct {
	// tasks leaves a running keepalived task in the swarm until a test removes it.
	tasks bool
	// config adjusts the fake swarm, after the fixture has.
	config func(*dockertest.FakeConfig)
	deps   func(*ingressdeploy.Deps)
}

// vipDisableFix is a swarm with ingress on, whose clock moves only when the deployer waits on it.
type vipDisableFix struct {
	*vipEnableRig
	clock *steppingClock
}

func newVIPDisableFix(t *testing.T, o vipDisableOpts) *vipDisableFix {
	t.Helper()
	f := &vipDisableFix{clock: newSteppingClock()}
	f.vipEnableRig = newVIPEnableRig(t, func(c *dockertest.FakeConfig) {
		if o.tasks {
			c.Tasks = []swarm.Task{vipKeepalivedTask()}
		}
		if o.config != nil {
			o.config(c)
		}
	}, func(d *ingressdeploy.Deps) {
		d.Clock = f.clock
		if o.deps != nil {
			o.deps(d)
		}
	})
	return f
}

// enabled switches the virtual IP on and fails the test unless the enable is done.
func (f *vipDisableFix) enabled() {
	f.t.Helper()
	wantVIPAllDone(f.t, f.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress}))
}

// disableVIP starts a switch-off and waits for its job to end. The job is returned whatever its outcome.
func (f *vipDisableFix) disableVIP() jobs.Job {
	f.t.Helper()
	id, err := f.svc.DisableVIP(testsupport.Context(f.t), testActor, testOwner, f.envID)
	if err != nil {
		f.t.Fatalf("DisableVIP: %v", err)
	}
	return f.wait(id)
}

// keepalivedService reads the keepalived service the fake swarm holds.
func (f *vipDisableFix) keepalivedService() (dockerx.ServiceDetail, bool) {
	f.t.Helper()
	d, _, err := f.cli.ServiceInspect(testsupport.Context(f.t), ingress.ServiceNameKeepalived)
	if err != nil {
		return dockerx.ServiceDetail{}, false
	}
	return d, true
}

// keepalivedSecrets are the names of the swarm's secrets that start with the key secrets' prefix.
func (f *vipDisableFix) keepalivedSecrets() []string {
	f.t.Helper()
	var out []string
	for _, s := range f.secrets() {
		if strings.HasPrefix(s.Name, ingress.SecretPrefixKeepalived) {
			out = append(out, s.Name)
		}
	}
	slices.Sort(out)
	return out
}

// auditDetails are the details of the only audit row of an action.
func (f *vipDisableFix) auditDetails(action string) map[string]string {
	f.t.Helper()
	if got := f.auditCountResult(action, audit.ResultSuccess); got != 1 {
		f.t.Fatalf("%s audit success rows = %d, want 1", action, got)
	}
	var raw string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ? AND result = ?`, action, string(audit.ResultSuccess)).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var d map[string]string
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		f.t.Fatalf("audit details %q: %v", raw, err)
	}
	return d
}

// wantVIPDisableDone fails the test unless every item of the switch-off ended done.
func wantVIPDisableDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemVIPRemoveService, ingressdeploy.ItemVIPWaitTasks, ingressdeploy.ItemVIPRemoveSecret, ingressdeploy.ItemVIPForget} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// TestDisableVIPRemovesEverything is the tracer of the switch-off (D-16, D-17, D-03): the preview lists the service, its
// tasks and the secret and writes nothing; the job then removes the service, waits until no keepalived task runs, removes the
// key secret and deletes the row with ingress.vip_disabled, and leaves no service, secret, row or mark behind. A key secret of
// another environment is not touched.
func TestDisableVIPRemovesEverything(t *testing.T) {
	f := newVIPDisableFix(t, vipDisableOpts{tasks: true})
	ctx := testsupport.Context(t)
	f.enabled()
	secret := ingressdeploy.KeepalivedSecretName(f.envID, 1)

	// A key secret of another environment, which the switch-off must leave.
	other := ingress.SecretPrefixKeepalived + "g1-someoneelse"
	if _, err := f.cli.SecretCreate(ctx, dockerx.SecretSpec{Name: other, Labels: ingress.KeepalivedLabels(), Data: []byte("hex:x")}); err != nil {
		t.Fatalf("SecretCreate: %v", err)
	}

	pv, err := f.svc.PreviewVIPDisable(ctx, f.envID)
	if err != nil {
		t.Fatalf("PreviewVIPDisable: %v", err)
	}
	if pv.Service != ingress.ServiceNameKeepalived || pv.Tasks != 1 || !slices.Equal(pv.Secrets, []string{secret}) || len(pv.Items) != 4 || pv.Address.String() != vipTestAddress {
		t.Fatalf("preview = %+v, want the service, one task, the environment's secret, four items and the address", pv)
	}
	before := len(f.fake.Writes())

	// The holder's task outlives the service for two polls, as a node takes a moment to stop its container.
	var atRelease []string
	f.clock.hook = func(n int) {
		if n == 2 {
			atRelease = f.fake.Writes()[before:]
			f.fake.Reconfigure(f.base)
		}
	}
	job := f.disableVIP()
	wantVIPDisableDone(t, job)
	if job.Kind != ingressdeploy.JobKindVIPDisable || job.EnvID != f.envID || job.Owner != testOwner {
		t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
	}

	// The service went first, and the secret only after the tasks were gone.
	if len(atRelease) != 1 || !strings.HasPrefix(atRelease[0], "DELETE /services/") {
		t.Errorf("writes while the tasks were awaited = %v, want only the service's removal", atRelease)
	}
	writes := f.fake.Writes()[before:]
	if len(writes) != 2 || !strings.HasPrefix(writes[0], "DELETE /services/") || !strings.HasPrefix(writes[1], "DELETE /secrets/") {
		t.Errorf("writes = %v, want the service's removal and then the secret's", writes)
	}
	if got := f.clock.waited(); got != 2 {
		t.Errorf("waited %d times, want 2 (the task went on the second)", got)
	}

	if _, ok := f.keepalivedService(); ok {
		t.Error("the keepalived service is still in the swarm")
	}
	if got := f.keepalivedSecrets(); !slices.Equal(got, []string{other}) {
		t.Errorf("key secrets left = %v, want only the other environment's %s", got, other)
	}
	if _, err := f.istore.GetVIP(ctx, f.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("GetVIP after the switch-off = %v, want ErrVIPNotFound", err)
	}
	d := f.auditDetails("ingress.vip_disabled")
	if d["address"] != vipTestAddress || d["generation"] != "1" || d["router_id"] == "" {
		t.Errorf("audit details = %v, want address, router_id and generation", d)
	}
	if f.svc.VIPIsDisabling(f.envID) {
		t.Error("the reader is still marked as watching a switch-off")
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("the operation lock is still held after the job")
	}

	// A switch-off of what is no longer there is refused, and writes nothing more.
	if _, err := f.svc.DisableVIP(ctx, testActor, testOwner, f.envID); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
		t.Errorf("a second DisableVIP = %v, want ErrVIPNotEnabled", err)
	}
	if _, err := f.svc.PreviewVIPDisable(ctx, f.envID); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
		t.Errorf("PreviewVIPDisable of nothing = %v, want ErrVIPNotEnabled", err)
	}
}

// wantSwitchOffLeft fails the test unless the switch-off ended with the item failed with vip_remove_failed, the items
// before it done, the items after it skipped as not run (except those named in tried, which ran), the row still there with
// enabled 0, the reader's mark off and the lock free.
func wantSwitchOffLeft(t *testing.T, f *vipDisableFix, job jobs.Job, failed string, tried ...string) {
	t.Helper()
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done: %+v", job.Items)
	}
	seen := false
	for _, id := range []string{ingressdeploy.ItemVIPRemoveService, ingressdeploy.ItemVIPWaitTasks, ingressdeploy.ItemVIPRemoveSecret, ingressdeploy.ItemVIPForget} {
		it := itemOf(job, id)
		switch {
		case id == failed:
			seen = true
			if it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPRemoveFailed || it.Message == "" {
				t.Errorf("item %s = %q code %q message %q, want failed with vip_remove_failed and a sentence", id, it.State, it.Code, it.Message)
			}
		case !seen || slices.Contains(tried, id):
			if it.State != jobs.ItemDone {
				t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
			}
		default:
			if it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
				t.Errorf("item %s = %q (code %q), want skipped and not_run", id, it.State, it.Code)
			}
		}
	}
	row, err := f.istore.GetVIP(testsupport.Context(t), f.envID)
	if err != nil || row.Enabled {
		t.Errorf("row = %+v, %v; want it kept with enabled 0 for the ensure loop to finish", row, err)
	}
	if got := f.auditCountResult("ingress.vip_disabled", audit.ResultSuccess); got != 0 {
		t.Errorf("a switch-off that left things behind wrote %d ingress.vip_disabled rows", got)
	}
	if f.svc.VIPIsDisabling(f.envID) {
		t.Error("the reader is still marked as watching a switch-off")
	}
	if _, held := f.locks.Held(f.envID); held {
		t.Error("the operation lock is still held after the job")
	}
}

// TestDisableVIPFailures (PROH-26-06): a service that cannot be removed, a secret that cannot be removed and tasks that never
// stop each end their item with vip_remove_failed and leave the row switched off; the tasks that never stop still let the
// secret be tried; and switching off again once the cause is gone finishes the job.
func TestDisableVIPFailures(t *testing.T) {
	t.Run("the service cannot be removed", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		f.fake.SetServiceRemoveError(ingress.ServiceNameKeepalived, "boom: engine words")

		job := f.disableVIP()
		wantSwitchOffLeft(t, f, job, ingressdeploy.ItemVIPRemoveService)
		if _, ok := f.keepalivedService(); !ok {
			t.Error("the service is gone although its removal failed")
		}
		if got := f.keepalivedSecrets(); len(got) != 1 {
			t.Errorf("key secrets = %v, want the one still there", got)
		}

		f.fake.SetServiceRemoveError(ingress.ServiceNameKeepalived, "")
		wantVIPDisableDone(t, f.disableVIP())
		if _, err := f.istore.GetVIP(testsupport.Context(t), f.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
			t.Errorf("GetVIP after the second switch-off = %v, want ErrVIPNotFound", err)
		}
	})

	t.Run("the secret cannot be removed", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		secret := ingressdeploy.KeepalivedSecretName(f.envID, 1)
		cfg := f.base
		cfg.RemoveErrors = map[string]string{secret: "boom: " + strings.Repeat("a", 64)}
		f.fake.Reconfigure(cfg)

		job := f.disableVIP()
		wantSwitchOffLeft(t, f, job, ingressdeploy.ItemVIPRemoveSecret)
		if _, ok := f.keepalivedService(); ok {
			t.Error("the service is still there although it was removed first")
		}
		// No engine word rides with an item that touched the key.
		if it := itemOf(job, ingressdeploy.ItemVIPRemoveSecret); it.Detail != "" || keyShape.MatchString(it.Message) {
			t.Errorf("the secret item carries %q / %q, want none of the engine's words", it.Detail, it.Message)
		}

		f.fake.Reconfigure(f.base)
		wantVIPDisableDone(t, f.disableVIP())
		if got := f.keepalivedSecrets(); len(got) != 0 {
			t.Errorf("key secrets left = %v after the second switch-off", got)
		}
	})

	t.Run("the tasks never stop", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{tasks: true})
		f.enabled()

		job := f.disableVIP()
		// The key secret is still tried and removed; the row is kept, since an address may still be held.
		wantSwitchOffLeft(t, f, job, ingressdeploy.ItemVIPWaitTasks, ingressdeploy.ItemVIPRemoveSecret)
		if it := itemOf(job, ingressdeploy.ItemVIPForget); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
			t.Errorf("forget = %q (code %q), want skipped and not_run", it.State, it.Code)
		}
		if got := f.keepalivedSecrets(); len(got) != 0 {
			t.Errorf("key secrets = %v, want the secret tried and removed", got)
		}
		// Sixty seconds of the injected clock, polled every PollInterval, and no real wait.
		if got, max := f.clock.waited(), int(ingressdeploy.VIPDisableTaskDeadline/stackops.PollInterval)+1; got < max-1 || got > max {
			t.Errorf("waited %d times, want about %d", got, max)
		}

		f.fake.Reconfigure(f.base)
		wantVIPDisableDone(t, f.disableVIP())
	})
}

// TestDisableVIPRefusals: a switch-off with no virtual IP is ErrVIPNotEnabled for the preview and the job, a held lock is a
// LockedError with nothing written, and a manager that does not allow privileged services does not block a switch-off, since
// removing a service is not an escalation (D-19).
func TestDisableVIPRefusals(t *testing.T) {
	t.Run("no virtual IP", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		ctx := testsupport.Context(t)
		if _, err := f.svc.PreviewVIPDisable(ctx, f.envID); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			t.Errorf("PreviewVIPDisable = %v, want ErrVIPNotEnabled", err)
		}
		if _, err := f.svc.DisableVIP(ctx, testActor, testOwner, f.envID); !errors.Is(err, ingressdeploy.ErrVIPNotEnabled) {
			t.Errorf("DisableVIP = %v, want ErrVIPNotEnabled", err)
		}
		if w := f.fake.Writes(); len(w) != 0 {
			t.Errorf("wrote %v", w)
		}
		if _, held := f.locks.Held(f.envID); held {
			t.Error("the operation lock is still held")
		}
	})

	t.Run("the lock is held", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		ctx := testsupport.Context(t)
		f.enabled()
		release, _, ok := f.locks.TryAcquire(f.envID, stackops.Holder{Kind: stackops.HolderKindIngress, By: "user:someone-else", Started: time.Now()})
		if !ok {
			t.Fatal("could not take the lock")
		}
		defer release()
		writes := len(f.fake.Writes())

		_, err := f.svc.DisableVIP(ctx, testActor, testOwner, f.envID)
		var locked *ingressdeploy.LockedError
		if !errors.As(err, &locked) || locked.Holder.By != "user:someone-else" {
			t.Fatalf("DisableVIP = %v, want a LockedError naming the holder", err)
		}
		if got := len(f.fake.Writes()); got != writes {
			t.Errorf("a refused switch-off wrote %v", f.fake.Writes()[writes:])
		}
		if row, err := f.istore.GetVIP(ctx, f.envID); err != nil || !row.Enabled {
			t.Errorf("row = %+v, %v; want it untouched and enabled", row, err)
		}
	})

	t.Run("a manager refuses privileged services", func(t *testing.T) {
		f := newVIPDisableFix(t, vipDisableOpts{})
		f.enabled()
		f.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerRefuses, envs.ManagerUnknown), nil)

		pv, err := f.svc.PreviewVIPDisable(testsupport.Context(t), f.envID)
		if err != nil || pv.Service == "" {
			t.Errorf("PreviewVIPDisable = %+v, %v; want the service named", pv, err)
		}
		wantVIPDisableDone(t, f.disableVIP())
	})
}

// vipAgentEngine answers OnNode with the engine of the node whose agent ID is asked for, as the node's agent would, and every
// other call as the engine it wraps.
type vipAgentEngine struct {
	ingressdeploy.Engine
	clis map[string]*dockerx.Client
}

func (e *vipAgentEngine) OnNode(ctx context.Context, envID, agentID string, fn func(*dockerx.Client) error) error {
	if c, ok := e.clis[agentID]; ok {
		return fn(c)
	}
	return e.Engine.OnNode(ctx, envID, agentID, fn)
}

// TestDisableVIPRecordsTheLastMove: with the reader showing node-a as the holder, a switch-off marks the environment as being
// disabled and reads the nodes while it waits, so the holder's STOP and the address landing on node-b are one move recorded
// with the reason disabled, and not as a failure of the holder.
func TestDisableVIPRecordsTheLastMove(t *testing.T) {
	// A task container carries the swarm's service name as well as the service's own labels.
	labels := ingress.KeepalivedLabels()
	labels["com.docker.swarm.service.name"] = ingress.ServiceNameKeepalived
	newNode := func(id string) (*dockertest.Fake, *dockerx.Client) {
		fake := dockertest.NewFake(t, dockertest.FakeConfig{Containers: []dockertest.FakeContainer{{
			ID: id, Names: []string{"/" + id}, State: "running", Labels: labels,
		}}})
		cli, err := dockerx.New(fake)
		if err != nil {
			t.Fatalf("dockerx.New: %v", err)
		}
		t.Cleanup(func() { _ = cli.Close() })
		return fake, cli
	}
	fa, ca := newNode("ka-a")
	fb, cb := newNode("ka-b")
	f := newVIPDisableFix(t, vipDisableOpts{tasks: true, deps: func(d *ingressdeploy.Deps) {
		d.Engine = &vipAgentEngine{Engine: d.Engine, clis: map[string]*dockerx.Client{"agent-a": ca, "agent-b": cb}}
	}})
	f.envs.set(func(i *ingressdeploy.EnvInfo) {
		i.Nodes = []ingressdeploy.NodeInfo{
			{SwarmNodeID: vipNodeA, AgentID: "agent-a", Name: "node-a", Role: "manager", Connected: true},
			{SwarmNodeID: vipNodeB, AgentID: "agent-b", Name: "node-b", Role: "manager", Connected: true},
		}
	})
	ctx := testsupport.Context(t)
	f.enabled()

	fa.AppendLogs("ka-a",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeA, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeA, 2, ingressbundle.VIPStateMaster, "")),
	)
	fb.AppendLogs("ka-b",
		logLine(t, vipT0, ingressbundle.VIPEvent{Event: ingressbundle.VIPEventStart, Node: vipNodeB, Seq: 1, Iface: "eth0"}),
		logLine(t, vipT0.Add(time.Second), vipStateEvent(vipNodeB, 2, ingressbundle.VIPStateBackup, "")),
	)
	f.svc.ReadVIPOnce(ctx, f.envID)
	if got := f.svc.VIPHolders(f.envID); len(got) != 1 || got[0].Hostname != "node-a" {
		t.Fatalf("holders before the switch-off = %+v, want node-a", got)
	}

	// The holder stops with the service and the other node takes the address, as the swarm's tasks end one after the other.
	f.clock.hook = func(n int) {
		switch n {
		case 1:
			fa.AppendLogs("ka-a", logLine(t, vipT0.Add(10*time.Second), vipStateEvent(vipNodeA, 3, ingressbundle.VIPStateStop, "")))
			fb.AppendLogs("ka-b", logLine(t, vipT0.Add(11*time.Second), vipStateEvent(vipNodeB, 3, ingressbundle.VIPStateMaster, "")))
		case 2:
			f.fake.Reconfigure(f.base)
		}
	}
	wantVIPDisableDone(t, f.disableVIP())

	moves, err := f.istore.VIPMovesSince(ctx, f.envID, time.Time{})
	if err != nil {
		t.Fatalf("VIPMovesSince: %v", err)
	}
	if len(moves) != 1 {
		t.Fatalf("%d moves stored, want the one: %+v", len(moves), moves)
	}
	if m := moves[0]; m.FromNodeName != "node-a" || m.ToNodeName != "node-b" || m.Reason != ingressdeploy.VIPMoveDisabled {
		t.Errorf("move = %+v, want node-a to node-b with reason %q", m, ingressdeploy.VIPMoveDisabled)
	}
}
