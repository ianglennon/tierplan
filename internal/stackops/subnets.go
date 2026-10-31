// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/stacks/convert"
)

// CodeNoFreeSubnet is the code of a deploy or review that cannot give a new network the block of the
// size the environment chose: no block of that size is free in the swarm's pools (D-20, NET-06).
const CodeNoFreeSubnet = "small_subnet_no_free_block"

// SubnetStore keeps the subnets a stack's deploys gave its networks, per revision. Repo is the one
// implementation.
type SubnetStore interface {
	// GoodSubnets is what the stack's newest good revision recorded, by network name.
	GoodSubnets(ctx context.Context, stackID string) (map[string]string, error)
	// SetSubnets stores on a revision the subnets its deploy gave the networks it created.
	SetSubnets(ctx context.Context, snapshotID string, subnets map[string]string) error
}

var _ SubnetStore = Repo{}

// SubnetPlan is the subnets small stack subnets give the new networks of a stack. ByKey is by the
// network's key in the compose file (convert.Options.Subnets) and ByName by its name on the swarm.
// Missing names the networks no block was free for. The zero value gives nothing to anyone.
type SubnetPlan struct {
	ByKey  map[string]string
	ByName map[string]string
	// Missing are the names of the networks that got no block, in key order.
	Missing []string
	// Mask is the size asked for and Pools the pools searched, worded for a message.
	Mask  int
	Pools string

	world  *subnetWorld
	unlock func()
}

// subnetWorld is what choosing the next block looks at: the pools, what is taken, and what must be
// avoided. Every pick is added to occupied, so the next one clears it.
type subnetWorld struct {
	pools    []netspace.Pool
	occupied []netip.Prefix
	avoid    []netip.Prefix
	mask     int
}

// Release gives the allocation lock back. It is safe to call more than once and on a plan that holds
// none.
func (p *SubnetPlan) Release() {
	if p != nil && p.unlock != nil {
		p.unlock()
		p.unlock = nil
	}
}

// NoFreeSubnetMessage is the sentence for a network that got no block (UI-SPEC default 19).
func NoFreeSubnetMessage(network string, mask int, pools string) string {
	return fmt.Sprintf("network %s can't get a /%d subnet: no free block is left in %s. Free up space, or choose a smaller size in this environment's settings.", network, mask, pools)
}

// Message is the sentence for one of the Missing networks.
func (p SubnetPlan) Message(network string) string {
	return NoFreeSubnetMessage(network, p.Mask, p.Pools)
}

// planSmallSubnets decides the subnet of each network the stack is about to create: one the planned
// list holds that is not external, has no ipam config of its own and is not on the swarm yet (live),
// taken in key order. Each gets the subnet recorded for it when that is still free, else the lowest
// free block of the environment's chosen size inside the swarm's pools, clear of every IPv4 subnet of
// every network on every node, the hosts' routes and the picks before it. A network that exists, an
// explicit ipam block and an external network are never touched. With the setting off, or nothing to
// give a subnet to, the plan is empty and nothing is read.
//
// A deploy (deploy true) holds the environment's allocation lock from here until the plan's Release,
// reads every node's networks afresh and so cannot give a block a network made through Coaming a
// moment ago holds. A review (deploy false) holds nothing and reads the cached view.
func (d Deps) planSmallSubnets(ctx context.Context, envID string, planned []convert.NetworkPlan, live []dockerx.NetworkRow, recorded map[string]string, deploy bool) (SubnetPlan, error) {
	if d.AddressSpaces == nil {
		return SubnetPlan{}, nil
	}
	enabled, mask := d.AddressSpaces.SmallSubnetSetting(ctx, envID)
	if !enabled || mask < 25 || mask > 28 {
		return SubnetPlan{}, nil
	}
	var todo []convert.NetworkPlan
	for _, n := range planned {
		if n.External || len(n.Pools) > 0 {
			continue
		}
		if _, found := findNetwork(live, n.Name); found {
			continue
		}
		todo = append(todo, n)
	}
	if len(todo) == 0 {
		return SubnetPlan{}, nil
	}
	var unlock func()
	if deploy {
		unlock = d.AddressSpaces.LockAllocation(envID)
	}
	in, err := d.AddressSpaces.AllocationInputs(ctx, envID, deploy)
	if err != nil {
		if unlock != nil {
			unlock()
		}
		return SubnetPlan{}, err
	}
	if len(in.Pools) == 0 {
		if unlock != nil {
			unlock()
		}
		return SubnetPlan{}, envs.ErrNoManager
	}
	w := &subnetWorld{pools: in.Pools, occupied: in.Occupied, avoid: in.Avoid, mask: mask}
	out := SubnetPlan{
		ByKey: map[string]string{}, ByName: map[string]string{}, Mask: mask, Pools: envs.PoolList(in.Pools),
		world: w, unlock: unlock,
	}
	for _, n := range todo {
		block, ok := w.reuse(recorded[n.Name])
		if !ok {
			block, ok = w.lowest()
		}
		if !ok {
			out.Missing = append(out.Missing, n.Name)
			continue
		}
		w.occupied = append(w.occupied, block)
		out.ByKey[n.Key], out.ByName[n.Name] = block.String(), block.String()
	}
	return out, nil
}

// reuse is the recorded block, when it is a valid IPv4 block inside a pool and still free.
func (w *subnetWorld) reuse(recorded string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(recorded)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return netip.Prefix{}, false
	}
	inPool := false
	for _, pool := range w.pools {
		if pool.Base.Masked().Bits() <= p.Bits() && pool.Base.Masked().Contains(p.Addr()) {
			inPool = true
			break
		}
	}
	if !inPool {
		return netip.Prefix{}, false
	}
	for _, list := range [][]netip.Prefix{w.occupied, w.avoid} {
		for _, o := range list {
			if o.Addr().Is4() && o.Overlaps(p) {
				return netip.Prefix{}, false
			}
		}
	}
	return p, true
}

// lowest is the lowest free block of the world's size.
func (w *subnetWorld) lowest() (netip.Prefix, bool) {
	block, _, err := netspace.LowestFree(w.pools, w.occupied, w.avoid, w.mask)
	return block, err == nil
}

// reassign picks another block for a network whose create the engine refused as overlapping: the
// refused block is taken from now on. It reports false when no other block is free.
func (p *SubnetPlan) reassign(key, name string, refused netip.Prefix) (string, bool) {
	if p == nil || p.world == nil {
		return "", false
	}
	p.world.occupied = append(p.world.occupied, refused)
	block, ok := p.world.lowest()
	if !ok {
		return "", false
	}
	p.world.occupied = append(p.world.occupied, block)
	p.ByKey[key], p.ByName[name] = block.String(), block.String()
	return block.String(), true
}

// assigned reports whether the subnet of n is one this plan chose, and the block.
func (p *SubnetPlan) assigned(n convert.NetworkPlan) (netip.Prefix, bool) {
	if p == nil || len(n.Pools) != 1 || p.ByName[n.Name] == "" || p.ByName[n.Name] != n.Pools[0].Subnet {
		return netip.Prefix{}, false
	}
	b, err := netip.ParsePrefix(n.Pools[0].Subnet)
	return b, err == nil
}

// isOverlapRefusal reports whether the engine refused a network create because its subnet overlaps
// another pool.
func isOverlapRefusal(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "overlap")
}

// subnetsToRecord is what a revision records after its deploy: the subnets the deploy chose, and
// those recorded before for networks that still exist, as the swarm has them now.
func subnetsToRecord(recorded map[string]string, live []dockerx.NetworkRow, chosen map[string]string) map[string]string {
	out := map[string]string{}
	for name := range recorded {
		row, ok := findNetwork(live, name)
		if !ok {
			continue
		}
		for _, raw := range row.Subnets {
			if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Is4() {
				out[name] = p.Masked().String()
				break
			}
		}
	}
	for name, subnet := range chosen {
		out[name] = subnet
	}
	return out
}
