// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The virtual IP write routes (Phase 26, VIP-01, VIP-05, D-16). The deployer is the real one over a fake swarm of two managers
// on 10.9.0.0/24 whose ingress is on; the jobs run through the real hub.

// vipWriteAddress is an address inside the network of the fixture's two nodes.
const vipWriteAddress = "10.9.0.50/24"

// vipWriteFix is the deployer fixture of the write routes.
type vipWriteFix struct {
	*deployFix
	managers *vipManagers
}

// newVIPWriteFix builds the fixture: a swarm whose two managers both allow privileged services and whose ingress is on. config
// adjusts the fake swarm.
func newVIPWriteFix(t *testing.T, config func(*dockertest.FakeConfig)) *vipWriteFix {
	t.Helper()
	v := &vipWriteFix{managers: &vipManagers{rows: []envs.ManagerPolicyRow{
		{NodeID: vipNodeA, Hostname: "manager-1", State: envs.ManagerAllows},
		{NodeID: vipNodeB, Hostname: "manager-2", State: envs.ManagerAllows},
	}}}
	v.deployFix = newDeployFixDeps(t, false, func(c *dockertest.FakeConfig) {
		c.NodeRows = []dockertest.FakeNode{
			{ID: vipNodeA, Hostname: "manager-1", Role: "manager", Leader: true, Address: "10.9.0.11"},
			{ID: vipNodeB, Hostname: "manager-2", Role: "manager", Address: "10.9.0.12"},
		}
		if config != nil {
			config(c)
		}
	}, func(_ *deployFix, d *ingressdeploy.Deps) {
		d.Envs = vipEnvs{}
		d.Managers = v.managers
	})
	if _, err := v.ingressStore.SetEnabled(testsupport.Context(t), v.envA, true, "u-admin", nil); err != nil {
		t.Fatalf("switch ingress on: %v", err)
	}
	return v
}

// vipBody is the JSON body of a request for an address.
func vipBody(address string) string { return `{"address":"` + address + `"}` }

// vipPreviewResp is the shape of the preview answer.
type vipPreviewResp struct {
	Findings []struct {
		Code     string   `json:"code"`
		Message  string   `json:"message"`
		Severity string   `json:"severity"`
		Items    []string `json:"items"`
	} `json:"findings"`
	Address         string `json:"address"`
	RouterID        int    `json:"routerId"`
	DerivedRouterID int    `json:"derivedRouterId"`
	RouterDerived   bool   `json:"routerDerived"`
	Managers        []struct {
		NodeID, Hostname, State, Note string
		LocalSocket                   bool
	} `json:"managers"`
	Warning         string   `json:"warning"`
	Change          bool     `json:"change"`
	Items           []string `json:"items"`
	Current         string   `json:"current"`
	CurrentRouterID int      `json:"currentRouterId"`
	Strategy        string   `json:"strategy"`
	Notice          string   `json:"notice"`
}

// wantNoVIPKey fails the test when a body carries the key the environment's virtual IP row holds, in the hex form the swarm
// secret has, or its sealed form (PROH-26-03).
func (v *vipWriteFix) wantNoVIPKey(t *testing.T, what, body string) {
	t.Helper()
	row, err := v.ingressStore.GetVIP(testsupport.Context(t), v.envA)
	if err != nil {
		return
	}
	raw, err := v.ingressStore.OpenVIPKey(v.envA, row.KeySealed)
	if err != nil {
		t.Fatalf("open the stored key: %v", err)
	}
	if h := hex.EncodeToString(raw); strings.Contains(strings.ToLower(body), h) {
		t.Errorf("%s carries the key: %s", what, body)
	}
	if row.KeySealed != "" && strings.Contains(body, row.KeySealed) {
		t.Errorf("%s carries the sealed key: %s", what, body)
	}
}

// startAndWait posts, puts or deletes to a virtual IP route that answers 202, waits for its job to end and returns the job as the
// ingress job route reads it.
func (v *vipWriteFix) startAndWait(t *testing.T, method, path, body string) jobResp {
	t.Helper()
	rec := v.credCall(t, v.adminA, method, path, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%s %s: %d %s, want 202", method, path, rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s %s Cache-Control = %q, want no-store", method, path, cc)
	}
	v.wantNoVIPKey(t, method+" "+path, rec.Body.String())
	start := decodeBody[struct {
		JobID string `json:"jobId"`
	}](t, rec)
	if start.JobID == "" {
		t.Fatalf("%s %s answered no job: %s", method, path, rec.Body)
	}
	if _, err := v.hub.Wait(testsupport.Context(t), start.JobID); err != nil {
		t.Fatalf("wait for the job: %v", err)
	}
	rec = v.credCall(t, v.adminA, http.MethodGet, "/api/env/"+v.envA+"/ingress/jobs/"+start.JobID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("read the job: %d %s", rec.Code, rec.Body)
	}
	v.wantNoVIPKey(t, "the job", rec.Body.String())
	return decodeBody[jobResp](t, rec)
}

// TestEnableIngressVIPRoute is the tracer (VIP-01, D-16): an Admin previews a virtual IP and reads its findings, the manager table
// and the fixed warning, and nothing is written; then switches it on, follows the job to done and finds the secret, the
// keepalived service and the status. No answer carries the key.
func TestEnableIngressVIPRoute(t *testing.T) {
	t.Parallel()
	v := newVIPWriteFix(t, nil)
	base := "/api/env/" + v.envA + "/ingress/vip"

	rec := v.credCall(t, v.adminA, http.MethodPost, base+"/preview", vipBody(vipWriteAddress))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[vipPreviewResp](t, rec)
	var unverified bool
	for _, f := range pv.Findings {
		if f.Severity == "error" {
			t.Errorf("the preview refuses: %+v", f)
		}
		unverified = unverified || f.Code == ingressdeploy.CodeVIPUnverified
		if f.Message == "" || f.Items == nil {
			t.Errorf("finding %+v must carry its sentence and an items list that is never null", f)
		}
	}
	derived := ingressdeploy.DerivedRouterID("cluster-1")
	if !unverified || pv.Address != vipWriteAddress || pv.RouterID != derived || pv.DerivedRouterID != derived || !pv.RouterDerived ||
		pv.Change || pv.Warning != ingressdeploy.VIPPrivilegeWarning || len(pv.Managers) != 2 || len(pv.Items) != 4 {
		t.Errorf("preview = %+v, want the address, the derived router ID %d, two managers, the fixed warning, four items and no change", pv, derived)
	}
	if n := len(v.fake.ServiceCreates()) + len(v.fake.SecretCreates()); n != 0 {
		t.Fatalf("the preview wrote %d objects", n)
	}
	if rec := v.credCall(t, v.adminA, http.MethodGet, base, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status after the preview: %d %s, want 404 (nothing was stored)", rec.Code, rec.Body)
	}

	job := v.startAndWait(t, http.MethodPost, base, vipBody(vipWriteAddress))
	if job.Kind != ingressdeploy.JobKindVIPEnable || job.State != jobs.JobDone || len(job.Items) != 4 {
		t.Fatalf("job = %+v, want a done %s with four items", job, ingressdeploy.JobKindVIPEnable)
	}
	for _, it := range job.Items {
		if it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", it.ID, it.State, it.Code)
		}
	}

	// The objects are on the fake swarm: the key as a swarm secret and the global keepalived service.
	if creates := v.fake.SecretCreates(); len(creates) != 1 || creates[0].Name != ingressdeploy.KeepalivedSecretName(v.envA, 1) {
		t.Errorf("secret creates = %+v, want the key secret of generation 1", creates)
	}
	if _, _, err := v.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceNameKeepalived); err != nil {
		t.Errorf("the keepalived service is not on the swarm: %v", err)
	}
	rec = v.credCall(t, v.adminA, http.MethodGet, base, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	v.wantNoVIPKey(t, "the status", rec.Body.String())
	if st := decodeBody[vipStatusResp](t, rec); st.Address != vipWriteAddress || st.RouterID != derived || !st.Enabled || st.KeyGeneration != 1 {
		t.Errorf("status = %+v", st)
	}
}
