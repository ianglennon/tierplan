// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The pools and overlay networks near exhaustion across the fleet (plan 12-28, ENV-10, D-17): every
// meter at 70% or more used, by the rules and the words of the address space pages (P10).

const (
	poolGettingFull = "getting_full"
	poolAlmostFull  = "almost_full"
)

// FleetPoolItem is one address pool or overlay network at 70% or more used.
type FleetPoolItem struct {
	EnvID     string `json:"envId" doc:"The environment it is in"`
	EnvName   string `json:"envName" doc:"The name of that environment"`
	Kind      string `json:"kind" enum:"pool,ingress,overlay" doc:"pool for an address pool, ingress for the swarm's routing mesh network, overlay for any other overlay network"`
	Name      string `json:"name" doc:"The pool's CIDR, or the network's name"`
	Subnet    string `json:"subnet" doc:"The pool's CIDR, or the network's IPv4 subnets, comma separated"`
	Used      uint64 `json:"used" minimum:"0" doc:"What is used, in the unit given"`
	Capacity  uint64 `json:"capacity" minimum:"0" doc:"What there is, in the unit given"`
	Percent   int    `json:"percent" minimum:"0" doc:"Whole percent used, rounded down"`
	State     string `json:"state" enum:"getting_full,almost_full" doc:"getting_full from 70% up to 90%, almost_full from 90%"`
	Unit      string `json:"unit" enum:"subnets,addresses" doc:"subnets for a pool, addresses for a network"`
	Estimated bool   `json:"estimated" doc:"True for a standalone host's pool, whose number is a guess from Docker's defaults"`
}

// FleetPoolsBody is the answer of GET /api/fleet/pools.
type FleetPoolsBody struct {
	Items       []FleetPoolItem        `json:"items" doc:"Every pool and overlay network at 70% or more used in the environments that were read, fullest first, then by environment name, then by name"`
	NotIncluded []FleetNotIncludedBody `json:"notIncluded" doc:"The environments the caller may read whose address space could not be read. Environments the caller may not read are never named"`
	ReadAt      string                 `json:"readAt" format:"date-time" doc:"When the answer was put together"`
}

// FleetPoolsOutput wraps FleetPoolsBody for huma.
type FleetPoolsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         FleetPoolsBody
}

// FleetPoolsInput is the query of GET /api/fleet/pools.
type FleetPoolsInput struct {
	Env []string `query:"env,explode" maxItems:"100" doc:"Only these environments. An ID the caller may not read is ignored, as if it were not given. Empty means every environment the caller may read"`
}

// fleetPoolsOf is the meters of an environment's address space at the warning threshold or above.
func fleetPoolsOf(env envs.Environment, space envs.AddressSpace) []FleetPoolItem {
	var out []FleetPoolItem
	for _, m := range envs.MetersOf(space) {
		if m.Percent < netspace.ThresholdWarn {
			continue
		}
		state := poolGettingFull
		if m.Percent >= netspace.ThresholdFull {
			state = poolAlmostFull
		}
		out = append(out, FleetPoolItem{
			EnvID: env.ID, EnvName: env.Name, Kind: m.Kind, Name: m.Name, Subnet: fleetSubnetOf(space, m),
			Used: m.Used, Capacity: m.Capacity, Percent: m.Percent, State: state, Unit: m.Unit, Estimated: m.Estimated,
		})
	}
	return out
}

// fleetSubnetOf is the subnet of a meter: a pool is its own CIDR, a network has its IPv4 subnets.
func fleetSubnetOf(space envs.AddressSpace, m envs.WorstMeter) string {
	if m.Kind == envs.MeterPool {
		return m.Name
	}
	for _, o := range space.Overlays {
		if o.Name == m.Name && o.Ingress == (m.Kind == envs.MeterIngress) {
			return strings.Join(o.Subnets, ", ")
		}
	}
	return ""
}

// fleetPools puts the pools of the results that were read in order: the fullest first (by percent),
// then by environment name, name, kind and environment ID, so the same input gives one order.
func fleetPools(results []fleetResult[[]FleetPoolItem]) []FleetPoolItem {
	items := []FleetPoolItem{}
	for _, r := range results {
		if r.Err == nil {
			items = append(items, r.Val...)
		}
	}
	slices.SortFunc(items, func(a, b FleetPoolItem) int {
		return cmp.Or(
			-cmp.Compare(a.Percent, b.Percent), strings.Compare(a.EnvName, b.EnvName), strings.Compare(a.Name, b.Name),
			strings.Compare(a.Kind, b.Kind), strings.Compare(a.EnvID, b.EnvID),
		)
	})
	return items
}

// registerFleetPools registers the fleet's pool route on r.
func registerFleetPools(r *httpx.Router, d FleetDeps, fail func(what string, err error) error) {
	httpx.Register(r, huma.Operation{
		OperationID: "listFleetPools",
		Method:      http.MethodGet,
		Path:        "/api/fleet/pools",
		Summary:     "List the address pools and overlay networks near exhaustion across the environments the caller may read",
		Description: "Every address pool and overlay network at 70% or more used (getting_full up to 90%, almost_full from 90%, by the whole percent rounded down) " +
			"in the environments the caller holds environments.read on, fullest first, then by environment name and network name. A standalone host's pool is a guess and says so (estimated). " +
			"An environment whose address space could not be read is named in notIncluded; one the caller may not read is neither listed nor named. At most 8 environments are read at once, each for at most 5 seconds.",
		Tags: []string{"fleet"},
	}, rbac.SelfService, func(ctx context.Context, in *FleetPoolsInput) (*FleetPoolsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.AddressSpaces == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, "", "Address pools are not available on this server.")
		}
		results, err := fleetFanOut(ctx, d, p, in.Env, func(ctx context.Context, env envs.Environment) ([]FleetPoolItem, int, error) {
			space, err := d.AddressSpaces.Read(ctx, env.ID)
			switch {
			case err != nil:
				return nil, 0, err
			case space.NotMeasured != "":
				return nil, 0, errors.New(space.NotMeasured)
			}
			return fleetPoolsOf(env, space), len(space.NotReporting), nil
		})
		if err != nil {
			return nil, fail("read the fleet's pools", err)
		}
		return &FleetPoolsOutput{CacheControl: "no-store", Body: FleetPoolsBody{
			Items: fleetPools(results), NotIncluded: fleetNotIncluded(results), ReadAt: rfc3339(d.now()),
		}}, nil
	})
}
