// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// The review carries the ingress policy into the draft check (plan 18-11, D-27): a hostname the environment's
// world does not allow is a problem in the review's list, and without a policy only the parser speaks.

const ingressReviewCompose = `services:
  web:
    image: nginx:1.27
    deploy:
      labels:
        tierplan.ingress.host: app.elsewhere.org
        tierplan.ingress.port: "80"
`

func TestReviewIngressPolicy(t *testing.T) {
	t.Parallel()
	r := newSwarmRig(t, nil)
	in := r.reviewInput(t, "ingress", ingressReviewCompose, nil)

	// No policy: a host outside every domain is not a problem, the parser accepts the labels.
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review without a policy: %v", err)
	}
	if hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Fatalf("problems = %+v, want no policy problem without a policy", rv.Problems)
	}

	// With a world that registers only apps.example.com the same draft is outside the domains.
	in.IngressPolicy = &stacks.IngressPolicy{StackID: in.Stack.ID, World: ingress.World{
		Domains: []ingress.Domain{{ID: "dom-1", EnvID: r.envID, Suffix: "apps.example.com"}},
	}}
	rv, err = r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review with a policy: %v", err)
	}
	if !hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Fatalf("problems = %+v, want %s", rv.Problems, ingress.CodeDomainNotAllowed)
	}
	for _, p := range rv.Problems {
		if p.Code == ingress.CodeDomainNotAllowed && p.Severity != stacks.SeverityError {
			t.Errorf("problem = %+v, want an error that blocks the deploy", p)
		}
	}
}

func TestReviewIngressPolicyServesStandaloneDrafts(t *testing.T) {
	t.Parallel()
	r := newStandaloneRig(t)
	in := r.reviewInput(t, "ingress-sa", `services:
  web:
    image: nginx:1.27
    labels:
      tierplan.ingress.host: app.elsewhere.org
      tierplan.ingress.port: "80"
`, nil)
	in.Stack.Target = stacks.TargetStandalone
	in.IngressPolicy = &stacks.IngressPolicy{StackID: in.Stack.ID, World: ingress.World{
		Domains: []ingress.Domain{{ID: "dom-1", EnvID: r.envID, Suffix: "apps.example.com"}},
	}}
	// The one draft check serves both targets: the standalone review that follows it starts from what it found.
	rv, err := r.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !hasCode(rv.Problems, ingress.CodeDomainNotAllowed) {
		t.Errorf("problems = %+v, want %s on a standalone draft too", rv.Problems, ingress.CodeDomainNotAllowed)
	}
}

func hasCode(ps stacks.Problems, code string) bool {
	for _, p := range ps {
		if p.Code == code {
			return true
		}
	}
	return false
}
