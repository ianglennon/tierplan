// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
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
	if got := f.auditCount(action); got != 1 {
		f.t.Fatalf("%s audit rows = %d, want 1", action, got)
	}
	var raw string
	if err := f.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = ?`, action).Scan(&raw); err != nil {
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
