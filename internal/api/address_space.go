// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/store"
)

// AddressSpaceDeps are the dependencies of the address-space routes of the networks phase.
// Every dependency the phase's routes use is here, so a later route is added without new
// wiring.
type AddressSpaceDeps struct {
	// AddressSpaces reads the pools of an environment and how full each is.
	AddressSpaces *envs.AddressSpaces
	// Aggregator and Clusters reach the engines of an environment.
	Aggregator *envs.Aggregator
	Clusters   *envs.Clusters
	// Store holds what the phase persists beside Docker's own state.
	Store *store.Store
	Audit *audit.Writer
	// Checker says what the caller may do beyond the permission of a route.
	Checker *rbac.Checker
	// Now is the clock; it defaults to time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// AddressSpaceOutput wraps AddressSpaceBody for huma.
type AddressSpaceOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AddressSpaceBody
}

// AddressSpaceBody is the answer of GET /api/env/{envId}/address-space.
type AddressSpaceBody struct {
	EnvType      string            `json:"envType" enum:"swarm,standalone" doc:"What the environment is"`
	Measured     bool              `json:"measured" doc:"True when the numbers were read from the swarm's own pools through a manager. False for a standalone host, whose rows are all estimated, and when notMeasured is set"`
	NotMeasured  string            `json:"notMeasured,omitempty" enum:"no_manager" doc:"Why a swarm could not be measured: no_manager means no manager answered. The pools last read are returned with state unknown and nothing counted. Absent when the swarm was measured or the environment is standalone"`
	Pools        []AddressPoolBody `json:"pools" doc:"One row per address pool, in the order the swarm or the daemon lists them. Empty when a swarm was never measured"`
	NotReporting []string          `json:"notReporting" doc:"The nodes that did not answer, whose networks are left out of the counts. Empty when every node answered"`
}

// AddressPoolBody is one address pool and how much of it is in use.
type AddressPoolBody struct {
	CIDR               string `json:"cidr" doc:"The pool's range, for example 10.0.0.0/8"`
	Mask               int    `json:"mask" minimum:"1" maximum:"30" doc:"The mask of each network allocated from the pool (the swarm's SubnetSize, or the daemon's pool size)"`
	Used               int64  `json:"used" minimum:"0" doc:"How many blocks of the pool, each of the size the mask gives, any subnet overlaps. A /28 inside one /26 block uses 1; a /16 in a /24-size pool uses 256. IPv6 subnets are not counted. At most 2^30, so it always fits"`
	Capacity           int64  `json:"capacity" minimum:"0" doc:"How many blocks the pool holds in all (at most 2^30). Zero cannot happen for a listed pool"`
	Percent            int    `json:"percent" minimum:"0" maximum:"100" doc:"used over capacity as a whole percent, rounded down, so 69.99% is 69"`
	State              string `json:"state" enum:"normal,getting_full,almost_full,unknown" doc:"normal under 70%, getting_full from 70% and almost_full from 90%, decided on the exact ratio and never on the rounded percent. unknown when the pool could not be measured"`
	LargestFree        string `json:"largestFree" doc:"The largest free block made of whole blocks of the pool, aligned to the pool, in CIDR form. Empty when no block is free or the pool was not measured"`
	LargestFreeSubnets int64  `json:"largestFreeSubnets" minimum:"0" doc:"How many blocks of the pool's mask largestFree holds. 0 with an empty largestFree"`
	Source             string `json:"source" enum:"swarm,daemon,defaults,entered" doc:"Where the pool came from: swarm (the swarm's own, or its built-in 10.0.0.0/8), daemon (the host's default-address-pools setting), defaults (Docker's built-in pools, because the host sets none) or entered (a pool a person entered)"`
	Estimated          bool   `json:"estimated" doc:"True when the numbers cannot be taken as exact. Every standalone row is: the daemon skips pool slots that overlap routes of the host, and engines differ"`
}

// RegisterAddressSpace registers the address-space routes. Nothing here is cached beyond the
// server's short read of the engines: the numbers are read live (the browser is told not to
// keep them).
func RegisterAddressSpace(r *httpx.Router, d AddressSpaceDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	httpx.Register(r, huma.Operation{
		OperationID: "getAddressSpace", Method: http.MethodGet, Path: "/api/env/{envId}/address-space",
		Summary: "The address pools of an environment and how full each is",
		Description: "One row per address pool. A swarm answers the pools its manager reports (Swarm.Cluster.DefaultAddrPool of the engine info; none reported means the swarm's built-in 10.0.0.0/8) at the swarm's subnet size, measured: " +
			"every network of the cluster, ingress included when it lies inside a pool, and every node's local network that overlaps a pool is counted. " +
			"A standalone host answers the pools its daemon reports (source daemon) or, when it reports none, Docker's built-in pools (source defaults), always estimated. " +
			"used and capacity count blocks of the pool's mask; state is decided on the exact ratio (normal under 70%, getting_full from 70%, almost_full from 90%) and percent is that ratio rounded down. " +
			"largestFree is the largest aligned free block and largestFreeSubnets how many blocks it holds. " +
			"When no manager of a swarm answers, notMeasured is no_manager and the pools last read are returned with state unknown and nothing counted; notReporting names the nodes that did not answer. " +
			"The server reads the engines at most once in 5 seconds per environment; the answer is never cached by the browser. IPv4 only. " +
			"Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*AddressSpaceOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.AddressSpaces == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			space, err := d.AddressSpaces.Read(ctx, env.ID)
			if err != nil {
				return nil, snapshotProblem(d.Log, env.ID, err)
			}
			return &AddressSpaceOutput{CacheControl: "no-store", Body: addressSpaceBody(space)}, nil
		})
}

// addressSpaceBody is the wire form of an environment's address space.
func addressSpaceBody(s envs.AddressSpace) AddressSpaceBody {
	body := AddressSpaceBody{
		EnvType: "standalone", Measured: s.Measured, NotMeasured: s.NotMeasured,
		Pools: make([]AddressPoolBody, 0, len(s.Pools)), NotReporting: s.NotReporting,
	}
	if s.Swarm {
		body.EnvType = "swarm"
	}
	if body.NotReporting == nil {
		body.NotReporting = []string{}
	}
	for _, p := range s.Pools {
		row := AddressPoolBody{
			CIDR: p.Pool.Base.String(), Mask: p.Pool.Size,
			Used: blocks(p.Used), Capacity: blocks(p.Capacity), Percent: p.Percent, State: string(p.State),
			LargestFreeSubnets: blocks(p.LargestFreeBlocks), Source: p.Source, Estimated: p.Estimated,
		}
		if p.LargestFree.IsValid() {
			row.LargestFree = p.LargestFree.String()
		}
		body.Pools = append(body.Pools, row)
	}
	return body
}

// maxBlocks is the most blocks a pool can hold: a /0 base cut into /30 blocks is 2^30. Every
// count of a valid pool is at most this, which is why it fits an int64 and a JSON number.
const maxBlocks = 1 << 30

// blocks is a count of blocks as the wire number, held to maxBlocks.
func blocks(n uint64) int64 { return int64(min(n, maxBlocks)) } //nolint:gosec // G115: held to 2^30 on this line
