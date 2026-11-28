// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestIngressPreviewsCarryTheSelfNotices is UI-SPEC S4: the ingress enable preview carries the carve-out sentence as its
// notice, out of the findings; the ingress disable preview says TierPlan's own route is released first and where TierPlan
// stays reachable, and carries no notice when the route is not on.
func TestIngressPreviewsCarryTheSelfNotices(t *testing.T) {
	t.Run("enable, the carve-out applies", func(t *testing.T) {
		r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) {
			d.OwnHTTP01InUse = func(context.Context) (bool, error) { return true, nil }
			d.LocalSwarm = func(context.Context) (envs.SwarmMembership, error) {
				return envs.SwarmMembership{Known: true, Member: true, ClusterID: testClusterID, NodeID: "elsewhere"}, nil
			}
			d.Self = selfReads(selfServiceOnNet, nil)
			d.SelfURLs = ownURLs
		}})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		want := "TierPlan's own certificate uses the HTTP-01 challenge. The ingress sends only /.well-known/acme-challenge/ requests for " +
			"tierplan.example.org and tierplan.internal to TierPlan, over the tierplan-self network, so the certificate keeps renewing."
		if res.Notice != want {
			t.Errorf("notice = %q, want %q", res.Notice, want)
		}
		if f, ok := findingOf(res.Findings, ingressdeploy.CodeHTTP01CarveOut); ok {
			t.Errorf("findings still hold the carve-out: %+v", f)
		}
	})

	t.Run("enable, no carve-out", func(t *testing.T) {
		r := newRig(t, rigOptions{})
		res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if res.Notice != "" {
			t.Errorf("notice = %q, want none", res.Notice)
		}
	})

	t.Run("disable", func(t *testing.T) {
		r, _, _, dom := newSelfRouteRig(t, selfRigOptions{})
		ctx := testsupport.Context(t)
		pv, err := r.svc.PreviewDisable(ctx, r.envID, seesAll)
		if err != nil {
			t.Fatalf("PreviewDisable: %v", err)
		}
		if pv.Notice != "" {
			t.Errorf("notice without the route = %q, want none", pv.Notice)
		}

		r.claimSelfRoute(dom)
		pv, err = r.svc.PreviewDisable(ctx, r.envID, seesAll)
		if err != nil {
			t.Fatalf("PreviewDisable: %v", err)
		}
		want := "TierPlan's own route on tp.apps.example.org is released first. Direct access on https://tierplan.example.org:7480 keeps working."
		if pv.Notice != want {
			t.Errorf("notice = %q, want %q", pv.Notice, want)
		}
		if len(pv.Findings) != 0 {
			t.Errorf("findings = %+v, want none", pv.Findings)
		}
	})

	t.Run("disable, standalone", func(t *testing.T) {
		r := newStandaloneDisableRig(t, nil)
		claimOwnRoute(r.rig)
		pv, err := r.svc.PreviewDisable(testsupport.Context(t), r.envID, seesAll)
		if err != nil {
			t.Fatalf("PreviewDisable: %v", err)
		}
		want := "TierPlan's own route on tp.apps.example.org is released first. Direct access on TierPlan's own address keeps working."
		if pv.Notice != want {
			t.Errorf("notice = %q, want %q", pv.Notice, want)
		}
	})
}
