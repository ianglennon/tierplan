// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
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

// vipProblem is the shape of a refusal.
type vipProblem struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
	Errors []struct {
		Location string   `json:"location"`
		Message  string   `json:"message"`
		Value    []string `json:"value"`
	} `json:"errors"`
}

func decodeVIPProblem(t *testing.T, body []byte) vipProblem {
	t.Helper()
	var p vipProblem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	return p
}

// wantNothingWritten fails the test when the fixture's swarm, hub or lock shows that a refused request did anything.
func (v *vipWriteFix) wantNothingWritten(t *testing.T) {
	t.Helper()
	if n := len(v.fake.ServiceCreates()) + len(v.fake.ServiceUpdates()) + len(v.fake.SecretCreates()); n != 0 {
		t.Errorf("a refused request caused %d writes", n)
	}
	if list := v.hub.List(func(jobs.Job) bool { return true }); len(list) != 0 {
		t.Errorf("a refused request started %d jobs", len(list))
	}
	if _, held := v.stackLocks.Held(v.envA); held {
		t.Error("a refused request left the environment's lock held")
	}
}

// TestIngressVIPRefusals: each refusal of the enable is 409 with its stable code as the type, the server's sentence as the
// detail and the finding in errors; an address that is not an address with its prefix length is 422 vip_address_invalid; a held
// lock is 409 operation_locked naming the holder. The preview of the same request answers 200 with the finding, and nothing is
// written or started (VIP-05, D-16, D-19).
func TestIngressVIPRefusals(t *testing.T) {
	t.Parallel()
	other := ingressstore.VIPConfig{
		EnvID: "env-other", Address: netip.MustParseAddr("10.9.0.77"), PrefixLen: 24, RouterID: 77, KeySealed: vipKeySentinel, KeyGeneration: 1, Enabled: true,
	}
	for _, tc := range []struct {
		name   string
		code   string
		status int
		body   string
		// setup adjusts the fixture; standalone builds the fixture of a standalone host instead.
		setup      func(t *testing.T, v *vipWriteFix)
		config     func(*dockertest.FakeConfig)
		standalone bool
		// previewIsChange is set when the environment has a virtual IP, so that the preview of the request is of a change.
		previewIsChange bool
		// detail is what the detail contains, and item what some finding's items contain.
		detail string
		item   string
	}{
		{name: "standalone", code: "vip_swarm_only", status: 409, body: vipBody(vipWriteAddress), standalone: true,
			detail: "A virtual IP is for swarms. A single host's own address already reaches its proxy."},
		{name: "ingress off", code: "vip_ingress_off", status: 409, body: vipBody(vipWriteAddress),
			setup: func(t *testing.T, v *vipWriteFix) {
				if _, err := v.ingressStore.SetEnabled(testsupport.Context(t), v.envA, false, "u-admin", nil); err != nil {
					t.Fatalf("switch ingress off: %v", err)
				}
			},
			detail: "Switch ingress on for this environment first. The virtual IP points at the proxies."},
		{name: "a manager refuses", code: "vip_privileged_not_allowed", status: 409, body: vipBody(vipWriteAddress),
			setup: func(_ *testing.T, v *vipWriteFix) {
				v.managers.rows[1].State = envs.ManagerRefuses
			},
			detail: "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true", item: "manager-2: refuses"},
		{name: "a manager has no agent", code: "vip_privileged_not_allowed", status: 409, body: vipBody(vipWriteAddress),
			setup: func(_ *testing.T, v *vipWriteFix) {
				v.managers.rows[0].State = envs.ManagerUnknown
			},
			detail: "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true", item: "manager-1: unknown"},
		{name: "outside the nodes' network", code: "vip_outside_node_subnet", status: 409, body: vipBody("192.168.5.50/24"),
			detail: "192.168.5.50 isn't inside the network of every node. These nodes are outside it: manager-1, manager-2."},
		{name: "a node's own address", code: "vip_is_node_address", status: 409, body: vipBody("10.9.0.11/24"),
			detail: "10.9.0.11 is already the address of manager-1.", item: "manager-1"},
		{name: "network address", code: "vip_is_network_or_broadcast", status: 409, body: vipBody("10.9.0.0/24"),
			detail: "10.9.0.0 is the network or broadcast address of 10.9.0.0/24. Choose another."},
		{name: "not unicast", code: "vip_not_unicast", status: 409, body: vipBody("127.0.0.1/8"),
			detail: "127.0.0.1 can't be a virtual IP."},
		{name: "IPv6", code: "vip_ipv6_unsupported", status: 409, body: vipBody("fd00::50/64"),
			detail: "IPv6 virtual addresses aren't supported yet. Use an IPv4 address."},
		{name: "router ID in use", code: "vip_router_id_in_use", status: 409, body: `{"address":"` + vipWriteAddress + `","routerId":77}`,
			setup: func(t *testing.T, v *vipWriteFix) {
				cfg := other
				cfg.EnvID = v.envB
				if err := v.ingressStore.PutVIP(testsupport.Context(t), cfg); err != nil {
					t.Fatalf("PutVIP of the other environment: %v", err)
				}
			},
			detail: "already uses router ID 77 on an overlapping network. Set a different router ID."},
		{name: "address of another environment", code: "vip_in_use_by_environment", status: 409, body: vipBody("10.9.0.77/24"),
			setup: func(t *testing.T, v *vipWriteFix) {
				cfg := other
				cfg.EnvID = v.envB
				if err := v.ingressStore.PutVIP(testsupport.Context(t), cfg); err != nil {
					t.Fatalf("PutVIP of the other environment: %v", err)
				}
			},
			detail: "10.9.0.77 is already the virtual IP of"},
		{name: "overlaps a Docker network", code: "vip_overlaps_docker", status: 409, body: vipBody(vipWriteAddress),
			config: func(c *dockertest.FakeConfig) {
				c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "app-net", Name: "appnet", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.9.0.0/24"}})
			},
			detail: "10.9.0.0/24 overlaps the network appnet on this swarm."},
		{name: "already enabled", code: "vip_already_enabled", status: 409, body: vipBody(vipWriteAddress),
			setup: func(t *testing.T, v *vipWriteFix) {
				cfg := other
				cfg.EnvID = v.envA
				if err := v.ingressStore.PutVIP(testsupport.Context(t), cfg); err != nil {
					t.Fatalf("PutVIP: %v", err)
				}
			},
			detail: "This environment already has a virtual IP. Change it or switch it off instead of enabling it again.", previewIsChange: true},
		{name: "address without a prefix", code: "vip_address_invalid", status: 422, body: vipBody("192.168.1.50"),
			detail: "Give the address with its prefix length, for example 192.168.1.50/24."},
		{name: "address that is words", code: "vip_address_invalid", status: 422, body: vipBody("not an address"),
			detail: "Give the address with its prefix length, for example 192.168.1.50/24."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var v *vipWriteFix
			if tc.standalone {
				v = &vipWriteFix{deployFix: newDeployFix(t, true)}
			} else {
				v = newVIPWriteFix(t, tc.config)
			}
			if tc.setup != nil {
				tc.setup(t, v)
			}
			base := "/api/env/" + v.envA + "/ingress/vip"

			rec := v.credCall(t, v.adminA, http.MethodPost, base, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("enable: %d %s, want %d", rec.Code, rec.Body, tc.status)
			}
			p := decodeVIPProblem(t, rec.Body.Bytes())
			if p.Type != tc.code || !strings.Contains(p.Detail, tc.detail) {
				t.Errorf("problem type %q detail %q, want %q containing %q", p.Type, p.Detail, tc.code, tc.detail)
			}
			if len(p.Errors) == 0 || p.Errors[0].Location != tc.code || p.Errors[0].Message == "" || p.Errors[0].Value == nil {
				t.Errorf("errors = %+v, want the finding %s with its sentence and an items list", p.Errors, tc.code)
			}
			if tc.item != "" && (len(p.Errors) == 0 || !slices.ContainsFunc(p.Errors[0].Value, func(it string) bool { return strings.Contains(it, tc.item) })) {
				t.Errorf("errors = %+v, want an item containing %q", p.Errors, tc.item)
			}
			v.wantNothingWritten(t)

			if tc.previewIsChange {
				return
			}
			// The preview of the same request answers 200 and carries the same finding as an error.
			rec = v.credCall(t, v.adminA, http.MethodPost, base+"/preview", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("preview: %d %s, want 200", rec.Code, rec.Body)
			}
			var got []string
			for _, f := range decodeBody[vipPreviewResp](t, rec).Findings {
				if f.Severity == "error" {
					got = append(got, f.Code)
				}
			}
			if !slices.Contains(got, tc.code) {
				t.Errorf("preview error findings = %v, want %s among them", got, tc.code)
			}
			v.wantNothingWritten(t)
		})
	}

	t.Run("a held lock", func(t *testing.T) {
		t.Parallel()
		v := newVIPWriteFix(t, nil)
		release, _, ok := v.stackLocks.TryAcquire(v.envA, stackops.Holder{
			Kind: stackops.HolderKindIngress, JobID: "job-2", Workload: "ingress", By: "ada", Started: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
		})
		if !ok {
			t.Fatal("the lock was not free")
		}
		t.Cleanup(release)
		rec := v.credCall(t, v.adminA, http.MethodPost, "/api/env/"+v.envA+"/ingress/vip", vipBody(vipWriteAddress))
		p := decodeVIPProblem(t, rec.Body.Bytes())
		if rec.Code != http.StatusConflict || p.Type != "operation_locked" || !strings.HasPrefix(p.Detail, "TierPlan is changing ingress on ") {
			t.Fatalf("enable under a held lock: %d %s, want 409 operation_locked naming who holds it", rec.Code, rec.Body)
		}
		var got []string
		for _, e := range p.Errors {
			got = append(got, e.Location+"="+e.Message)
		}
		for _, want := range []string{"kind=ingress", "job=job-2", "started=2026-01-01T09:00:00Z"} {
			if !slices.Contains(got, want) {
				t.Errorf("errors = %v, want %s", got, want)
			}
		}
		if n := len(v.fake.ServiceCreates()) + len(v.fake.SecretCreates()); n != 0 {
			t.Errorf("a locked enable wrote %d objects", n)
		}
		if rec := v.credCall(t, v.adminA, http.MethodPost, "/api/env/"+v.envA+"/ingress/vip/preview", vipBody(vipWriteAddress)); rec.Code != http.StatusOK {
			t.Errorf("preview under the lock: %d %s, want 200", rec.Code, rec.Body)
		}
	})

	t.Run("a body huma refuses", func(t *testing.T) {
		t.Parallel()
		v := newVIPWriteFix(t, nil)
		base := "/api/env/" + v.envA + "/ingress/vip"
		for _, body := range []string{
			`{}`, `{"address":"255.255.255.255/32x"}`, `{"address":"` + vipWriteAddress + `","routerId":0}`,
			`{"address":"` + vipWriteAddress + `","routerId":256}`, `{"address":"` + vipWriteAddress + `","routerId":"seven"}`, `not json`,
		} {
			for _, path := range []string{base, base + "/preview"} {
				if rec := v.credCall(t, v.adminA, http.MethodPost, path, body); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusBadRequest {
					t.Errorf("POST %s %s: %d %s, want 422", path, body, rec.Code, rec.Body)
				}
			}
		}
		v.wantNothingWritten(t)
	})
}

// TestChangeAndDisableIngressVIPRoutes: with a virtual IP on, the preview is of a change (its strategy, its notice and the
// current address), PUT changes the address through a job, previewIngressVIPDisable lists the service and the secret, and
// DELETE runs the switch-off job. With no virtual IP, change and disable and the switch-off preview answer 404 vip_not_enabled
// (VIP-01, D-16).
func TestChangeAndDisableIngressVIPRoutes(t *testing.T) {
	t.Parallel()
	v := newVIPWriteFix(t, nil)
	base := "/api/env/" + v.envA + "/ingress/vip"

	// With no virtual IP, a change and a switch-off are refused, and nothing is written.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, base, vipBody(vipWriteAddress)},
		{http.MethodDelete, base, ""},
		{http.MethodPost, base + "/disable/preview", ""},
	} {
		rec := v.credCall(t, v.adminA, c.method, c.path, c.body)
		if p := decodeVIPProblem(t, rec.Body.Bytes()); rec.Code != http.StatusNotFound || p.Type != "vip_not_enabled" || p.Detail != "This environment has no virtual IP." {
			t.Errorf("%s %s with no virtual IP: %d %s, want 404 vip_not_enabled", c.method, c.path, rec.Code, rec.Body)
		}
	}
	v.wantNothingWritten(t)

	if job := v.startAndWait(t, http.MethodPost, base, vipBody(vipWriteAddress)); job.State != jobs.JobDone {
		t.Fatalf("enable job = %+v, want done", job)
	}
	derived := ingressdeploy.DerivedRouterID("cluster-1")

	// The preview of a request is now the preview of a change: a new address rolls, a new router ID replaces.
	rec := v.credCall(t, v.adminA, http.MethodPost, base+"/preview", vipBody("10.9.0.60/24"))
	if rec.Code != http.StatusOK {
		t.Fatalf("change preview: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("change preview Cache-Control = %q, want no-store", cc)
	}
	pv := decodeBody[vipPreviewResp](t, rec)
	if !pv.Change || pv.Current != vipWriteAddress || pv.CurrentRouterID != derived || pv.Address != "10.9.0.60/24" || pv.Strategy != "roll" || pv.Notice == "" ||
		len(pv.Items) != 2 || pv.Warning != ingressdeploy.VIPPrivilegeWarning {
		t.Errorf("change preview = %+v, want a roll from %s with the notice, two items and the warning", pv, vipWriteAddress)
	}
	rec = v.credCall(t, v.adminA, http.MethodPost, base+"/preview", `{"address":"`+vipWriteAddress+`","routerId":77}`)
	if pv := decodeBody[vipPreviewResp](t, rec); !pv.Change || pv.Strategy != "replace" || pv.RouterDerived || pv.RouterID != 77 {
		t.Errorf("router ID change preview = %+v, want a replace with router ID 77 chosen", pv)
	}
	// Enabling again is refused, and the change's refusals are the same codes.
	rec = v.credCall(t, v.adminA, http.MethodPost, base, vipBody("10.9.0.60/24"))
	if p := decodeVIPProblem(t, rec.Body.Bytes()); rec.Code != http.StatusConflict || p.Type != "vip_already_enabled" {
		t.Errorf("enable again: %d %s, want 409 vip_already_enabled", rec.Code, rec.Body)
	}
	for body, code := range map[string]string{vipBody("10.9.0.11/24"): "vip_is_node_address", vipBody("192.168.1.50"): "vip_address_invalid"} {
		rec = v.credCall(t, v.adminA, http.MethodPut, base, body)
		if p := decodeVIPProblem(t, rec.Body.Bytes()); p.Type != code || (rec.Code != http.StatusConflict && rec.Code != http.StatusUnprocessableEntity) {
			t.Errorf("PUT %s: %d %s, want %s", body, rec.Code, rec.Body, code)
		}
	}
	if n := len(v.fake.ServiceUpdates()); n != 0 {
		t.Fatalf("a refused or previewed change updated the service %d times", n)
	}

	// PUT changes the address through a job.
	job := v.startAndWait(t, http.MethodPut, base, vipBody("10.9.0.60/24"))
	if job.Kind != ingressdeploy.JobKindVIPChange || job.State != jobs.JobDone || len(job.Items) != 2 {
		t.Fatalf("change job = %+v, want a done %s with two items", job, ingressdeploy.JobKindVIPChange)
	}
	if n := len(v.fake.ServiceUpdates()); n != 1 {
		t.Errorf("service updates = %d, want 1", n)
	}
	rec = v.credCall(t, v.adminA, http.MethodGet, base, "")
	if st := decodeBody[vipStatusResp](t, rec); rec.Code != http.StatusOK || st.Address != "10.9.0.60/24" || st.RouterID != derived || st.KeyGeneration != 1 {
		t.Errorf("status after the change: %d %+v, want 10.9.0.60/24 with the same router ID and key", rec.Code, st)
	}

	// The preview of the switch-off lists the service and the key secret and nothing else of the key.
	rec = v.credCall(t, v.adminA, http.MethodPost, base+"/disable/preview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable preview: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("disable preview Cache-Control = %q, want no-store", cc)
	}
	v.wantNoVIPKey(t, "the disable preview", rec.Body.String())
	dp := decodeBody[struct {
		Address string   `json:"address"`
		Service string   `json:"service"`
		Tasks   int      `json:"tasks"`
		Secrets []string `json:"secrets"`
		Items   []string `json:"items"`
	}](t, rec)
	if dp.Address != "10.9.0.60/24" || dp.Service != ingress.ServiceNameKeepalived || len(dp.Secrets) != 1 || dp.Secrets[0] != ingressdeploy.KeepalivedSecretName(v.envA, 1) || len(dp.Items) != 4 {
		t.Errorf("disable preview = %+v, want the address, the keepalived service, the key secret and four items", dp)
	}

	// DELETE runs the switch-off job, after which the environment has no virtual IP and the service and secret are gone.
	job = v.startAndWait(t, http.MethodDelete, base, "")
	if job.Kind != ingressdeploy.JobKindVIPDisable || job.State != jobs.JobDone || len(job.Items) != 4 {
		t.Fatalf("disable job = %+v, want a done %s with four items", job, ingressdeploy.JobKindVIPDisable)
	}
	if rec := v.credCall(t, v.adminA, http.MethodGet, base, ""); rec.Code != http.StatusNotFound {
		t.Errorf("status after the switch-off: %d %s, want 404", rec.Code, rec.Body)
	}
	if _, _, err := v.cli.ServiceInspect(testsupport.Context(t), ingress.ServiceNameKeepalived); err == nil {
		t.Error("the keepalived service is still on the swarm after the switch-off")
	}
	if secrets, err := v.cli.SecretList(testsupport.Context(t)); err != nil || len(secrets) != 0 {
		t.Errorf("secrets after the switch-off = %v, %v; want none", secrets, err)
	}
	rec = v.credCall(t, v.adminA, http.MethodDelete, base, "")
	if p := decodeVIPProblem(t, rec.Body.Bytes()); rec.Code != http.StatusNotFound || p.Type != "vip_not_enabled" {
		t.Errorf("switch off again: %d %s, want 404 vip_not_enabled", rec.Code, rec.Body)
	}
}
