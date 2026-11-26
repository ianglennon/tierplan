// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// seedOtherVIP gives another environment a virtual IP, so the address and router ID checks have a neighbour to meet.
func seedOtherVIP(t *testing.T, v *vipEnableRig, address string, prefixLen, routerID int) {
	t.Helper()
	if _, err := v.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at, env_type)
		VALUES ('env-other', 'agent', 'other', '', 1, 1, 1, 'swarm')`); err != nil {
		t.Fatalf("insert the other environment: %v", err)
	}
	cfg := ingressstore.VIPConfig{
		EnvID: "env-other", Address: mustAddr(address), PrefixLen: prefixLen, RouterID: routerID, KeySealed: "sealed", KeyGeneration: 1, Enabled: true,
	}
	if err := v.istore.PutVIP(testsupport.Context(t), cfg); err != nil {
		t.Fatalf("PutVIP of the other environment: %v", err)
	}
}

// wantRefused fails the test unless the enable is refused before anything is written with a PreflightError carrying code, and
// the preview refuses with the same code. It returns the refusing finding.
func wantRefused(t *testing.T, v *vipEnableRig, req ingressdeploy.VIPRequest, code string) ingressdeploy.Finding {
	t.Helper()
	ctx := testsupport.Context(t)
	rowBefore, errBefore := v.istore.GetVIP(ctx, v.envID)
	pv, err := v.svc.PreviewVIP(ctx, v.envID, req)
	if err != nil {
		t.Fatalf("PreviewVIP: %v", err)
	}
	if !slices.Contains(codesOf(pv.Findings, ingressdeploy.SeverityError), code) {
		t.Errorf("the preview's refusals are %v, want %s", codesOf(pv.Findings, ingressdeploy.SeverityError), code)
	}
	_, err = v.svc.EnableVIP(ctx, testActor, testOwner, v.envID, req)
	var pe *ingressdeploy.PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("EnableVIP = %v, want a *PreflightError with %s", err, code)
	}
	var found *ingressdeploy.Finding
	for i, f := range pe.Findings {
		if f.Code == code && f.Severity == ingressdeploy.SeverityError {
			found = &pe.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("the refusals are %v, want %s", codesOf(pe.Findings, ingressdeploy.SeverityError), code)
	}
	if w := v.fake.Writes(); len(w) != 0 {
		t.Errorf("a refused enable wrote %v", w)
	}
	if n := len(v.fake.SecretCreates()) + len(v.fake.ServiceCreates()); n != 0 {
		t.Errorf("a refused enable created %d objects", n)
	}
	if rowAfter, errAfter := v.istore.GetVIP(ctx, v.envID); rowAfter != rowBefore || !errors.Is(errAfter, errBefore) {
		t.Errorf("a refused enable changed the row: %+v (%v), was %+v (%v)", rowAfter, errAfter, rowBefore, errBefore)
	}
	if v.auditCount("ingress.vip_enabled") != 0 {
		t.Error("a refused enable wrote an audit row")
	}
	if _, held := v.locks.Held(v.envID); held {
		t.Error("a refused enable left the operation lock held")
	}
	return *found
}

// TestEnableVIPRefusals: every refusal of the enable ends in a PreflightError with its stable code, and nothing was written
// to the swarm or the store (D-16, D-19, D-08).
func TestEnableVIPRefusals(t *testing.T) {
	ok := ingressdeploy.VIPRequest{Address: vipTestAddress}
	cases := []struct {
		name  string
		code  string
		req   ingressdeploy.VIPRequest
		setup func(t *testing.T, v *vipEnableRig)
		// config adjusts the fake swarm.
		config func(*dockertest.FakeConfig)
		deps   func(*ingressdeploy.Deps)
		// check looks at the refusing finding.
		check func(t *testing.T, f ingressdeploy.Finding)
	}{
		{name: "ingress off", code: "vip_ingress_off", req: ok, setup: func(t *testing.T, v *vipEnableRig) {
			if _, err := v.istore.SetEnabled(testsupport.Context(t), v.envID, false, "u", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "ingress paused", code: ingressdeploy.CodePaused, req: ok, setup: func(t *testing.T, v *vipEnableRig) {
			if _, err := v.istore.SetPaused(testsupport.Context(t), v.envID, true, "u", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a manager refuses", code: ingressdeploy.CodeVIPPrivilegedNotAllowed, req: ok, setup: func(_ *testing.T, v *vipEnableRig) {
			v.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerRefuses, envs.ManagerAllows), nil)
		}, check: func(t *testing.T, f ingressdeploy.Finding) {
			if !strings.Contains(f.Message, "TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true") {
				t.Errorf("the sentence does not name the setting: %q", f.Message)
			}
			if !slices.Contains(f.Items, "manager-2: "+envs.ManagerRefuses) || !slices.Contains(f.Items, "manager-1: "+envs.ManagerAllows) {
				t.Errorf("items = %v, want each manager's state", f.Items)
			}
		}},
		{name: "a manager has no agent", code: ingressdeploy.CodeVIPPrivilegedNotAllowed, req: ok, setup: func(_ *testing.T, v *vipEnableRig) {
			v.managers.set(vipManagerRows(envs.ManagerAllows, envs.ManagerUnknown, envs.ManagerAllows), nil)
		}},
		{name: "the managers cannot be read", code: ingressdeploy.CodeVIPPrivilegedNotAllowed, req: ok, setup: func(_ *testing.T, v *vipEnableRig) {
			v.managers.set(nil, errors.New("boom: engine words"))
		}, check: func(t *testing.T, f ingressdeploy.Finding) {
			if !strings.Contains(f.Message, "could not be read") || strings.Contains(f.Message, "boom") {
				t.Errorf("message = %q, want it to say the managers could not be read and not the error", f.Message)
			}
		}},
		{name: "no managers dependency", code: ingressdeploy.CodeVIPPrivilegedNotAllowed, req: ok, deps: func(d *ingressdeploy.Deps) { d.Managers = nil }},
		{name: "address invalid", code: ingressdeploy.CodeVIPAddressInvalid, req: ingressdeploy.VIPRequest{Address: "10.9.0.50"}},
		{name: "address IPv6", code: ingressdeploy.CodeVIPIPv6Unsupported, req: ingressdeploy.VIPRequest{Address: "fd00::5/64"}},
		{name: "address not unicast", code: ingressdeploy.CodeVIPNotUnicast, req: ingressdeploy.VIPRequest{Address: "127.0.0.1/8"}},
		{name: "address is the network address", code: ingressdeploy.CodeVIPNetworkOrBroadcast, req: ingressdeploy.VIPRequest{Address: "10.9.0.0/24"}},
		{name: "address outside the nodes' network", code: ingressdeploy.CodeVIPOutsideNodeSubnet, req: ingressdeploy.VIPRequest{Address: "10.8.0.50/24"}},
		{name: "address is a node's", code: ingressdeploy.CodeVIPIsNodeAddress, req: ingressdeploy.VIPRequest{Address: "10.9.0.12/24"}},
		{name: "address is another environment's", code: ingressdeploy.CodeVIPInUseByEnvironment, req: ok, setup: func(t *testing.T, v *vipEnableRig) {
			seedOtherVIP(t, v, "10.9.0.50", 24, 77)
		}, check: func(t *testing.T, f ingressdeploy.Finding) {
			if !slices.Contains(f.Items, "prod") {
				t.Errorf("items = %v, want the other environment's name", f.Items)
			}
		}},
		{name: "address overlaps an allocated network", code: ingressdeploy.CodeVIPOverlapsDocker, req: ok, config: func(c *dockertest.FakeConfig) {
			c.Networks = append(c.Networks, dockertest.FakeNetwork{ID: "app-net", Name: "app-net", Driver: "overlay", Scope: "swarm", Subnets: []string{"10.9.0.0/25"}})
		}, check: func(t *testing.T, f ingressdeploy.Finding) {
			if !slices.Contains(f.Items, "app-net") {
				t.Errorf("items = %v, want the network's name", f.Items)
			}
		}},
		{name: "router ID invalid", code: ingressdeploy.CodeVIPRouterIDInvalid, req: ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(256)}},
		{name: "router ID zero", code: ingressdeploy.CodeVIPRouterIDInvalid, req: ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(0)}},
		{name: "router ID in use", code: ingressdeploy.CodeVIPRouterIDInUse, req: ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(77)},
			setup: func(t *testing.T, v *vipEnableRig) { seedOtherVIP(t, v, "10.9.0.60", 24, 77) }},
		{name: "already enabled", code: ingressdeploy.CodeVIPAlreadyEnabled, req: ok, setup: func(t *testing.T, v *vipEnableRig) {
			cfg := ingressstore.VIPConfig{EnvID: v.envID, Address: mustAddr("10.9.0.70"), PrefixLen: 24, RouterID: 5, KeySealed: "sealed", KeyGeneration: 1, Enabled: true}
			if err := v.istore.PutVIP(testsupport.Context(t), cfg); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVIPEnableRig(t, tc.config, tc.deps)
			if tc.setup != nil {
				tc.setup(t, v)
			}
			f := wantRefused(t, v, tc.req, tc.code)
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}

	t.Run("standalone", func(t *testing.T) {
		v := newVIPEnableRig(t, nil, nil)
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.Swarm = false })
		wantRefused(t, v, ok, ingressdeploy.CodeVIPSwarmOnly)
	})
	t.Run("no manager connected", func(t *testing.T) {
		v := newVIPEnableRig(t, nil, nil)
		v.envs.set(func(i *ingressdeploy.EnvInfo) { i.ManagerConnected = false })
		wantRefused(t, v, ok, ingressdeploy.CodeNoManager)
	})
}

// TestVIPPreflightNameTaken: a service named tierplan-keepalived, or a secret under its prefix, that is not TierPlan's refuses
// the enable naming the object; TierPlan's own objects do not (A-08).
func TestVIPPreflightNameTaken(t *testing.T) {
	req := ingressdeploy.VIPRequest{Address: vipTestAddress}
	foreign := func(c *dockertest.FakeConfig) {
		c.ServiceRows = append(c.ServiceRows, dockertest.FakeService{ID: "svc-x", Name: ingress.ServiceNameKeepalived, Mode: "global"})
	}
	t.Run("a foreign service", func(t *testing.T) {
		v := newVIPEnableRig(t, foreign, nil)
		f := wantRefused(t, v, req, ingressdeploy.CodeVIPNameTaken)
		if !slices.Contains(f.Items, "service "+ingress.ServiceNameKeepalived) {
			t.Errorf("items = %v, want the service named", f.Items)
		}
	})
	t.Run("a foreign secret", func(t *testing.T) {
		v := newVIPEnableRig(t, func(c *dockertest.FakeConfig) {
			c.Secrets = append(c.Secrets, dockertest.FakeSecret{ID: "sec-x", Name: ingress.SecretPrefixKeepalived + "x"})
		}, nil)
		f := wantRefused(t, v, req, ingressdeploy.CodeVIPNameTaken)
		if !slices.Contains(f.Items, "secret "+ingress.SecretPrefixKeepalived+"x") {
			t.Errorf("items = %v, want the secret named", f.Items)
		}
	})
	t.Run("TierPlan's own objects", func(t *testing.T) {
		v := newVIPEnableRig(t, func(c *dockertest.FakeConfig) {
			c.ServiceRows = append(c.ServiceRows, dockertest.FakeService{ID: "svc-k", Name: ingress.ServiceNameKeepalived, Mode: "global", Labels: ingress.KeepalivedLabels()})
			c.Secrets = append(c.Secrets, dockertest.FakeSecret{ID: "sec-k", Name: ingress.SecretPrefixKeepalived + "old", Labels: ingress.KeepalivedLabels()})
		}, nil)
		pv, err := v.svc.PreviewVIP(testsupport.Context(t), v.envID, req)
		if err != nil {
			t.Fatal(err)
		}
		if errs := codesOf(pv.Findings, ingressdeploy.SeverityError); len(errs) > 0 {
			t.Errorf("TierPlan's own objects refuse with %v", errs)
		}
	})
}

// TestEnableVIPWarnings: an address inside only a pool, and the unverified line, are warnings and the enable goes on with them;
// an override of the router ID is used as given and no override takes the derived one.
func TestEnableVIPWarnings(t *testing.T) {
	v := newVIPEnableRig(t, nil, nil)
	pv, err := v.svc.PreviewVIP(testsupport.Context(t), v.envID, ingressdeploy.VIPRequest{Address: vipTestAddress})
	if err != nil {
		t.Fatal(err)
	}
	warn := codesOf(pv.Findings, ingressdeploy.SeverityWarning)
	if !slices.Contains(warn, ingressdeploy.CodeVIPInsidePool) || !slices.Contains(warn, ingressdeploy.CodeVIPUnverified) {
		t.Fatalf("warnings = %v, want the pool overlap and the unverified line", warn)
	}
	if errs := codesOf(pv.Findings, ingressdeploy.SeverityError); len(errs) > 0 {
		t.Fatalf("refusals = %v, want none", errs)
	}
	if pv.RouterID != ingressdeploy.DerivedRouterID(testClusterID) || !pv.RouterDerived {
		t.Errorf("no override gave router ID %d derived %v, want the derived one", pv.RouterID, pv.RouterDerived)
	}

	wantVIPAllDone(t, v.enableVIP(ingressdeploy.VIPRequest{Address: vipTestAddress, RouterID: intp(255)}))
	row, err := v.istore.GetVIP(testsupport.Context(t), v.envID)
	if err != nil {
		t.Fatal(err)
	}
	if row.RouterID != 255 || row.RouterDerived {
		t.Errorf("row router ID = %d derived %v, want the override 255 as chosen", row.RouterID, row.RouterDerived)
	}
}
