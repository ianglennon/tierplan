// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// TierPlan's own route is a system route: its conditions are derived like any route's, with the tierplan-self network as
// the network it must be attached to (Phase 28 A-07), and its state follows RouteStates' precedence. An input that was not
// read is never reported ready (Phase 27 rule).

// systemRoute is TierPlan's own route as the store holds it.
var systemRoute = ingress.Route{Hostname: "tp.apps.example.org", PathPrefix: "/", Service: "tierplan", Port: 7480, Scheme: ingress.SchemeHTTPS}

// systemServingInput is the environment's input with the system route in the current bundle and both active nodes applied it.
func systemServingInput() stackops.RouteStateInput {
	key := stackops.RouteKey{Hostname: systemRoute.Hostname, Path: "/"}
	return stackops.RouteStateInput{
		Enabled: true, BundleKnown: true, NodesApplied: true, ActiveNodes: 2, AppliedNodes: 2,
		InBundle: map[stackops.RouteKey]bool{key: true},
	}
}

func TestSystemRouteConditions(t *testing.T) {
	t.Parallel()
	key := stackops.RouteKey{Hostname: systemRoute.Hostname, Path: "/"}
	type cond struct{ state, reason string }
	ready := cond{stackops.CondReady, ""}
	cases := []struct {
		name                    string
		edit                    func(*stackops.RouteStateInput)
		attached, attachedKnown bool
		want                    [5]cond
		state, reason           string
	}{
		{
			name: "attached, certificate ready and applied", attached: true, attachedKnown: true,
			want:  [5]cond{ready, ready, ready, ready, ready},
			state: stackops.RouteServing,
		},
		{
			name: "not attached", attached: false, attachedKnown: true,
			edit: func(in *stackops.RouteStateInput) { in.InBundle = map[stackops.RouteKey]bool{} },
			want: [5]cond{
				ready, {stackops.CondNotReady, stackops.ReasonSelfNotAttached}, {stackops.CondUnknown, stackops.ReasonSelfNotAttached},
				ready, {stackops.CondNotReady, stackops.ReasonNotInBundle},
			},
			state: stackops.RouteNotAttached, reason: stackops.ReasonSelfNotAttached,
		},
		{
			name: "attachment not observed yet", attached: true, attachedKnown: false,
			want: [5]cond{
				ready, {stackops.CondUnknown, stackops.ReasonNotObserved}, {stackops.CondUnknown, stackops.ReasonNotObserved}, ready, ready,
			},
			state: stackops.RouteUnknown,
		},
		{
			name: "certificate pending", attached: true, attachedKnown: true,
			edit: func(in *stackops.RouteStateInput) {
				in.InBundle = map[stackops.RouteKey]bool{}
				in.Skipped = map[stackops.RouteKey]string{key: stackops.ReasonNoCertificate}
			},
			want: [5]cond{
				ready, ready, ready, {stackops.CondNotReady, stackops.ReasonNoCertificate}, {stackops.CondNotReady, stackops.ReasonNotInBundle},
			},
			state: stackops.RouteWaitingCertificate, reason: stackops.ReasonNoCertificate,
		},
		{
			name: "TierPlan's own input cannot be rendered (WR-09)", attached: true, attachedKnown: true,
			edit: func(in *stackops.RouteStateInput) {
				in.InBundle = map[stackops.RouteKey]bool{}
				in.Skipped = map[stackops.RouteKey]string{key: stackops.ReasonSelfInputInvalid}
			},
			want: [5]cond{
				ready, ready, {stackops.CondNotReady, stackops.ReasonSelfInputInvalid}, {stackops.CondUnknown, stackops.ReasonSelfInputInvalid},
				{stackops.CondNotReady, stackops.ReasonNotInBundle},
			},
			state: stackops.RouteBackendNotRunning,
		},
		{
			name: "bundle not applied by every node", attached: true, attachedKnown: true,
			edit:  func(in *stackops.RouteStateInput) { in.NodesApplied, in.AppliedNodes = false, 1 },
			want:  [5]cond{ready, ready, ready, ready, {stackops.CondNotReady, stackops.ReasonNodesBehind}},
			state: stackops.RoutePending,
		},
		{
			name: "no bundle yet", attached: true, attachedKnown: true,
			edit: func(in *stackops.RouteStateInput) { *in = stackops.RouteStateInput{Enabled: true} },
			want: [5]cond{
				ready, ready, ready, {stackops.CondUnknown, stackops.ReasonNoBundle}, {stackops.CondUnknown, stackops.ReasonNoBundle},
			},
			state: stackops.RoutePending,
		},
		{
			name: "ingress off", attached: true, attachedKnown: true,
			edit: func(in *stackops.RouteStateInput) { in.Enabled = false },
			want: [5]cond{
				ready, {stackops.CondUnknown, stackops.ReasonIngressOff}, {stackops.CondUnknown, stackops.ReasonIngressOff},
				{stackops.CondUnknown, stackops.ReasonIngressOff}, {stackops.CondUnknown, stackops.ReasonIngressOff},
			},
			state: stackops.RouteIngressOff,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := systemServingInput()
			if tc.edit != nil {
				tc.edit(&in)
			}
			conds := stackops.SystemRouteConditions(in, systemRoute, tc.attached, tc.attachedKnown)
			if len(conds) != len(conditionOrder) {
				t.Fatalf("conditions = %+v, want five", conds)
			}
			for i, c := range conds {
				if c.Code != conditionOrder[i] {
					t.Errorf("condition %d = %q, want %q", i, c.Code, conditionOrder[i])
				}
				if got := (cond{c.State, c.Reason}); got != tc.want[i] {
					t.Errorf("%s = %+v, want %+v", c.Code, got, tc.want[i])
				}
				if c.State == stackops.CondReady && c.Reason != "" {
					t.Errorf("%s is ready with reason %q", c.Code, c.Reason)
				}
			}
			if a := conds[4]; a.Applied != in.AppliedNodes || a.Total != in.ActiveNodes {
				t.Errorf("applied counts = %d of %d, want %d of %d", a.Applied, a.Total, in.AppliedNodes, in.ActiveNodes)
			}
			state, reason := stackops.SystemRouteState(conds)
			if state != tc.state || reason != tc.reason {
				t.Errorf("state = %q (%q), want %q (%q)", state, reason, tc.state, tc.reason)
			}
		})
	}

	t.Run("a list that is not the five is unknown", func(t *testing.T) {
		t.Parallel()
		if state, _ := stackops.SystemRouteState(nil); state != stackops.RouteUnknown {
			t.Errorf("state of no conditions = %q, want unknown", state)
		}
	})
}
