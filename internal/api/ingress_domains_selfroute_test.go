// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// A domain cannot be deleted from under TierPlan's own route, and the refusal says what to do (Phase 28, D-09).

// TestDomainDeleteNamesTierPlansOwnRoute: with TierPlan's own route under the domain, its delete is refused with the domain-in-use
// problem, the holders list names TierPlan itself in the environment, and the sentence says to turn off TierPlan's own route
// first; it asks to remove hostnames from the stacks only once a stack holds one too.
func TestDomainDeleteNamesTierPlansOwnRoute(t *testing.T) {
	t.Parallel()
	const turnOff = "Turn off TierPlan's own route first."
	const fromStacks = "Remove those hostnames from the stacks first."
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	envName := f.envNameOf(t, f.envA)
	if err := f.ingressStore.SetSystemClaim(context.Background(), ingressstore.SystemClaimInput{
		EnvID: f.envA, Hostname: "tp.apps.example.com", DomainID: d.ID, Service: "tierplan", Port: 7480, Actor: "admin",
	}, nil); err != nil {
		t.Fatalf("SetSystemClaim: %v", err)
	}
	self := "TierPlan itself in " + envName

	refused := func(t *testing.T) ingressProblem {
		t.Helper()
		rec := f.credCall(t, f.adminA, http.MethodDelete, ingressPath(f.envA, "/", d.ID), "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("delete under TierPlan's own route: %d %s, want 409", rec.Code, rec.Body)
		}
		p := decodeIngressProblem(t, rec)
		if p.Type != "ingress_domain_in_use" {
			t.Errorf("type = %q, want ingress_domain_in_use", p.Type)
		}
		return p
	}

	t.Run("TierPlan's own route alone", func(t *testing.T) {
		p := refused(t)
		if len(p.Errors) != 1 || p.Errors[0].Location != "holders[0]" || p.Errors[0].Message != self {
			t.Errorf("errors = %+v, want TierPlan itself as the one holder", p.Errors)
		}
		if !strings.Contains(p.Detail, self) || !strings.HasSuffix(p.Detail, turnOff) {
			t.Errorf("detail = %q, want it to name %q and end %q", p.Detail, self, turnOff)
		}
		if strings.Contains(p.Detail, fromStacks) {
			t.Errorf("detail = %q asks to remove hostnames from stacks, but no stack holds one", p.Detail)
		}
	})

	t.Run("with a stack holder as well", func(t *testing.T) {
		f.claim(t, f.envA, "stack-web", "web", "web.apps.example.com")
		p := refused(t)
		want := map[string]bool{self: false, "web in " + envName: false}
		for _, e := range p.Errors {
			if _, ok := want[e.Message]; ok {
				want[e.Message] = true
			}
		}
		for label, seen := range want {
			if !seen {
				t.Errorf("errors = %+v, want %q among the holders", p.Errors, label)
			}
		}
		if !strings.Contains(p.Detail, fromStacks) || !strings.HasSuffix(p.Detail, turnOff) {
			t.Errorf("detail = %q, want both %q and, last, %q", p.Detail, fromStacks, turnOff)
		}
	})

	if views, err := f.ingressStore.Domains(context.Background(), f.envA); err != nil || len(views) != 1 {
		t.Errorf("domains = %+v, %v, want the domain kept", views, err)
	}
}
