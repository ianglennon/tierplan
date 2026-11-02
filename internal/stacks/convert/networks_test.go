// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package convert_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/stacks/convert"
)

// subnetCompose has the default network (one service names none), a declared network with no
// ipam, one with an explicit ipam block, and an external one.
const subnetCompose = `
services:
  web:
    image: nginx:1.27
  api:
    image: nginx:1.27
    networks: [backend, fixed, outside]
networks:
  backend: {}
  fixed:
    ipam:
      config:
        - subnet: 172.30.0.0/24
  outside:
    external: true
`

// TestConvertNetworkSubnetOption proves Options.Subnets reaches only a network the file leaves
// without an ipam block: the default network and a declared one with none get the subnet; an
// explicit ipam block and an external network keep what the file says.
func TestConvertNetworkSubnetOption(t *testing.T) {
	t.Parallel()
	loaded, err := stacks.Load(context.Background(), stacks.LoadInput{
		Name: "shop", Target: stacks.TargetSwarm,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(subnetCompose)}}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	opts := convert.Options{StackName: "shop", StackID: "id-shop", Subnets: map[string]string{
		"default": "10.0.2.0/26", "backend": "10.0.2.64/26", "fixed": "10.0.9.0/26", "outside": "10.0.9.64/26",
	}}
	got, err := convert.Convert(loaded.Project, opts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	subnets := map[string][]convert.Pool{}
	for _, n := range got.Networks {
		subnets[n.Key] = n.Pools
	}
	want := map[string][]convert.Pool{
		"default": {{Subnet: "10.0.2.0/26"}},
		"backend": {{Subnet: "10.0.2.64/26"}},
		"fixed":   {{Subnet: "172.30.0.0/24"}},
		"outside": nil,
	}
	for key, w := range want {
		g := subnets[key]
		if len(g) != len(w) || (len(w) > 0 && g[0] != w[0]) {
			t.Errorf("network %s pools = %+v, want %+v", key, g, w)
		}
	}

	// Without the option nothing changes: no network gets a pool of its own making.
	opts.Subnets = nil
	plain, err := convert.Convert(loaded.Project, opts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	for _, n := range plain.Networks {
		if n.Key != "fixed" && len(n.Pools) != 0 {
			t.Errorf("network %s pools = %+v with no Subnets option, want none", n.Key, n.Pools)
		}
	}
}
