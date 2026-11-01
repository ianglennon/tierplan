// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/stacks"
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

// reviewSubnets runs the planner a deploy runs, without the lock and on the cached view, so a review
// shows the blocks the deploy will then use. A swarm whose pools cannot be read leaves the review with
// no subnet to show and logs why; the deploy itself refuses in that case.
func (d Deps) reviewSubnets(ctx context.Context, envID string, stack Stack, plan convert.Plan, live liveStack, recorded map[string]string) SubnetPlan {
	nets := live.Networks
	if hasExternal(plan) {
		nets = live.AllNetworks
	}
	out, err := d.planSmallSubnets(ctx, envID, plan.Networks, nets, recorded, false)
	if err != nil {
		d.log().Debug("the subnets of a stack's new networks could not be planned for a review", "environment", envID, "stack", stack.Name, "error", err)
		return SubnetPlan{}
	}
	return out
}

// subnetProblems are the blocking problems for the networks no block was free for, one each, keyed by the
// network's section of the file (UI-SPEC default 19).
func subnetProblems(plan convert.Plan, sub SubnetPlan) stacks.Problems {
	var out stacks.Problems
	for _, name := range sub.Missing {
		key := ""
		for _, n := range plan.Networks {
			if n.Name == name {
				key = "networks." + n.Key
				break
			}
		}
		out = append(out, stacks.Problem{
			Severity: stacks.SeverityError, Code: CodeNoFreeSubnet, Message: sub.Message(name), File: stacks.ComposeFile, Key: key,
			Line: 1, Col: 1, EndLine: 1, EndCol: 2,
		})
	}
	return out
}

// standaloneSubnetEligible reports whether small stack subnets may give a project network a subnet: a network
// Coaming's deploy creates, on the default bridge driver, with no address configuration of its own (D-20, P7 D-18).
func standaloneSubnetEligible(n types.NetworkConfig) bool {
	if bool(n.External) || n.Name == "" || len(n.Ipam.Config) > 0 || (n.Ipam.Driver != "" && n.Ipam.Driver != "default") {
		return false
	}
	if n.Driver != "" && n.Driver != "bridge" {
		return false
	}
	if n.EnableIPv6 != nil && *n.EnableIPv6 {
		return false
	}
	return n.EnableIPv4 == nil || *n.EnableIPv4
}

// recordedBlock is the recorded subnet when it is a masked IPv4 prefix, else "".
func recordedBlock(raw string) string {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return ""
	}
	return p.String()
}

// hashedSubnet is the IPv4 subnet of a network the host holds whose Compose config-hash label is the hash of the
// project network with exactly that subnet and not the hash of it as the file declares it: a network an earlier
// deploy made with a subnet that no good revision recorded (the deploy failed after the network was created). Giving
// the project network the subnet is what keeps the next up from recreating it. A network with no label, or whose
// label matches the plain network, has none.
func hashedSubnet(n types.NetworkConfig, row dockerx.NetworkRow) (string, bool) {
	have := row.Labels[stacks.LabelComposeConfigHash]
	if have == "" {
		return "", false
	}
	if plain, err := dockerx.ComposeNetworkHash(&n); err != nil || plain == have {
		return "", false
	}
	for _, raw := range row.Subnets {
		p, err := netip.ParsePrefix(raw)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		with := n
		with.Ipam.Config = []*types.IPAMPool{{Subnet: p.Masked().String()}}
		if h, err := dockerx.ComposeNetworkHash(&with); err == nil && h == have {
			return p.Masked().String(), true
		}
	}
	return "", false
}

// planStandaloneSubnets decides the subnet of each network of a standalone stack's project that gets one (D-20,
// D-22), in key order, for a network that is not external, declares no address configuration and uses the default
// driver:
//
//   - one the host holds and a revision recorded a subnet for is given exactly that subnet, whether the setting is
//     still on or not: the Compose SDK stamps a hash of the whole network on it, and a network whose configuration
//     differs from the label is recreated under its running containers;
//   - one the host holds with no record is left alone (its hash was computed without a subnet), unless its hash
//     label proves it was made with one (hashedSubnet);
//   - one the host does not hold takes its recorded subnet when that block is still free, else, with the setting on,
//     the lowest free block of the chosen size in the host's pools (entered, daemon or Docker's defaults), clear of
//     every network on the host, the host's routes and the picks before it.
//
// Nothing is read for a stack with nothing to decide. A deploy (deploy true) holds the allocation lock from here until
// the plan's Release and reads the host afresh; a review holds nothing and reads the cached view.
func (d Deps) planStandaloneSubnets(ctx context.Context, envID string, p *types.Project, live standaloneLive, recorded map[string]string, deploy bool) (SubnetPlan, error) {
	enabled, mask := false, 0
	if d.AddressSpaces != nil {
		enabled, mask = d.AddressSpaces.SmallSubnetSetting(ctx, envID)
		enabled = enabled && mask >= 25 && mask <= 28
	}
	out := SubnetPlan{ByKey: map[string]string{}, ByName: map[string]string{}, Mask: mask}
	type pending struct{ key, name, recorded string }
	var todo []pending
	for _, key := range sortedKeys(p.Networks) {
		n := p.Networks[key]
		if !standaloneSubnetEligible(n) {
			continue
		}
		rec := recordedBlock(recorded[n.Name])
		if row, exists := findNetwork(live.Networks, n.Name); exists {
			if rec == "" {
				rec, _ = hashedSubnet(n, row)
			}
			if rec != "" {
				out.ByKey[key], out.ByName[n.Name] = rec, rec
			}
			continue
		}
		if enabled || rec != "" {
			todo = append(todo, pending{key, n.Name, rec})
		}
	}
	if len(todo) == 0 || d.AddressSpaces == nil {
		return out, nil
	}
	var unlock func()
	if deploy {
		unlock = d.AddressSpaces.LockAllocation(envID)
	}
	release := func() {
		if unlock != nil {
			unlock()
		}
	}
	in, err := d.AddressSpaces.AllocationInputs(ctx, envID, deploy)
	if err == nil && len(in.Pools) == 0 {
		err = errors.New("the host has no address pool a network can be given a block of")
	}
	if err != nil {
		release()
		if !enabled {
			// Only recorded subnets were asked for: the network is made as it would have been without them.
			d.log().Debug("the address pools could not be read to reuse a recorded subnet", "environment", envID, "error", err)
			return out, nil
		}
		return SubnetPlan{}, err
	}
	w := &subnetWorld{pools: in.Pools, occupied: in.Occupied, avoid: in.Avoid, mask: mask}
	out.Pools, out.world, out.unlock = envs.PoolList(in.Pools), w, unlock
	for _, t := range todo {
		block, ok := w.reuse(t.recorded)
		if !ok && enabled {
			if block, ok = w.lowest(); !ok {
				out.Missing = append(out.Missing, t.name)
				continue
			}
		}
		if !ok {
			continue
		}
		w.occupied = append(w.occupied, block)
		out.ByKey[t.key], out.ByName[t.name] = block.String(), block.String()
	}
	return out, nil
}

// applyProjectSubnets gives the project networks of the plan their subnets, exactly those and nothing else: the
// compose file the person wrote is never touched, only the project handed to the Compose SDK.
func applyProjectSubnets(p *types.Project, plan SubnetPlan) {
	for key, subnet := range plan.ByKey {
		n, ok := p.Networks[key]
		if !ok {
			continue
		}
		n.Ipam.Config = []*types.IPAMPool{{Subnet: subnet}}
		p.Networks[key] = n
	}
}

// standaloneSubnetProblems are the blocking problems for the networks of a standalone project no block was free
// for, keyed by the network's section of the file (UI-SPEC default 19).
func standaloneSubnetProblems(p *types.Project, sub SubnetPlan) stacks.Problems {
	plan := convert.Plan{}
	for _, key := range sortedKeys(p.Networks) {
		plan.Networks = append(plan.Networks, convert.NetworkPlan{Key: key, Name: p.Networks[key].Name})
	}
	return subnetProblems(plan, sub)
}

// reviewStandaloneSubnets runs the planner a standalone deploy runs, without the lock and on the cached view, and
// gives the project the subnets, so a review shows the blocks the deploy will use and compares the networks the host
// holds with the configuration the deploy will send. A host whose pools cannot be read leaves the review with no
// subnet to show, and logs why; the deploy itself refuses in that case.
func (d Deps) reviewStandaloneSubnets(ctx context.Context, envID string, stack Stack, p *types.Project, live standaloneLive, recorded map[string]string) SubnetPlan {
	out, err := d.planStandaloneSubnets(ctx, envID, p, live, recorded, false)
	if err != nil {
		d.log().Debug("the subnets of a standalone stack's new networks could not be planned for a review", "environment", envID, "stack", stack.Name, "error", err)
		return SubnetPlan{}
	}
	applyProjectSubnets(p, out)
	return out
}

// standaloneNetworkRows are the review's rows for the networks of a standalone project the host does not hold yet:
// the subnet each will be created with, and whether the environment's small stack subnets chose it (D-56). A network
// whose file gives it a subnet shows that one, without the badge.
func standaloneNetworkRows(p *types.Project, live []dockerx.NetworkRow, sub SubnetPlan) []ObjectChange {
	var out []ObjectChange
	for _, name := range missingNetworks(p, live) {
		row := ObjectChange{Change: stacks.ChangeAdded, Kind: "network", Name: name}
		if chosen := sub.ByName[name]; chosen != "" {
			row.Subnet, row.SmallSubnetDefault = chosen, true
		} else {
			for _, n := range p.Networks {
				if n.Name == name && len(n.Ipam.Config) > 0 && n.Ipam.Config[0] != nil {
					row.Subnet = n.Ipam.Config[0].Subnet
				}
			}
		}
		out = append(out, row)
	}
	return out
}
