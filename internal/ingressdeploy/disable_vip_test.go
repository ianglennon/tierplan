// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// vipIngressFix is a swarm whose ingress an enable has switched on and whose virtual IP is on, over the clock that moves only
// when the deployer waits.
func newVIPIngressFix(t *testing.T) *vipDisableFix {
	t.Helper()
	f := newVIPDisableFix(t, vipDisableOpts{})
	wantAllDone(t, f.enable(ingressdeploy.EnableRequest{}))
	f.enabled()
	return f
}

// disableIngress starts the ingress switch-off and waits for its job to end. The job is returned whatever its outcome.
func (f *vipDisableFix) disableIngress() jobs.Job {
	f.t.Helper()
	id, err := f.svc.Disable(testsupport.Context(f.t), testActor, testOwner, f.envID, seesAll)
	if err != nil {
		f.t.Fatalf("Disable: %v", err)
	}
	return f.wait(id)
}

// writeIndex is the place of the first write that starts with prefix and mentions id, -1 when there is none.
func writeIndex(writes []string, prefix, id string) int {
	return slices.IndexFunc(writes, func(w string) bool { return strings.HasPrefix(w, prefix) && strings.Contains(w, id) })
}

// TestDisableIngressRemovesVIPFirst is the tracer of D-17: switching ingress off on a swarm whose virtual IP is on runs a
// first item, virtual_ip, that removes the keepalived service, its secret and its row before the proxy service is touched, and
// the preview names the address and the secret.
func TestDisableIngressRemovesVIPFirst(t *testing.T) {
	f := newVIPIngressFix(t)
	ctx := testsupport.Context(t)
	secret := ingressdeploy.KeepalivedSecretName(f.envID, 1)
	keepalived, ok := f.keepalivedService()
	if !ok {
		t.Fatal("the swarm holds no keepalived service")
	}
	proxy, ok := f.service()
	if !ok {
		t.Fatal("the swarm holds no proxy service")
	}

	pv, err := f.svc.PreviewDisable(ctx, f.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if v := pv.Removes.VirtualIP; v == nil || v.Address.String() != vipTestAddress || v.SecretName != secret {
		t.Fatalf("preview removes the virtual IP %+v, want %s and %s", v, vipTestAddress, secret)
	}
	before := len(f.fake.Writes())

	job := f.disableIngress()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	if len(job.Items) == 0 || job.Items[0].ID != "virtual_ip" {
		t.Fatalf("the first item = %+v, want virtual_ip", job.Items)
	}
	if it := itemOf(job, ingressdeploy.ItemVirtualIP); it.State != jobs.ItemDone || it.Result["removed"] != "true" {
		t.Errorf("virtual_ip = %q %v, want done and removed", it.State, it.Result)
	}

	// The keepalived service went, and with it the secret, before the proxy service was removed.
	writes := f.fake.Writes()[before:]
	vipGone, secretGone, proxyGone := writeIndex(writes, "DELETE /services/", keepalived.ID), writeIndex(writes, "DELETE /secrets/", ""), writeIndex(writes, "DELETE /services/", proxy.ID)
	if vipGone < 0 || secretGone < 0 || proxyGone < 0 || vipGone >= proxyGone || secretGone >= proxyGone {
		t.Errorf("writes = %v, want the keepalived service and its secret removed before the proxy service", writes)
	}
	if _, ok := f.keepalivedService(); ok {
		t.Error("the keepalived service is still in the swarm")
	}
	if got := f.keepalivedSecrets(); len(got) != 0 {
		t.Errorf("key secrets left = %v, want none", got)
	}
	if _, err := f.istore.GetVIP(ctx, f.envID); !errors.Is(err, ingressstore.ErrVIPNotFound) {
		t.Errorf("GetVIP after the switch-off = %v, want ErrVIPNotFound", err)
	}
	if n := f.auditCount("ingress.vip_disabled"); n != 1 {
		t.Errorf("ingress.vip_disabled audit rows = %d, want 1", n)
	}
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || st.Enabled {
		t.Errorf("ingress state = %+v, %v, want switched off", st, err)
	}
	if _, ok := f.service(); ok {
		t.Error("the proxy service is still in the swarm")
	}
}

// TestDisableIngressStopsWhenTheVIPStays: a virtual IP whose keepalived tasks do not end ends the virtual_ip item with
// vip_remove_failed and every later item not run, so the proxies are never removed under a running keepalived and ingress
// stays on; the row stays, switched off, for the next try (PROH-26-06).
func TestDisableIngressStopsWhenTheVIPStays(t *testing.T) {
	f := newVIPIngressFix(t)
	ctx := testsupport.Context(t)
	cfg := f.base
	cfg.Tasks = append(cfg.Tasks, vipKeepalivedTask())
	f.fake.Reconfigure(cfg)

	job := f.disableIngress()
	if job.State == jobs.JobDone {
		t.Fatalf("the job ended done with a keepalived task still running: %+v", job.Items)
	}
	if it := itemOf(job, ingressdeploy.ItemVirtualIP); it.State != jobs.ItemFailed || it.Code != ingressdeploy.CodeVIPRemoveFailed {
		t.Errorf("virtual_ip = %q %q, want failed with vip_remove_failed", it.State, it.Code)
	}
	for _, id := range []string{"switch_off", "service", "volumes", "network", "secret", "token", "nodes"} {
		if it := itemOf(job, id); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
			t.Errorf("%s = %q %q, want skipped and not_run", id, it.State, it.Code)
		}
	}
	if _, ok := f.service(); !ok {
		t.Error("the proxy service was removed under a running keepalived")
	}
	if st, err := f.istore.DeployState(ctx, f.envID); err != nil || !st.Enabled {
		t.Errorf("ingress state = %+v, %v, want still on", st, err)
	}
	row, err := f.istore.GetVIP(ctx, f.envID)
	if err != nil || row.Enabled {
		t.Errorf("the virtual IP row = %+v, %v, want kept and switched off", row, err)
	}
}

// TestDisableIngressWithoutAVIPFinishesTheVIPItemUnchanged: a swarm with no virtual IP lists virtual_ip first, ends it done
// with nothing removed, and writes nothing for it.
func TestDisableIngressWithoutAVIPFinishesTheVIPItemUnchanged(t *testing.T) {
	f := newDisableFix(t, disableOpts{})
	pv, err := f.svc.PreviewDisable(testsupport.Context(t), f.envID, seesAll)
	if err != nil {
		t.Fatalf("PreviewDisable: %v", err)
	}
	if pv.Removes.VirtualIP != nil {
		t.Errorf("preview removes the virtual IP %+v, want none", pv.Removes.VirtualIP)
	}
	job := f.disable(seesAll)
	wantDisableDone(t, job)
	if it := itemOf(job, ingressdeploy.ItemVirtualIP); it.Result["removed"] != "false" {
		t.Errorf("virtual_ip result = %v, want nothing removed", it.Result)
	}
}
