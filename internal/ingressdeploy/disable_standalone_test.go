// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/ingresshub"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// standaloneDisableRig is a standalone host with ingress switched on, its proxy up and enrolled, and the deployer's
// switch-off dependencies (the kicker and the node rows) in place.
type standaloneDisableRig struct {
	*standaloneRig
	kicker *kickRecorder
}

// newStandaloneDisableRig enables ingress on an empty standalone host through the job, so the fake host holds the proxy
// container, the network and the volume the enable leaves, and enrols the proxy's node row.
func newStandaloneDisableRig(t *testing.T, deps func(*ingressdeploy.Deps)) *standaloneDisableRig {
	t.Helper()
	k := &kickRecorder{}
	r := newStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) {
		d.Kicker = k
		rows, ok := d.Tokens.(*ingresshub.Service)
		if !ok {
			t.Fatalf("the tokens are %T, want the hub service", d.Tokens)
		}
		d.NodeRows = rows
		if deps != nil {
			deps(d)
		}
	}})
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
	r.addNodeRow("active")
	k.mu.Lock()
	k.calls = nil
	k.mu.Unlock()
	return &standaloneDisableRig{standaloneRig: r, kicker: k}
}

// disable runs a switch-off and waits for its job to end.
func (r *standaloneDisableRig) disable() jobs.Job {
	r.t.Helper()
	id, err := r.svc.Disable(testsupport.Context(r.t), testActor, testOwner, r.envID, seesAll)
	if err != nil {
		r.t.Fatalf("Disable: %v", err)
	}
	return r.wait(id)
}

// hostHolds reports what the standalone host holds of the ingress proxy's objects, read through the engine as a person
// would: its containers by the component label, its networks by name and its volumes by name.
func (r *standaloneDisableRig) hostHolds() (container, network, volume bool) {
	r.t.Helper()
	ctx := testsupport.Context(r.t)
	cs, err := r.cli.ContainersWithLabel(ctx, ingress.LabelComponent, ingress.ComponentIngress)
	if err != nil {
		r.t.Fatalf("ContainersWithLabel: %v", err)
	}
	nets, err := r.cli.Networks(ctx)
	if err != nil {
		r.t.Fatalf("Networks: %v", err)
	}
	vol, err := r.cli.VolumeExists(ctx, ingress.VolumeName)
	if err != nil {
		r.t.Fatalf("VolumeExists: %v", err)
	}
	return len(cs) > 0, slices.ContainsFunc(nets, func(n dockerx.NetworkRow) bool { return n.Name == ingress.NetworkName }), vol
}

// wantStandaloneDisableDone fails the test unless the job is the standalone switch-off and every item ended done.
func wantStandaloneDisableDone(t *testing.T, job jobs.Job) {
	t.Helper()
	want := []string{"self_route", "switch_off", "proxy", "network", "volumes", "token", "nodes"}
	var got []string
	for _, it := range job.Items {
		got = append(got, it.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %v, want %v", got, want)
	}
	if job.Kind != ingressdeploy.JobKindDisable {
		t.Errorf("job kind = %q, want %q", job.Kind, ingressdeploy.JobKindDisable)
	}
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}
}

// TestStandaloneDisableRemovesEverything is the tracer: a switch-off of a standalone host whose proxy has enrolled stops
// delivery, closes the proxy's session, takes the project down, confirms the network gone, removes the cache volume,
// disables the join token and removes the node row, each in its item and in that order, and leaves the host with none of the
// three objects.
func TestStandaloneDisableRemovesEverything(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	ctx := testsupport.Context(t)
	if c, n, v := r.hostHolds(); !c || !n || !v {
		t.Fatalf("before the switch-off the host holds container %v, network %v, volume %v; want all three", c, n, v)
	}

	prev, err := r.svc.PreviewDisable(ctx, r.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if len(prev.Findings) != 0 || prev.Removes.Container == "" || prev.Removes.Network != ingress.NetworkName ||
		len(prev.Removes.Volumes) != 1 || prev.Removes.Volumes[0].ID != testAgentID || len(prev.Unreachable) != 0 {
		t.Fatalf("preview = %+v, want nothing in the way, the container, the network and the one host's volume", prev)
	}

	job := r.disable()
	wantStandaloneDisableDone(t, job)

	if st, err := r.istore.DeployState(ctx, r.envID); err != nil || st.Enabled {
		t.Fatalf("DeployState = %+v, %v; want ingress off", st, err)
	}
	if got := r.auditCount("ingress.disabled"); got != 1 {
		t.Errorf("ingress.disabled audit rows = %d, want 1", got)
	}
	if got := r.kicker.calls; len(got) != 1 || got[0] != r.envID+" ingress_disabled" {
		t.Errorf("KickEnv calls = %v, want exactly one for the environment", got)
	}
	if got := r.sys.downs; len(got) != 1 || got[0] != r.envID+"/"+testAgentID {
		t.Errorf("DownSystem calls = %v, want one for the host's node", got)
	}

	if c, n, v := r.hostHolds(); c || n || v {
		t.Errorf("after the switch-off the host holds container %v, network %v, volume %v; want none", c, n, v)
	}
	if it := itemOf(job, ingressdeploy.ItemVolumes); it.Result["removed"] != "true" {
		t.Errorf("the volumes item's result = %v, want the volume removed", it.Result)
	}

	tok, err := r.tokens.TokenStatus(ctx, r.envID)
	if err != nil || !tok.Exists || !tok.Disabled {
		t.Errorf("TokenStatus = %+v, %v; want a disabled token that still exists", tok, err)
	}
	if rows, err := r.tokens.ListNodes(ctx, r.envID); err != nil || len(rows) != 0 {
		t.Errorf("ListNodes = %+v, %v; want no node row", rows, err)
	}
	for action, want := range map[string]int{
		"ingress.disabled": 1, "ingress.join_token_disabled": 1, "ingress.node_removed": 1, "ingress.wipe_queued": 0,
	} {
		if got := r.auditCount(action); got != want {
			t.Errorf("%s audit rows = %d, want %d", action, got, want)
		}
	}

	// The operation lock is free: it was given back before the job ended.
	if holder, held := r.locks.Held(r.envID); held {
		t.Errorf("the operation lock is still held after the job: %+v", holder)
	}
}

// wantStandaloneRefused runs the preview and the switch-off and fails unless both refuse with exactly the one finding code,
// the switch-off as a *DisableRefusedError and with no job; it then checks that the refusal changed nothing of the switch,
// the host, the token, the node rows or the wipes, and gave the lock back. It returns the finding as the preview reports it.
func (r *standaloneDisableRig) wantStandaloneRefused(code string) ingressdeploy.Finding {
	r.t.Helper()
	ctx := testsupport.Context(r.t)
	prev, err := r.svc.PreviewDisable(ctx, r.envID, seesAll)
	if err != nil {
		r.t.Fatalf("PreviewDisable: %v", err)
	}
	if len(prev.Findings) != 1 || prev.Findings[0].Code != code || prev.Findings[0].Severity != ingressdeploy.SeverityError {
		r.t.Fatalf("preview findings = %+v, want exactly %s as an error", prev.Findings, code)
	}
	id, err := r.svc.Disable(ctx, testActor, testOwner, r.envID, seesAll)
	var refused *ingressdeploy.DisableRefusedError
	if !errors.As(err, &refused) || id != "" {
		r.t.Fatalf("Disable = %q, %v; want a *DisableRefusedError and no job", id, err)
	}
	if len(refused.Findings) != 1 || refused.Findings[0].Code != code {
		r.t.Errorf("Disable's findings = %+v, want %s", refused.Findings, code)
	}

	if st, err := r.istore.DeployState(ctx, r.envID); err != nil || !st.Enabled {
		r.t.Errorf("DeployState = %+v, %v; a refused switch-off must leave ingress on", st, err)
	}
	if c, n, v := r.hostHolds(); !c || !n || !v {
		r.t.Errorf("the host holds container %v, network %v, volume %v after a refusal; want all three", c, n, v)
	}
	if got := r.sys.downs; len(got) != 0 {
		r.t.Errorf("a refused switch-off took the project down: %v", got)
	}
	if tok, err := r.tokens.TokenStatus(ctx, r.envID); err != nil || !tok.Exists || tok.Disabled {
		r.t.Errorf("TokenStatus = %+v, %v; a refused switch-off must leave the token enabled", tok, err)
	}
	if rows, err := r.tokens.ListNodes(ctx, r.envID); err != nil || len(rows) != 1 {
		r.t.Errorf("ListNodes = %+v, %v; a refused switch-off must leave the node row", rows, err)
	}
	if p, err := r.istore.PendingWipes(ctx, r.envID); err != nil || len(p) != 0 {
		r.t.Errorf("PendingWipes = %+v, %v; a refused switch-off must queue no wipe", p, err)
	}
	for _, action := range []string{"ingress.disabled", "ingress.join_token_disabled", "ingress.node_removed", "ingress.wipe_queued"} {
		if got := r.auditCount(action); got != 0 {
			r.t.Errorf("%s audit rows = %d after a refusal, want 0", action, got)
		}
	}
	if len(r.kicker.calls) != 0 {
		r.t.Errorf("KickEnv calls = %v after a refusal, want none", r.kicker.calls)
	}
	release, held, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: stackops.HolderKindIngress})
	if !ok {
		r.t.Fatalf("the operation lock is still held after a refusal: %+v", held)
	}
	release()
	return prev.Findings[0]
}

// TestStandaloneDisableRefusedWhileRoutesLive: a running stack's claim and a stopped stack's each refuse a switch-off and its
// preview with ingress_routes_live, naming each route, and nothing is changed.
func TestStandaloneDisableRefusedWhileRoutesLive(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	fix := &disableFix{rig: r.rig}
	fix.seedRoute(r.envID, "stack-1", "web", "b.apps.example.com", "/", false)
	fix.seedRoute(r.envID, "stack-2", "batch", "a.apps.example.com", "/z", true)
	got := r.wantStandaloneRefused(ingressdeploy.CodeRoutesLive)
	want := []string{"a.apps.example.com/z (stack batch)", "b.apps.example.com/ (stack web)"}
	if !slices.Equal(got.Items, want) {
		t.Errorf("items = %q, want %q", got.Items, want)
	}
}

// TestStandaloneDisableRefusedWhenUnreachable: a host whose node is not connected refuses the preview as a finding and the
// switch-off as a refusal with ingress_host_unreachable; the switch stays on, the token is not disabled, the node row stays and
// no wipe is queued, because nothing can be removed from a host that cannot be reached (D-21).
func TestStandaloneDisableRefusedWhenUnreachable(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
	got := r.wantStandaloneRefused(ingressdeploy.CodeHostUnreachable)
	if got.Code != "ingress_host_unreachable" || !strings.Contains(got.Message, "Nothing was changed.") {
		t.Errorf("finding = %+v", got)
	}
}

// TestStandaloneDisableRefusedWhilePaused: a restore that has the host's ingress paused refuses the switch-off and writes
// nothing to the host, even with a route live and the host unreachable: the pause is reported alone and first (PROH-25-03).
func TestStandaloneDisableRefusedWhilePaused(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	if _, err := r.istore.SetPaused(testsupport.Context(t), r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	(&disableFix{rig: r.rig}).seedRoute(r.envID, "stack-1", "web", "b.apps.example.com", "/", false)
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
	r.wantStandaloneRefused(ingressdeploy.CodePaused)
}

// TestStandaloneDisableRefusalNamesBothBlockers: an unreachable host with a route live says both, the host first.
func TestStandaloneDisableRefusalNamesBothBlockers(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	(&disableFix{rig: r.rig}).seedRoute(r.envID, "stack-1", "web", "b.apps.example.com", "/", false)
	r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
	prev, err := r.svc.PreviewDisable(testsupport.Context(t), r.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	var codes []string
	for _, f := range prev.Findings {
		codes = append(codes, f.Code)
	}
	if want := []string{ingressdeploy.CodeHostUnreachable, ingressdeploy.CodeRoutesLive}; !slices.Equal(codes, want) {
		t.Errorf("finding codes = %v, want %v", codes, want)
	}
}

// foreignProjectContainer is a container somebody started into the proxy's compose project: no component label, and a service
// that is not the proxy.
func foreignProjectContainer() dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: "foreign-1", Names: []string{"/tierplan-ingress-sidecar-1"}, Image: "alpine", State: "running",
		Labels: map[string]string{stacks.LabelComposeProject: ingress.ProjectName, stacks.LabelComposeService: "sidecar"},
	}
}

// TestStandaloneDisableRefusedForAForeignContainerOfTheProject: the project's down removes every container of the project, so
// a container in it that is not the proxy refuses the preview and the switch-off with ingress_foreign_container, naming it,
// and the project is not taken down (WR-A3).
func TestStandaloneDisableRefusedForAForeignContainerOfTheProject(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	r.sys.mu.Lock()
	r.sys.base.Containers = append(r.sys.base.Containers, foreignProjectContainer())
	r.sys.reconfigure()
	r.sys.mu.Unlock()
	got := r.wantStandaloneRefused(ingressdeploy.CodeForeignContainer)
	if !slices.Equal(got.Items, []string{"tierplan-ingress-sidecar-1"}) || !strings.Contains(got.Message, "Nothing was changed.") {
		t.Errorf("finding = %+v, want the foreign container named", got)
	}
}

// TestStandaloneDisableProxyStepRefusesAContainerThatCameSinceTheCheck: a foreign container that joins the project after the
// check is refused by the proxy step before the down, which fails naming it; the project is not taken down.
func TestStandaloneDisableProxyStepRefusesAContainerThatCameSinceTheCheck(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	r.kicker.probe = func() string {
		r.sys.mu.Lock()
		defer r.sys.mu.Unlock()
		r.sys.base.Containers = append(r.sys.base.Containers, foreignProjectContainer())
		r.sys.reconfigure()
		return ""
	}
	job := r.disable()
	var proxy *jobs.Item
	for i := range job.Items {
		if job.Items[i].ID == ingressdeploy.ItemProxy {
			proxy = &job.Items[i]
		}
	}
	if job.State == jobs.JobDone || proxy == nil || proxy.State != jobs.ItemFailed || proxy.Code != ingressdeploy.CodeDisableProxyFailed {
		t.Fatalf("job state %q, proxy item %+v; want the proxy item failed with %s", job.State, proxy, ingressdeploy.CodeDisableProxyFailed)
	}
	if !strings.Contains(proxy.Detail, "tierplan-ingress-sidecar-1") {
		t.Errorf("proxy item detail = %q, want the foreign container named", proxy.Detail)
	}
	if got := r.sys.downs; len(got) != 0 {
		t.Errorf("the project was taken down with a foreign container in it: %v", got)
	}
	if c, _, _ := r.hostHolds(); !c {
		t.Error("the proxy container is gone")
	}
}

// networkKeeper plays a host whose network survives the project's down, as one a container that is not the proxy is attached
// to does (Pitfall 7): after the real down it puts the network back and, when attached is set, a user container on it.
type networkKeeper struct {
	ingressdeploy.SystemDeployer
	sys *fakeSystem
	// keep and attached are guarded by sys.mu.
	keep, attached bool
}

func (k *networkKeeper) set(keep, attached bool) {
	k.sys.mu.Lock()
	defer k.sys.mu.Unlock()
	k.keep, k.attached = keep, attached
}

func (k *networkKeeper) DownSystem(ctx context.Context, envID, nodeID string) error {
	if err := k.SystemDeployer.DownSystem(ctx, envID, nodeID); err != nil {
		return err
	}
	k.sys.mu.Lock()
	defer k.sys.mu.Unlock()
	if !k.keep {
		return nil
	}
	k.sys.host.NetworkSubnet = "10.0.8.0/24"
	k.sys.base.Containers = nil
	if k.attached {
		k.sys.base.Containers = []dockertest.FakeContainer{{
			ID: "user-1", Names: []string{"/web-1"}, Image: "nginx", State: "running",
			Networks: map[string]dockertest.FakeEndpoint{ingress.NetworkName: {NetworkID: "net-ingress"}},
		}}
	}
	k.sys.reconfigure()
	return nil
}

// TestStandaloneDisableReportsANetworkLeftBehind: a container that is not the proxy keeps the network after the down. The
// network item fails disable_network_failed naming it, the volume, token and node items are not run and nothing past the
// network was removed; once the container is gone the same call finishes what is left, the network included, and the volume is
// removed.
func TestStandaloneDisableReportsANetworkLeftBehind(t *testing.T) {
	var keeper *networkKeeper
	r := newStandaloneDisableRig(t, func(d *ingressdeploy.Deps) {
		sys, ok := d.System.(*fakeSystem)
		if !ok {
			t.Fatalf("the deployer is %T, want the fake", d.System)
		}
		keeper = &networkKeeper{SystemDeployer: d.System, sys: sys}
		d.System = keeper
	})
	ctx := testsupport.Context(t)
	keeper.set(true, true)

	job := r.disable()
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done with a network left behind: %+v", job.Items)
	}
	for _, id := range []string{ingressdeploy.ItemSwitchOff, ingressdeploy.ItemProxy} {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("%s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
	if ingressdeploy.CodeDisableNetworkFailed != "disable_network_failed" {
		t.Errorf("the network failure code = %q", ingressdeploy.CodeDisableNetworkFailed)
	}
	net := itemOf(job, ingressdeploy.ItemNetwork)
	if net.State != jobs.ItemFailed || net.Code != ingressdeploy.CodeDisableNetworkFailed {
		t.Fatalf("network item = %q code %q, want failed disable_network_failed", net.State, net.Code)
	}
	if !strings.Contains(net.Detail, "web-1") || !strings.Contains(net.Detail, "still attached to the ingress network") {
		t.Errorf("network detail = %q, want it to name the attached container", net.Detail)
	}
	for _, id := range []string{ingressdeploy.ItemVolumes, ingressdeploy.ItemToken, ingressdeploy.ItemNodes} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
			t.Errorf("%s = %q code %q, want skipped not_run", id, it.State, it.Code)
		}
	}
	if _, n, v := r.hostHolds(); !n || !v {
		t.Errorf("the host holds network %v, volume %v after the failed job; want both left for the rerun", n, v)
	}
	if tok, err := r.tokens.TokenStatus(ctx, r.envID); err != nil || tok.Disabled {
		t.Errorf("TokenStatus = %+v, %v; the token must not be disabled by a job that stopped before it", tok, err)
	}
	if rows, err := r.tokens.ListNodes(ctx, r.envID); err != nil || len(rows) != 1 {
		t.Errorf("ListNodes = %+v, %v; the node row must stay", rows, err)
	}
	if st, err := r.istore.DeployState(ctx, r.envID); err != nil || st.Enabled {
		t.Errorf("DeployState = %+v, %v; the switch is off, the job's first step having run", st, err)
	}
	if holder, held := r.locks.Held(r.envID); held {
		t.Errorf("the operation lock is still held after a failed job: %+v", holder)
	}

	// The container is gone, the network is not: the rerun removes it itself and finishes the rest.
	keeper.set(true, false)
	job = r.disable()
	wantStandaloneDisableDone(t, job)
	if it := itemOf(job, ingressdeploy.ItemNetwork); it.Result["removed"] != "true" {
		t.Errorf("network result = %v, want the network removed by the rerun", it.Result)
	}
	if c, n, v := r.hostHolds(); c || n || v {
		t.Errorf("after the rerun the host holds container %v, network %v, volume %v; want none", c, n, v)
	}
	// The first job's switch-off row is the one success row; its failure at the network is the other row (A-01 class F).
	if got := r.auditCountResult("ingress.disabled", audit.ResultSuccess); got != 1 {
		t.Errorf("ingress.disabled success rows = %d after the rerun, want 1", got)
	}
	for action, want := range map[string]int{"ingress.disabled": 2, "ingress.join_token_disabled": 1, "ingress.node_removed": 1} {
		if got := r.auditCount(action); got != want {
			t.Errorf("%s audit rows = %d after the rerun, want %d", action, got, want)
		}
	}
}

// TestStandaloneDisableLeavesAVolumeAContainerStillMounts: the cache volume is never removed under a container that mounts
// it; the volumes item fails naming the container and the token and node items are not run, so the volume is never left
// behind while the job says it is done (PROH-25-06).
func TestStandaloneDisableLeavesAVolumeAContainerStillMounts(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	r.sys.mu.Lock()
	r.sys.base.Containers = []dockertest.FakeContainer{{
		ID: "keys-1", Names: []string{"/keys-1"}, Image: "busybox", State: "exited", Mounts: []string{ingress.VolumeName},
	}}
	r.sys.reconfigure()
	r.sys.mu.Unlock()

	job := r.disable()
	vol := itemOf(job, ingressdeploy.ItemVolumes)
	if job.State == jobs.JobDone || vol.State != jobs.ItemFailed || vol.Code != ingressdeploy.CodeDisableVolumesFailed {
		t.Fatalf("job %q, volumes item %q code %q; want a failed disable_volumes_failed", job.State, vol.State, vol.Code)
	}
	if !strings.Contains(vol.Detail, "keys-1") {
		t.Errorf("volumes detail = %q, want it to name the container", vol.Detail)
	}
	if _, _, v := r.hostHolds(); !v {
		t.Error("the volume was removed under a container that mounts it")
	}
	for _, id := range []string{ingressdeploy.ItemToken, ingressdeploy.ItemNodes} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped {
			t.Errorf("%s = %q, want skipped", id, it.State)
		}
	}
}

// brokenEngine is an engine that is up but fails every call, as a host whose read goes wrong does.
type brokenEngine struct{ srv *httptest.Server }

func newBrokenEngine(t *testing.T) *brokenEngine {
	t.Helper()
	version := regexp.MustCompile(`^/v[0-9.]+`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.52")
		w.Header().Set("Content-Type", "application/json")
		if version.ReplaceAllString(r.URL.Path, "") == "/_ping" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"read broke"}`))
	}))
	t.Cleanup(srv.Close)
	return &brokenEngine{srv: srv}
}

func (b *brokenEngine) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", b.srv.Listener.Addr().String())
}

func (b *brokenEngine) Describe() string { return "tcp://" + b.srv.Listener.Addr().String() }

// TestStandaloneDisablePreviewNamesTheContainerComposeMakes: a host that cannot be read still gets a preview, which names
// the container as Compose names it, with its -1 (IN-B4).
func TestStandaloneDisablePreviewNamesTheContainerComposeMakes(t *testing.T) {
	r := newStandaloneDisableRig(t, nil)
	cli, err := dockerx.New(newBrokenEngine(t))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	r.eng.cli = cli
	prev, err := r.svc.PreviewDisable(testsupport.Context(t), r.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if want := ingress.ProjectName + "-" + ingress.ProxyComposeService + "-1"; prev.Removes.Container != want {
		t.Errorf("removes.container = %q, want %q", prev.Removes.Container, want)
	}
}
