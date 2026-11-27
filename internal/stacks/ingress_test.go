// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// TestStackMayNotNameTierPlanSelf pins D-02, D-06 and CR-02: every service on tierplan-self is a trusted proxy and can
// answer TierPlan's own backend name, so no stack may reach it, whether it declares a network of that name, points at it as
// an external one or names it as a network mode, on either target. The one exception is the swarm stack TierPlan's own
// service runs in (IngressPolicy.SelfNamespace), which reaches it through the D-03 lines as an external network; even that
// stack may not declare a network of its own by the name. The ingress network's rule is unchanged.
func TestStackMayNotNameTierPlanSelf(t *testing.T) {
	t.Parallel()
	check := func(name, compose string, target stacks.Target, selfNS string) (reserved []stacks.Problem, errs []stacks.Problem) {
		in := stacks.CheckInput{Name: name, Target: target, Copy: copyOf(compose)}
		if target == stacks.TargetSwarm {
			in.Ingress = &stacks.IngressAttach{Network: ingress.NetworkName}
		}
		if selfNS != "" {
			in.IngressPolicy = &stacks.IngressPolicy{StackID: "st-1", SelfNamespace: selfNS}
		}
		for _, p := range stacks.Check(context.Background(), in).Problems {
			if p.Code == stacks.CodeIngressNetworkReserved || p.Code == stacks.CodeSelfNetworkReserved {
				reserved = append(reserved, p)
			} else if p.Severity == stacks.SeverityError {
				errs = append(errs, p)
			}
		}
		return reserved, errs
	}
	const (
		externalByKey = `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    external: true
`
		externalByName = `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    external: true
    name: tierplan-self
`
		ownByKey = `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self: {}
`
	)
	cases := []struct {
		name, compose string
		want          int
		code, key     string
		names         string // the reserved name the message must carry
	}{
		{"own network under the self key", ownByKey, 1, stacks.CodeSelfNetworkReserved, "networks.tierplan-self", ingress.SelfNetworkName},
		{"own network under the self key with a driver", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    driver: bridge
`, 1, stacks.CodeSelfNetworkReserved, "networks.tierplan-self", ingress.SelfNetworkName},
		{"own network under another key with the self name", `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    name: tierplan-self
`, 1, stacks.CodeSelfNetworkReserved, "networks.edge", ingress.SelfNetworkName},
		{"external under the self key with another name", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    external: true
    name: somewhere-else
`, 1, stacks.CodeSelfNetworkReserved, "networks.tierplan-self", ingress.SelfNetworkName},
		{"external by key", externalByKey, 1, stacks.CodeSelfNetworkReserved, "networks.tierplan-self", ingress.SelfNetworkName},
		{"external by name under another key", externalByName, 1, stacks.CodeSelfNetworkReserved, "networks.edge", ingress.SelfNetworkName},
		{"the self network as a network mode", `services:
  web:
    image: nginx:1.27
    network_mode: tierplan-self
`, 1, stacks.CodeSelfNetworkReserved, "services.web.network_mode", ingress.SelfNetworkName},
		{"the ingress network is still refused as a network of the stack's own", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress: {}
`, 1, stacks.CodeIngressNetworkReserved, "networks.tierplan-ingress", ingress.NetworkName},
		{"the ingress network as an external one is still allowed", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress:
    external: true
`, 0, "", "", ""},
		{"a network of another name", routedCompose, 0, "", "", ""},
	}
	for _, c := range cases {
		for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
			if strings.Contains(c.name, "network mode") && target == stacks.TargetSwarm {
				continue // a swarm refuses network_mode on its own; the standalone check is the one that matters
			}
			// A stack of another name than TierPlan's own is refused alike with or without a policy.
			for _, selfNS := range []string{"", "tierplan"} {
				got, _ := check("shop", c.compose, target, selfNS)
				if len(got) != c.want {
					t.Errorf("%s (%s, self %q): %d reserved-name problems %+v, want %d", c.name, target, selfNS, len(got), got, c.want)
					continue
				}
				for _, p := range got {
					if p.Severity != stacks.SeverityError || p.Code != c.code || p.Key != c.key || p.Line == 0 || !strings.Contains(p.Message, c.names) {
						t.Errorf("%s (%s): problem %+v, want a %s error at %s on a line naming %s", c.name, target, p, c.code, c.key, c.names)
					}
				}
			}
		}
	}

	// TierPlan's own swarm stack may point at the network as an external one, and only that.
	for _, compose := range []string{externalByKey, externalByName} {
		got, errs := check("tierplan", compose, stacks.TargetSwarm, "tierplan")
		if len(got) != 0 {
			t.Errorf("TierPlan's own swarm stack, external: reserved-name problems %+v, want none", got)
		}
		for _, p := range errs {
			if strings.Contains(p.Message, ingress.SelfNetworkName) {
				t.Errorf("TierPlan's own swarm stack: the external reference was refused: %+v", p)
			}
		}
		if got, _ := check("tierplan", compose, stacks.TargetStandalone, "tierplan"); len(got) != 1 {
			t.Errorf("a standalone stack named like TierPlan's swarm stack: %d problems %+v, want 1: TierPlan connects itself there", len(got), got)
		}
		if got, _ := check("tierplan", compose, stacks.TargetSwarm, ""); len(got) != 1 {
			t.Errorf("a swarm stack named tierplan with no self namespace known: %d problems %+v, want 1", len(got), got)
		}
	}
	if got, _ := check("tierplan", ownByKey, stacks.TargetSwarm, "tierplan"); len(got) != 1 || got[0].Code != stacks.CodeSelfNetworkReserved {
		t.Errorf("TierPlan's own swarm stack declaring a network of its own by the name: %+v, want one %s", got, stacks.CodeSelfNetworkReserved)
	}
}
