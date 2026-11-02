// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// The address-space settings of one environment (plan 10-21, NET-06, D-21, D-07 as amended): the
// opt-in to small stack subnets and its size, and the pools a person enters for a standalone host.
// Both are saved against a version, so two admins never overwrite each other unseen.

const (
	problemSettingsChanged = "settings_changed"
	problemNotStandalone   = "not_standalone"
	problemPoolBaseInvalid = "pool_base_invalid"
	problemPoolSizeInvalid = "pool_size_invalid"
	problemPoolOverlap     = "pool_overlap"

	// maxAuditPoolsText bounds one list of pools in an audit row.
	maxAuditPoolsText = 512

	auditTargetEnvironment = "environment"
)

// SmallSubnetsBody is the small-subnet setting of an environment.
type SmallSubnetsBody struct {
	Enabled bool `json:"enabled" doc:"True when a stack deployed here gets a small subnet of its own for its networks"`
	Mask    int  `json:"mask" minimum:"25" maximum:"28" doc:"The size of each small subnet: 25 to 28 (a /26 holds 59 usable addresses, a /28 holds 11). Kept while the setting is off"`
}

// PoolEntryBody is one address pool a person entered for a standalone host.
type PoolEntryBody struct {
	Base string `json:"base" maxLength:"43" doc:"The pool's range as an IPv4 CIDR with no host bits set, for example 10.20.0.0/16. Anything else is 422 pool_base_invalid"`
	Size int    `json:"size" minimum:"0" maximum:"128" doc:"The mask of each network allocated from the pool: from the base's own mask to 30, else 422 pool_size_invalid"`
}

// AddressSettingsBody is the answer of the address-space settings routes.
type AddressSettingsBody struct {
	SmallSubnets SmallSubnetsBody `json:"smallSubnets"`
	Pools        []PoolEntryBody  `json:"pools" doc:"The pools entered for a standalone host, in the order entered. Empty means the host's own (default-address-pools) or Docker's default pools are used"`
	Version      int64            `json:"version" minimum:"0" doc:"How many times either setting was saved. Send it back with a save: a save made against any other version is 409 settings_changed"`
	ChangedBy    string           `json:"changedBy" doc:"The display name of whoever saved either setting last; empty when none was saved or that person is not known"`
}

// AddressSettingsOutput wraps AddressSettingsBody for huma.
type AddressSettingsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AddressSettingsBody
}

// SmallSubnetsRequest is the body of PUT /api/env/{envId}/address-space/small-subnets.
type SmallSubnetsRequest struct {
	Enabled bool  `json:"enabled" doc:"Whether small stack subnets are on"`
	Mask    int   `json:"mask" minimum:"25" maximum:"28" doc:"The size of each small subnet, 25 to 28"`
	Version int64 `json:"version" minimum:"0" doc:"The version read with the settings; a save against another version is 409 settings_changed"`
}

// SmallSubnetsInput is the path and body of the small-subnet route.
type SmallSubnetsInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      SmallSubnetsRequest
}

// AddressPoolsRequest is the body of PUT /api/env/{envId}/address-space/pools.
type AddressPoolsRequest struct {
	Pools   []PoolEntryBody `json:"pools" maxItems:"16" doc:"The pools of the host, up to 16, replacing the ones saved. An empty list goes back to the host's own or Docker's default pools"`
	Version int64           `json:"version" minimum:"0" doc:"The version read with the settings; a save against another version is 409 settings_changed"`
}

// AddressPoolsInput is the path and body of the pools route.
type AddressPoolsInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      AddressPoolsRequest
}

// registerAddressSettings registers the address-space settings routes. RegisterAddressSpace calls it.
func registerAddressSettings(r *httpx.Router, d AddressSpaceDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getAddressSettings", Method: http.MethodGet, Path: "/api/env/{envId}/address-space/settings",
		Summary: "The address-space settings of an environment",
		Description: "Whether small stack subnets are on and their size (off at /26 by default), the pools entered for a standalone host (none by default), " +
			"the version of the two and the display name of whoever saved them last. Needs networks.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*AddressSettingsOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			row, err := d.Store.RQ.GetAddressSettings(ctx, env.ID)
			if err != nil {
				return nil, addressSettingsError(d, env.ID, "read the settings", err)
			}
			return &AddressSettingsOutput{CacheControl: "no-store", Body: addressSettingsBody(ctx, d, row)}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "setSmallSubnets", Method: http.MethodPut, Path: "/api/env/{envId}/address-space/small-subnets",
		Summary: "Turn small stack subnets on or off, and choose their size",
		Description: "mask is 25 to 28. version is the one read with the settings: a save made against any other version is 409 settings_changed and names who saved last. " +
			"A save that changes nothing answers 200 and writes nothing. " +
			"Needs networks.addressing (Env Admin) on the environment. A change is audited as environment.small_subnets_changed, with the new and the previous values.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksAddressing, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SmallSubnetsInput) (*AddressSettingsOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Store == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := setSmallSubnets(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &AddressSettingsOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "setAddressPools", Method: http.MethodPut, Path: "/api/env/{envId}/address-space/pools",
		Summary: "Enter the address pools of a standalone host",
		Description: "An override for a standalone host whose daemon's pools TierPlan cannot read or should not trust: when pools are saved they replace what the daemon reports, and the rows read source entered (always estimated). " +
			"At most 16 pools. Each base is an IPv4 CIDR with no host bits set (422 pool_base_invalid) and each size runs from the base's own mask to 30 (422 pool_size_invalid); no two pools may overlap (422 pool_overlap). " +
			"Each 422 names the row in its error location. A swarm has its own pools, so it is 409 not_standalone. An empty list removes the override. " +
			"version is the one read with the settings: a save made against any other version is 409 settings_changed and names who saved last. A save that changes nothing answers 200 and writes nothing. " +
			"Needs networks.addressing (Env Admin) on the environment. A change is audited as environment.address_pools_saved, with the new and the previous pools.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksAddressing, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *AddressPoolsInput) (*AddressSettingsOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.Store == nil || d.Audit == nil || d.Clusters == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			swarm, err := d.Clusters.IsSwarm(ctx, env.ID)
			if err != nil {
				return nil, snapshotProblem(d.Log, env.ID, err)
			}
			if swarm {
				return nil, httpx.Problem(http.StatusConflict, problemNotStandalone, "A swarm has its own address pools. They are set when the swarm is created, so they cannot be entered here.")
			}
			body, err := setAddressPools(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &AddressSettingsOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// addressSettingsBody is the wire form of the stored settings. The name of whoever saved last is
// looked up, and is empty when that person is gone.
func addressSettingsBody(ctx context.Context, d AddressSpaceDeps, row sqlc.GetAddressSettingsRow) AddressSettingsBody {
	body := AddressSettingsBody{
		SmallSubnets: SmallSubnetsBody{Enabled: row.SmallSubnets != 0, Mask: int(row.SmallSubnetMask)},
		Pools:        []PoolEntryBody{},
		Version:      row.AddressSettingsVersion,
	}
	for _, p := range envs.DecodeEnteredPools(row.AddressPools) {
		body.Pools = append(body.Pools, PoolEntryBody{Base: p.Base.String(), Size: p.Size})
	}
	if row.AddressSettingsBy != "" {
		body.ChangedBy = keeperName(ctx, d, map[string]string{}, row.AddressSettingsBy)
	}
	return body
}

// settingsConflict is the answer to a save made against a version that is no longer the stored one.
func settingsConflict(ctx context.Context, d AddressSpaceDeps, row sqlc.GetAddressSettingsRow) error {
	who := ""
	if row.AddressSettingsBy != "" {
		who = keeperName(ctx, d, map[string]string{}, row.AddressSettingsBy)
	}
	detail := "These settings were changed by someone else. Reload them and try again."
	if who != "" {
		detail = "These settings were changed by " + who + ". Reload them and try again."
	}
	return &huma.ErrorModel{
		Type: problemSettingsChanged, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: "changedBy", Message: who}},
	}
}

// setSmallSubnets saves the opt-in and its size. The old values are read, the new ones written and
// the audit row recorded in one transaction. A save that changes nothing writes nothing.
func setSmallSubnets(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *SmallSubnetsInput) (AddressSettingsBody, error) {
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	old, err := q.GetAddressSettings(ctx, envID)
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "read the settings", err)
	}
	if in.Body.Version != old.AddressSettingsVersion {
		return AddressSettingsBody{}, settingsConflict(ctx, d, old)
	}
	if (old.SmallSubnets != 0) == in.Body.Enabled && int(old.SmallSubnetMask) == in.Body.Mask {
		return addressSettingsBody(ctx, d, old), nil
	}
	n, err := q.SetSmallSubnets(ctx, sqlc.SetSmallSubnetsParams{
		SmallSubnets: boolInt(in.Body.Enabled), SmallSubnetMask: int64(in.Body.Mask), AddressSettingsBy: p.ID,
		UpdatedAt: d.Now().UTC().UnixNano(), ID: envID, AddressSettingsVersion: old.AddressSettingsVersion,
	})
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "save the setting", err)
	}
	if n == 0 {
		return AddressSettingsBody{}, settingsConflict(ctx, d, old)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionEnvironmentSmallSubnetsChanged, TargetType: auditTargetEnvironment, TargetID: envID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{
			"enabled": strconv.FormatBool(in.Body.Enabled), "mask": strconv.Itoa(in.Body.Mask),
			"previous_enabled": strconv.FormatBool(old.SmallSubnets != 0), "previous_mask": strconv.FormatInt(old.SmallSubnetMask, 10),
		},
	}); err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "commit", err)
	}
	old.SmallSubnets, old.SmallSubnetMask = boolInt(in.Body.Enabled), int64(in.Body.Mask)
	old.AddressSettingsVersion++
	old.AddressSettingsBy = p.ID
	return addressSettingsBody(ctx, d, old), nil
}

// boolInt is the stored form of a flag.
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// addressSettingsError maps a failed read of the settings: an environment that is not there is the
// environment's 404, anything else is logged and answered as an internal error.
func addressSettingsError(d AddressSpaceDeps, envID, what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return httpx.Problem(http.StatusNotFound, "environment_not_found", "This environment does not exist.")
	}
	d.Log.Error("address settings request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// validatePools checks the pools a person entered and returns them as the reader counts them. Every
// row is checked on its own first (base, then size), then the rows against each other, and the first
// failure is a 422 that names the row (T-10-53).
func validatePools(in []PoolEntryBody) ([]netspace.Pool, error) {
	pools := make([]netspace.Pool, 0, len(in))
	for i, e := range in {
		base, err := netip.ParsePrefix(strings.TrimSpace(e.Base))
		if err != nil || !base.Addr().Is4() || base != base.Masked() {
			return nil, poolProblem(problemPoolBaseInvalid, i, "base",
				fmt.Sprintf("Pool %d: the base must be an IPv4 range with no host bits set, such as 10.20.0.0/16.", i+1))
		}
		pool := netspace.Pool{Base: base, Size: e.Size}
		if !pool.Valid() {
			return nil, poolProblem(problemPoolSizeInvalid, i, "size",
				fmt.Sprintf("Pool %d: the size must be from /%d (the base's own mask) to /30.", i+1, base.Bits()))
		}
		pools = append(pools, pool)
	}
	for j := range pools {
		for i := range j {
			if pools[i].Base.Overlaps(pools[j].Base) {
				return nil, poolProblem(problemPoolOverlap, j, "base",
					fmt.Sprintf("Pool %d overlaps pool %d (%s and %s).", j+1, i+1, pools[j].Base, pools[i].Base))
			}
		}
	}
	return pools, nil
}

// poolProblem is the 422 for one row of the pools: the code is the problem type and the row (counted
// from 0) and the field are in the error location.
func poolProblem(code string, row int, field, detail string) error {
	return &huma.ErrorModel{
		Type: code, Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Detail: detail,
		Errors: []*huma.ErrorDetail{{Location: fmt.Sprintf("body.pools[%d].%s", row, field), Message: code, Value: row}},
	}
}

// poolsText is a list of pools as an audit row shows it: base and size, comma separated, cut to what
// an audit detail may hold.
func poolsText(pools []netspace.Pool) string {
	parts := make([]string, 0, len(pools))
	for _, p := range pools {
		parts = append(parts, p.Base.String()+"/"+strconv.Itoa(p.Size))
	}
	text := strings.Join(parts, ",")
	if len(text) > maxAuditPoolsText {
		text = text[:maxAuditPoolsText]
	}
	return text
}

// setAddressPools saves the entered pools. The old list is read, the new one written and the audit row
// recorded in one transaction, and the reader's last answer is dropped so the next read uses the new
// pools. A save that changes nothing writes nothing.
func setAddressPools(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *AddressPoolsInput) (AddressSettingsBody, error) {
	pools, err := validatePools(in.Body.Pools)
	if err != nil {
		return AddressSettingsBody{}, err
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	old, err := q.GetAddressSettings(ctx, envID)
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "read the settings", err)
	}
	if in.Body.Version != old.AddressSettingsVersion {
		return AddressSettingsBody{}, settingsConflict(ctx, d, old)
	}
	stored := envs.EncodeEnteredPools(pools)
	if stored == envs.EncodeEnteredPools(envs.DecodeEnteredPools(old.AddressPools)) {
		return addressSettingsBody(ctx, d, old), nil
	}
	n, err := q.SetAddressPools(ctx, sqlc.SetAddressPoolsParams{
		AddressPools: stored, AddressSettingsBy: p.ID, UpdatedAt: d.Now().UTC().UnixNano(), ID: envID, AddressSettingsVersion: old.AddressSettingsVersion,
	})
	if err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "save the pools", err)
	}
	if n == 0 {
		return AddressSettingsBody{}, settingsConflict(ctx, d, old)
	}
	if err := d.Audit.RecordTx(ctx, tx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionEnvironmentAddressPoolsSaved, TargetType: auditTargetEnvironment, TargetID: envID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{"pools": poolsText(pools), "previous_pools": poolsText(envs.DecodeEnteredPools(old.AddressPools))},
	}); err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return AddressSettingsBody{}, addressSettingsError(d, envID, "commit", err)
	}
	if d.AddressSpaces != nil {
		d.AddressSpaces.Invalidate(envID)
	}
	old.AddressPools = stored
	old.AddressSettingsVersion++
	old.AddressSettingsBy = p.ID
	return addressSettingsBody(ctx, d, old), nil
}
