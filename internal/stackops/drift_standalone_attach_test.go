// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stackops/stackopstest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// attachDriftCompose is a stack with one routed service and one that is not.
const attachDriftCompose = `services:
  web:
    image: nginx:1.27-alpine
    labels:
      tierplan.ingress.host: app.apps.example.com
      tierplan.ingress.port: "80"
    environment:
      MODE: prod
  worker:
    image: busybox:1.36
    command: ["sleep", "1000"]
`

// attachDriftDeclared is the same stack with the shared network listed by the file itself, with a setting of the person's.
const attachDriftDeclared = `services:
  web:
    image: nginx:1.27-alpine
    labels:
      tierplan.ingress.host: app.apps.example.com
      tierplan.ingress.port: "80"
    environment:
      MODE: prod
    networks:
      default:
      front:
        priority: 3
networks:
  front:
    external: true
    name: tierplan-ingress
`

// driftWhile is the drift check of compose, as a revision deployed it, while the host holds h, with ingress on or off now.
func driftWhile(t *testing.T, h *stackopstest.Hosted, compose string, ingressOn bool) stackops.DriftResult {
	t.Helper()
	rig := hostRig(t, h)
	if ingressOn {
		rig.deps.Ingress = &fakeIngress{attach: attachedBy()}
	} else {
		rig.deps.Ingress = &fakeIngress{}
	}
	st := standaloneStackOf(h.Name)
	rev := stackops.Snapshot{
		ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
	}
	res, err := rig.deps.Drift(testsupport.Context(t), stackops.DriftInput{EnvID: rig.envID, Stack: st, Deployed: &rev, Pins: h.Pins})
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	return res
}

// TestStandaloneDriftIgnoresTheAttachment is the tracer of D-16 / Phase 24 D-20: a stack deployed before ingress was
// switched on reads in sync once it is on, and one deployed attached reads in sync once it is off.
func TestStandaloneDriftIgnoresTheAttachment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		compose    string
		attachedAt bool // the host's containers were made while ingress was on
		ingressOn  bool // ingress is on now
	}{
		{"deployed before ingress, on now", attachDriftCompose, false, true},
		{"deployed attached, off now", attachDriftCompose, true, false},
		{"deployed attached, on now", attachDriftCompose, true, true},
		{"deployed unattached, off now", attachDriftCompose, false, false},
		{"a listed entry, deployed attached, off now", attachDriftDeclared, true, false},
		{"a listed entry, deployed before ingress, on now", attachDriftDeclared, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var h *stackopstest.Hosted
			if tc.attachedAt {
				h = stackopstest.HostAttached(t, "demo", "stack-demo", tc.compose, nil)
			} else {
				h = hostStack(t, "demo", tc.compose, nil)
			}

			res := driftWhile(t, h, tc.compose, tc.ingressOn)

			if res.State != stackops.DriftNone || len(res.Services) != 0 || res.Count != 0 {
				t.Fatalf("drift = %s %v (count %d), want none: the attachment is TierPlan's", res.State, driftNames(res), res.Count)
			}
		})
	}
}

// TestStandaloneDriftStillSeesARealChange (T-25-28): only the attachment is left out; a changed environment value of the
// same service is drift in every combination, and what it shows does not mention the shared network.
func TestStandaloneDriftStillSeesARealChange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		attachedAt bool
		ingressOn  bool
	}{
		{"deployed before ingress, on now", false, true},
		{"deployed attached, off now", true, false},
		{"deployed attached, on now", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var h *stackopstest.Hosted
			if tc.attachedAt {
				h = stackopstest.HostAttached(t, "demo", "stack-demo", attachDriftCompose, nil)
			} else {
				h = hostStack(t, "demo", attachDriftCompose, nil)
			}
			env := h.Docs["web"].Config.Env
			i := slices.Index(env, "MODE=prod")
			if i < 0 {
				t.Fatalf("the container's environment %v has no MODE=prod", env)
			}
			env[i] = "MODE=dev"

			res := driftWhile(t, h, attachDriftCompose, tc.ingressOn)

			if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) {
				t.Fatalf("drift = %s %v, want drifted with web changed", res.State, driftNames(res))
			}
			web := res.Services[0]
			if web.Diff.Settings != 1 || res.Count != 1 {
				t.Errorf("web = %+v count %d, want the one environment value counted", web.Diff, res.Count)
			}
			for side, doc := range map[string]string{"what runs": string(web.Diff.Current), "what was deployed": string(web.Diff.Next)} {
				if strings.Contains(doc, ingress.NetworkName) {
					t.Errorf("%s = %s, want the attachment left out", side, doc)
				}
			}
		})
	}
}
