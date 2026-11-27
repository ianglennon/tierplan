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

// TestStackMayNameTierPlanSelfOnlyAsExternal pins D-02, D-03 and T-28-14: a stack that declares a network named
// tierplan-self is refused with the reserved-name problem unless it is the external reference, which is how the stack
// file lines of D-03 name it. The ingress network's rule is unchanged.
func TestStackMayNameTierPlanSelfOnlyAsExternal(t *testing.T) {
	t.Parallel()
	check := func(compose string, target stacks.Target) (reserved []stacks.Problem, errs []stacks.Problem) {
		in := stacks.CheckInput{Name: "shop", Target: target, Copy: copyOf(compose)}
		if target == stacks.TargetSwarm {
			in.Ingress = &stacks.IngressAttach{Network: ingress.NetworkName}
		}
		for _, p := range stacks.Check(context.Background(), in).Problems {
			if p.Code == stacks.CodeIngressNetworkReserved {
				reserved = append(reserved, p)
			} else if p.Severity == stacks.SeverityError {
				errs = append(errs, p)
			}
		}
		return reserved, errs
	}
	cases := []struct {
		name, compose string
		want          int
		key           string
		names         string // the reserved name the message must carry
	}{
		{"own network under the self key", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self: {}
`, 1, "networks.tierplan-self", ingress.SelfNetworkName},
		{"own network under the self key with a driver", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    driver: bridge
`, 1, "networks.tierplan-self", ingress.SelfNetworkName},
		{"own network under another key with the self name", `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    name: tierplan-self
`, 1, "networks.edge", ingress.SelfNetworkName},
		{"external under the self key with another name", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    external: true
    name: somewhere-else
`, 1, "networks.tierplan-self", ingress.SelfNetworkName},
		{"external by key", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-self]
networks:
  tierplan-self:
    external: true
`, 0, "", ""},
		{"external by name under another key", `services:
  web:
    image: nginx:1.27
    networks: [edge]
networks:
  edge:
    external: true
    name: tierplan-self
`, 0, "", ""},
		{"the ingress network is still refused as a network of the stack's own", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress: {}
`, 1, "networks.tierplan-ingress", ingress.NetworkName},
		{"the ingress network as an external one is still allowed", `services:
  web:
    image: nginx:1.27
    networks: [tierplan-ingress]
networks:
  tierplan-ingress:
    external: true
`, 0, "", ""},
		{"a network of another name", routedCompose, 0, "", ""},
	}
	for _, c := range cases {
		for _, target := range []stacks.Target{stacks.TargetSwarm, stacks.TargetStandalone} {
			got, errs := check(c.compose, target)
			if len(got) != c.want {
				t.Errorf("%s (%s): %d reserved-name problems %+v, want %d", c.name, target, len(got), got, c.want)
				continue
			}
			for _, p := range got {
				if p.Severity != stacks.SeverityError || p.Key != c.key || p.Line == 0 || !strings.Contains(p.Message, c.names) {
					t.Errorf("%s (%s): problem %+v, want an error at %s on a line naming %s", c.name, target, p, c.key, c.names)
				}
			}
			if c.want == 0 && strings.Contains(c.name, "external") {
				for _, p := range errs {
					if strings.Contains(p.Message, ingress.SelfNetworkName) {
						t.Errorf("%s (%s): the external reference was refused: %+v", c.name, target, p)
					}
				}
			}
		}
	}
}
