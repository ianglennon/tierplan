// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Every deploy gate carries the read-only ingress policy (plan 18-13, D-18, D-21): a revision whose hostname the
// environment's world refuses is not prepared, not re-checked into a standalone job and not offered as a roll back.

const (
	gateHost = "app.apps.example.com"

	gateSwarmCompose = `services:
  web:
    image: nginx:1.27-alpine
    deploy:
      labels:
        tierplan.ingress.host: app.apps.example.com
        tierplan.ingress.port: "80"
`
	gateStandaloneCompose = `services:
  web:
    image: nginx:1.27-alpine
    labels:
      tierplan.ingress.host: app.apps.example.com
      tierplan.ingress.port: "80"
`
)

var gatePinned = "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)

// gatePolicy is a world that registers apps.example.com and holds back the host the way mode says: "reserved",
// "held" (another stack holds it), "outside" (no domain covers it) or "free" (nothing is wrong).
func gatePolicy(mode string) *stacks.IngressPolicy {
	w := ingress.World{Domains: []ingress.Domain{{ID: "dom-1", EnvID: "env-a", Suffix: "apps.example.com"}}}
	switch mode {
	case "reserved":
		w.Reserved = map[string]bool{gateHost: true}
	case "held":
		w.Claims = map[string]ingress.ClaimInfo{gateHost: {
			Claim:   ingress.Claim{EnvID: "env-a", Hostname: gateHost, OwnerKind: ingress.OwnerStack, StackID: "other-stack"},
			EnvName: "prod", StackName: "other",
		}}
	case "outside":
		w.Domains = []ingress.Domain{{ID: "dom-2", EnvID: "env-a", Suffix: "elsewhere.org"}}
	}
	return &stacks.IngressPolicy{StackID: "stack-web", World: w}
}

var gateCases = []struct{ mode, code string }{
	{"reserved", ingress.CodeHostReserved},
	{"held", ingress.CodeHostTaken},
	{"outside", ingress.CodeDomainNotAllowed},
}

// refusedWith fails unless err is a prepare error of the load stage that carries a problem with code.
func refusedWith(t *testing.T, err error, code string) {
	t.Helper()
	var pe *stackops.PrepareError
	var nd *stacks.NotDeployableError
	if !errors.As(err, &pe) || pe.Stage != stackops.PrepareLoad || !errors.As(err, &nd) {
		t.Fatalf("err = %v, want a load-stage prepare error wrapping a not-deployable error", err)
	}
	for _, p := range nd.Problems {
		if p.Code == code {
			return
		}
	}
	t.Fatalf("problems = %+v, want %s", nd.Problems, code)
}

func TestIngressGatePrepareRollbackRefusesTheHostnameThePolicyRefuses(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"web_web": gatePinned}
	for _, c := range gateCases {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			_, err := stackops.Deps{}.PrepareRollbackWith(context.Background(), rollbackStack, rollbackTarget(gateSwarmCompose), pins, gatePolicy(c.mode))
			refusedWith(t, err, c.code)
		})
	}
}

func TestIngressGatePrepareRollbackPreparesTheSameRevisionWithoutAPolicyOrWithAFreeName(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"web_web": gatePinned}
	target := rollbackTarget(gateSwarmCompose)
	if _, err := (stackops.Deps{}).PrepareRollback(context.Background(), rollbackStack, target, pins); err != nil {
		t.Errorf("PrepareRollback: %v, want the revision prepared without a policy", err)
	}
	if _, err := (stackops.Deps{}).PrepareRollbackWith(context.Background(), rollbackStack, target, pins, nil); err != nil {
		t.Errorf("PrepareRollbackWith(nil): %v, want the revision prepared without a policy", err)
	}
	if _, err := (stackops.Deps{}).PrepareRollbackWith(context.Background(), rollbackStack, target, pins, gatePolicy("free")); err != nil {
		t.Errorf("PrepareRollbackWith(free): %v, want a free name under a registered domain prepared", err)
	}
}

func TestIngressGatePrepareRollbackStandaloneRefusesTheHostnameThePolicyRefuses(t *testing.T) {
	t.Parallel()
	pins := map[string]string{"web": gatePinned}
	st := standaloneStackOf("web")
	target := rollbackTarget(gateStandaloneCompose)
	for _, c := range gateCases {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			_, err := stackops.Deps{}.PrepareRollbackStandaloneWith(context.Background(), st, target, pins, gatePolicy(c.mode))
			refusedWith(t, err, c.code)
		})
	}
	if _, err := (stackops.Deps{}).PrepareRollbackStandalone(context.Background(), st, target, pins); err != nil {
		t.Errorf("PrepareRollbackStandalone: %v, want the revision prepared without a policy", err)
	}
	if _, err := (stackops.Deps{}).PrepareRollbackStandaloneWith(context.Background(), st, target, pins, nil); err != nil {
		t.Errorf("PrepareRollbackStandaloneWith(nil): %v, want the revision prepared without a policy", err)
	}
	if _, err := (stackops.Deps{}).PrepareRollbackStandaloneWith(context.Background(), st, target, pins, gatePolicy("free")); err != nil {
		t.Errorf("PrepareRollbackStandaloneWith(free): %v, want a free name prepared", err)
	}
}

func TestIngressGateStandaloneJobCheckRefusesAndRunsNothing(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	in := standaloneInput("demo", gateStandaloneCompose)
	in.IngressPolicy = gatePolicy("reserved")

	res, job := rig.deployStandalone(t, in)

	if res.OK() || res.Code != stackops.CodeStackInvalid {
		t.Fatalf("result = %+v, want stack_invalid on the check step", res)
	}
	if got := itemStates(job); got["check"] != "failed:stack_invalid" || got["services"] != "skipped:not_run" {
		t.Errorf("items = %v, want the check step failed with stack_invalid and the rest not run", got)
	}
	if stand.calls != 0 || len(rig.fake.Writes()) != 0 {
		t.Errorf("SDK calls %d, writes %v, want nothing sent to the host", stand.calls, rig.fake.Writes())
	}

	// The same input with no policy passes the check step and reaches the SDK.
	in.IngressPolicy = nil
	res, _ = rig.deployStandalone(t, in)
	if !res.OK() || stand.calls != 1 {
		t.Errorf("result = %+v after %d SDK calls, want the deploy to run without a policy", res, stand.calls)
	}
}

func TestIngressGateRollbackReviewListsThePolicyProblemWithItsCode(t *testing.T) {
	t.Parallel()
	for _, c := range gateCases {
		t.Run("swarm/"+c.mode, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, nil)
			in := rig.rollbackReviewInput(t, "web", gateSwarmCompose, map[string]string{"web_web": gatePinned})
			in.IngressPolicy = gatePolicy(c.mode)
			rv, err := rig.deps.Review(testsupport.Context(t), in)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if !hasCode(rv.Problems, stackops.CodeRevisionNotDeployable) || !hasCode(rv.Problems, c.code) {
				t.Errorf("problems = %+v, want revision_not_deployable and %s", rv.Problems, c.code)
			}
		})
		t.Run("standalone/"+c.mode, func(t *testing.T) {
			t.Parallel()
			rig := newStandaloneRig(t)
			in := rig.standaloneRollbackInput(t, "web", gateStandaloneCompose, map[string]string{"web": gatePinned})
			in.IngressPolicy = gatePolicy(c.mode)
			rv, err := rig.deps.Review(testsupport.Context(t), in)
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if !hasCode(rv.Problems, stackops.CodeRevisionNotDeployable) || !hasCode(rv.Problems, c.code) {
				t.Errorf("problems = %+v, want revision_not_deployable and %s", rv.Problems, c.code)
			}
		})
	}
}

func TestIngressGateRollbackReviewWithoutAPolicyNamesNoPolicyCode(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	// The revision stored no image for its service: it fails for a reason that has nothing to do with ingress.
	in := rig.rollbackReviewInput(t, "web", gateSwarmCompose, map[string]string{})
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Problems) != 1 || rv.Problems[0].Code != stackops.CodeRevisionNotDeployable {
		t.Fatalf("problems = %+v, want exactly revision_not_deployable", rv.Problems)
	}
	for _, p := range rv.Problems {
		if strings.HasPrefix(p.Code, "ingress_") {
			t.Errorf("problem = %+v, want no ingress code without a policy", p)
		}
	}
}
