// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/netspace"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

const (
	// sourceOrphanCleanup is the source detail of what the orphan list removes; sourceJanitor is
	// the one of what the janitor removes through the same code.
	sourceOrphanCleanup = "orphan_cleanup"
	sourceJanitor       = "janitor"
	// problemInspectTooLarge: the network's masked inspect document is over what the audit log
	// stores whole, so the delete is refused (D-12).
	problemInspectTooLarge = "inspect_too_large"

	outcomeDeleted = "deleted"
	outcomeKept    = "kept"

	reasonInUseAgain  = "in_use_again"
	reasonNotOrphaned = "not_orphaned"
	reasonNodeMissing = "node_missing"

	problemKeptByLabel  = "kept_by_label"
	problemKeepNotFound = "keep_not_found"
)

// KeepTarget is one network to keep out of the orphan list.
type KeepTarget struct {
	Name   string `json:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The network's name. A keep follows the name, so it holds for a network of that name made again"`
	NodeID string `json:"nodeId,omitempty" maxLength:"128" doc:"The node a local network is on: the nodeId of its row. Left out for a swarm-wide network, and for a local network when one node has it"`
}

// KeepRequest is the body of PUT /api/env/{envId}/address-space/keeps.
type KeepRequest struct {
	Targets []KeepTarget `json:"targets" minItems:"1" maxItems:"100" doc:"The networks to keep, 1 to 100 (T-10-33)"`
}

// KeepInput is the path and body of the keep route.
type KeepInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      KeepRequest
}

// KeepBody is the answer of the keep route.
type KeepBody struct {
	Kept int `json:"kept" minimum:"0" doc:"How many networks are newly kept. One that was kept already changes and records nothing and is not counted"`
}

// KeepOutput wraps KeepBody for huma.
type KeepOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         KeepBody
}

// UnkeepInput is the path and query of the unkeep route.
type UnkeepInput struct {
	EnvironmentPathInput
	Name   string `path:"name" maxLength:"128" doc:"The network's name"`
	NodeID string `query:"nodeId" maxLength:"128" doc:"The node a local network is on: the nodeId of its row. Left out for a swarm-wide network, and for a local network when one node has it"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// KeptRowBody is one network kept out of the orphan list.
type KeptRowBody struct {
	Name       string   `json:"name"`
	Node       string   `json:"node" doc:"The node a local network is on, Cluster for a swarm-wide network, empty for a keep whose network is gone and whose node is not known"`
	NodeID     string   `json:"nodeId" doc:"The node the keep is for: the node's ID for a local network, empty for a swarm-wide one"`
	Subnets    []string `json:"subnets" doc:"The network's subnets in CIDR notation; empty when it is not present or has none"`
	KeptBy     string   `json:"keptBy" enum:"label,user" doc:"label: the network carries the tierplan.keep=true label, which only its owner can remove. user: a person kept it"`
	KeptByName string   `json:"keptByName" doc:"The display name of the person who kept it; empty when kept by label or when that person is not known"`
	KeptAt     string   `json:"keptAt,omitempty" format:"date-time" doc:"When it was kept; absent when kept by label alone"`
	Present    bool     `json:"present" doc:"True when a network of this name (and node) exists now. A keep outlives its network, so it holds when one of that name is made again"`
}

// OrphanDeleteInput is the path and query of the orphan delete route.
type OrphanDeleteInput struct {
	EnvironmentPathInput
	NetworkID string `path:"networkId" maxLength:"128" doc:"The network ID: the networkId of a row of the orphans list"`
	NodeID    string `query:"nodeId" maxLength:"128" doc:"The node a local network is on: the nodeId of its row. Ignored for a swarm-scoped network, which belongs to the cluster"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// OrphanDeleteBody is the answer of the orphan delete.
type OrphanDeleteBody struct {
	Outcome string `json:"outcome" enum:"deleted,kept" doc:"deleted: the network was removed. kept: TierPlan checked again just before and left it alone, which is a safe answer and not an error"`
	Reason  string `json:"reason" enum:"in_use_again,not_orphaned,node_missing," doc:"Why a network was kept, empty when it was deleted. in_use_again: something uses it now, or the engine refused the remove as in use. not_orphaned: its stack exists again, it was kept, or it is inside the grace period. node_missing: a node or the manager stopped answering, so TierPlan could not be sure"`
	Name    string `json:"name" doc:"The network's name"`
}

// OrphanDeleteOutput wraps OrphanDeleteBody for huma.
type OrphanDeleteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         OrphanDeleteBody
}

// registerAddressOrphans registers the routes that act on the orphan list.
func registerAddressOrphans(r *httpx.Router, d AddressSpaceDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "deleteOrphanNetwork", Method: http.MethodDelete, Path: "/api/env/{envId}/address-space/orphans/{networkId}",
		Summary: "Delete an orphaned or unused network",
		Description: "Removes one network from the orphan list, once and never retried. TierPlan first reads the environment again and judges this one network from that fresh read, " +
			"so a network that something started using, whose stack exists again, that was kept, or that sits on a node or manager that stopped answering is kept and not removed: " +
			"200 with outcome kept and the reason (in_use_again, not_orphaned or node_missing). The engine refusing the remove because the network is in use is also kept (in_use_again), not an error. " +
			"Before the remove, the network's whole inspect document as the engine returned it, with secret-like values in its labels and options masked, is written to the audit log as network.inspect_saved. " +
			"A document over 64 KB cannot be stored whole, so the delete is refused: 409 inspect_too_large and nothing is removed. A failure to write it is a 500 and nothing is removed. " +
			"200 with outcome deleted when the network was removed, audited as network.removed with source orphan_cleanup. " +
			"404 network_not_found when the network is not there; 409 network_builtin for the engine's own networks; 422 node_required when two nodes have a local network with that ID. " +
			"Needs networks.manage (Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *OrphanDeleteInput) (*OrphanDeleteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.AddressSpaces == nil || d.Aggregator == nil || d.Audit == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := deleteOrphan(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &OrphanDeleteOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "keepNetworks", Method: http.MethodPut, Path: "/api/env/{envId}/address-space/keeps",
		Summary: "Keep networks out of the orphan list",
		Description: "Stores a keep for each named network (1 to 100), in one transaction: the networks are never listed as orphaned or unused, and a network of that name made again is kept too. " +
			"A network that is kept already changes and records nothing. Each new keep is audited as network.kept. " +
			"422 node_required when a name is held by a local network on more than one node and no nodeId is given. " +
			"Needs networks.keep (Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermNetworksKeep, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *KeepInput) (*KeepOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.AddressSpaces == nil || d.Aggregator == nil || d.Audit == nil || d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			n, err := keepNetworks(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &KeepOutput{CacheControl: "no-store", Body: KeepBody{Kept: n}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "unkeepNetwork", Method: http.MethodDelete, Path: "/api/env/{envId}/address-space/keeps/{name}",
		Summary: "Stop keeping a network out of the orphan list",
		Description: "Removes the keep of one network, audited as network.unkept. " +
			"404 keep_not_found when the network is not kept by a person. 409 kept_by_label when the network is kept by the tierplan.keep=true label on it, which only its owner can remove. " +
			"422 node_required when a local network of that name is on more than one node and no nodeId is given. " +
			"Needs networks.keep (Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermNetworksKeep, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UnkeepInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || d.AddressSpaces == nil || d.Aggregator == nil || d.Audit == nil || d.Store == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, unkeepNetwork(ctx, d, p, env.ID, in)
		})
}

// keepNode is the node a keep for the network called name is keyed by: "" for a swarm-wide
// network, else the node's ID. A name held by local networks on several nodes needs the node.
// labelKept says whether a network of that name on that node carries the keep label.
func keepNode(agg envs.Aggregate, name, nodeID string) (node string, labelKept bool, err error) {
	var local []envs.NetworkRow
	for _, n := range agg.Networks {
		if n.Name != name {
			continue
		}
		if n.Scope == dockerx.ScopeSwarm {
			return "", n.Labels[stacks.LabelKeep] == "true", nil
		}
		if nodeID == "" || n.NodeID == nodeID {
			local = append(local, n)
		}
	}
	switch {
	case nodeID != "":
		return nodeID, len(local) > 0 && local[0].Labels[stacks.LabelKeep] == "true", nil
	case len(local) == 1:
		return local[0].NodeID, local[0].Labels[stacks.LabelKeep] == "true", nil
	case len(local) > 1:
		return "", false, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
			"More than one node has a network named "+name+". Say which one (nodeId).")
	}
	return "", false, nil
}

// keepInternal logs a failure of the store and answers it as a failed request.
func keepInternal(d AddressSpaceDeps, what, envID string, err error) error {
	d.Log.Error("could not change the networks kept from the orphan list", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// keepNetworks stores a keep for each target not kept already. The rows and their audit entries
// are one transaction: a request that changes nothing writes nothing.
func keepNetworks(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *KeepInput) (int, error) {
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return 0, snapshotProblem(d.Log, envID, err)
	}
	type target struct{ name, node string }
	var targets []target
	seen := map[target]bool{}
	for _, t := range in.Body.Targets {
		node, _, err := keepNode(agg, t.Name, t.NodeID)
		if err != nil {
			return 0, err
		}
		k := target{t.Name, node}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, k)
		}
	}

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, keepInternal(d, "begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	existing, err := q.ListNetworkKeeps(ctx, envID)
	if err != nil {
		return 0, keepInternal(d, "list", envID, err)
	}
	have := make(map[target]bool, len(existing))
	for _, k := range existing {
		have[target{k.Name, k.NodeID}] = true
	}
	bulk := ""
	if len(targets) > 1 {
		bulk = "true"
	}
	kept := 0
	for _, t := range targets {
		if have[t] {
			continue
		}
		if err := q.InsertNetworkKeep(ctx, sqlc.InsertNetworkKeepParams{
			EnvID: envID, NodeID: t.node, Name: t.name, KeptBy: p.ID, KeptAt: d.Now().UTC().UnixNano(),
		}); err != nil {
			return 0, keepInternal(d, "keep", envID, err)
		}
		if err := d.Audit.RecordTx(ctx, tx, keepEntry(ctx, p, envID, in.UserAgent, audit.ActionNetworkKept, t.name, t.node, bulk)); err != nil {
			return 0, keepInternal(d, "audit", envID, err)
		}
		kept++
	}
	if kept == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, keepInternal(d, "commit", envID, err)
	}
	d.AddressSpaces.ForgetOrphans(envID)
	return kept, nil
}

// keepEntry is the audit entry of a keep or an unkeep. The target is the network's name: a keep
// is keyed by it, and the network may not exist.
func keepEntry(ctx context.Context, p *rbac.Principal, envID, userAgent, action, name, node, bulk string) audit.Entry {
	details := map[string]string{"name": name}
	if node != "" {
		details["node"] = node
	}
	if bulk != "" {
		details["bulk"] = bulk
	}
	return audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: networkTargetType, TargetID: name, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}
}

// unkeepNetwork removes the keep row of one network. A network kept only by its label has no row
// to remove, and is told so.
func unkeepNetwork(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *UnkeepInput) error {
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return snapshotProblem(d.Log, envID, err)
	}
	node, labelKept, err := keepNode(agg, in.Name, in.NodeID)
	if err != nil {
		return err
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return keepInternal(d, "begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	n, err := q.DeleteNetworkKeep(ctx, sqlc.DeleteNetworkKeepParams{EnvID: envID, NodeID: node, Name: in.Name})
	if err != nil {
		return keepInternal(d, "unkeep", envID, err)
	}
	if n == 0 {
		if labelKept {
			return httpx.Problem(http.StatusConflict, problemKeptByLabel,
				"Kept by the tierplan.keep label on the network. Remove the label to stop keeping it.")
		}
		return httpx.Problem(http.StatusNotFound, problemKeepNotFound, in.Name+" is not kept.")
	}
	if err := d.Audit.RecordTx(ctx, tx, keepEntry(ctx, p, envID, in.UserAgent, audit.ActionNetworkUnkept, in.Name, node, "")); err != nil {
		return keepInternal(d, "audit", envID, err)
	}
	if err := tx.Commit(); err != nil {
		return keepInternal(d, "commit", envID, err)
	}
	d.AddressSpaces.ForgetOrphans(envID)
	return nil
}

// keptBody is the kept part of the page. A read that fails leaves the list empty and never fails
// the page.
func keptBody(ctx context.Context, d AddressSpaceDeps, envID string) []KeptRowBody {
	rows, err := d.AddressSpaces.Kept(ctx, envID)
	if err != nil {
		d.Log.Warn("could not list the networks kept from the orphan list", "environment", envID, "error", err)
		return []KeptRowBody{}
	}
	names := map[string]string{}
	out := make([]KeptRowBody, 0, len(rows))
	for _, r := range rows {
		b := KeptRowBody{Name: r.Name, Node: r.Node, NodeID: r.NodeID, Subnets: append([]string{}, r.Subnets...), KeptBy: "user", Present: r.Present}
		if r.ByLabel {
			b.KeptBy = "label"
		}
		if r.KeptBy != "" {
			if !r.ByLabel {
				b.KeptByName = keeperName(ctx, d, names, r.KeptBy)
			}
			b.KeptAt = r.KeptAt.UTC().Format(time.RFC3339)
		}
		out = append(out, b)
	}
	return out
}

// keeperName is the display name of the user who kept a network: the display name, else the
// username, else empty when the user is not known.
func keeperName(ctx context.Context, d AddressSpaceDeps, cache map[string]string, userID string) string {
	if name, ok := cache[userID]; ok {
		return name
	}
	name := ""
	if d.Store != nil {
		if u, err := d.Store.RQ.GetUserByID(ctx, userID); err == nil {
			name = u.DisplayName
			if name == "" {
				name = u.Username
			}
		}
	}
	cache[userID] = name
	return name
}

// deleteOrphan removes one orphaned or unused network for the orphan list's route.
func deleteOrphan(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *OrphanDeleteInput) (OrphanDeleteBody, error) {
	return removeOrphanNetwork(ctx, d, p, envID, in.UserAgent, sourceOrphanCleanup, in.NetworkID, in.NodeID)
}

// removeOrphanNetwork removes one orphaned or unused network, and is the only code that does: the
// orphan list's route and the janitor both call it, source saying which. It judges the network again
// from a fresh read just before it acts and removes it only if it is still orphaned or unused
// (D-12). The network's whole masked inspect document is in the audit log before the remove is
// sent, and a document that cannot be stored whole refuses the delete. The remove is sent once.
// Its errors are problems (httpx.Problem) and its audit rows are written as they happen, by p.
func removeOrphanNetwork(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID, userAgent, source, networkID, nodeID string) (OrphanDeleteBody, error) {
	row, class, err := d.AddressSpaces.Recheck(ctx, envID, networkID, nodeID)
	switch {
	case errors.Is(err, envs.ErrNetworkGone):
		return OrphanDeleteBody{}, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, "This network does not exist. It may have been removed.")
	case errors.Is(err, envs.ErrNetworkAmbiguous):
		return OrphanDeleteBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
			"More than one node has a network with this ID. Say which one (nodeId).")
	case err != nil:
		return OrphanDeleteBody{}, snapshotProblem(d.Log, envID, err)
	}
	o := orphanAttempt{d: d, p: p, envID: envID, userAgent: userAgent, source: source, row: row}
	if row.Scope != dockerx.ScopeSwarm {
		o.node = row.NodeID
	}

	if row.Builtin {
		o.record(ctx, audit.ActionNetworkRemoved, problemNetworkBuiltin, "")
		return OrphanDeleteBody{}, httpx.Problem(http.StatusConflict, problemNetworkBuiltin, row.Name+" is built in and can't be deleted.")
	}
	var reason string
	switch class {
	case netspace.ClassOrphaned:
		reason = "orphan"
	case netspace.ClassUnused:
		reason = "unused"
	case netspace.ClassInUse:
		// Something attached after the list was read: kept, which is the check doing its job.
		return o.kept(ctx, reasonInUseAgain, reasonInUseAgain), nil
	case netspace.ClassCantTell:
		// A node or the manager stopped answering, so TierPlan cannot be sure nothing uses it.
		return o.kept(ctx, reasonNodeMissing, reasonNodeMissing), nil
	default:
		// Its stack exists again, it was kept, it is declared external, its stack is busy, or it is
		// inside the grace period.
		return o.kept(ctx, reasonNotOrphaned, reasonNotOrphaned), nil
	}

	raw, err := o.readInspect(ctx)
	if err != nil {
		return OrphanDeleteBody{}, err
	}
	doc, masked, err := inspectRecord(raw)
	if err != nil {
		d.Log.Error("the inspect document of a network could not be prepared for the audit log", "environment", envID, "network", row.Name, "error", err)
		o.record(ctx, audit.ActionNetworkRemoved, codeActionFailed, "")
		return OrphanDeleteBody{}, huma.Error500InternalServerError("internal error")
	}
	if err := o.saveInspect(ctx, doc, masked); err != nil {
		if errors.Is(err, audit.ErrEvidenceTooLarge) {
			o.record(ctx, audit.ActionNetworkRemoved, problemInspectTooLarge, "")
			return OrphanDeleteBody{}, httpx.Problem(http.StatusConflict, problemInspectTooLarge,
				"TierPlan didn't delete "+row.Name+": its details are over 64 KB, too large to save in the audit log first.")
		}
		d.Log.Error("the inspect document of a network could not be saved in the audit log, so the network was not removed", "environment", envID, "network", row.Name, "error", err)
		o.record(ctx, audit.ActionNetworkRemoved, codeActionFailed, "")
		return OrphanDeleteBody{}, huma.Error500InternalServerError("internal error")
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	remove := func(c *dockerx.Client) error { return c.NetworkRemove(writeCtx, row.NetworkID) }
	if row.Scope == dockerx.ScopeSwarm {
		err = d.Aggregator.WriteOnManager(writeCtx, envID, remove)
	} else {
		err = d.Aggregator.WriteOnNode(writeCtx, envID, row.NodeID, remove)
	}
	d.AddressSpaces.ForgetOrphans(envID)
	if err != nil {
		code, problem := networkRemoveProblem(d.Log, envID, row.Name, err)
		o.record(ctx, audit.ActionNetworkRemoved, code, "")
		// The engine refusing because something is attached is the safety working, not a failure,
		// and it is never retried.
		if dockerx.IsNetworkInUse(err) {
			return OrphanDeleteBody{Outcome: outcomeKept, Reason: reasonInUseAgain, Name: row.Name}, nil
		}
		return OrphanDeleteBody{}, problem
	}
	o.record(ctx, audit.ActionNetworkRemoved, codeRemoved, reason)
	return OrphanDeleteBody{Outcome: outcomeDeleted, Name: row.Name}, nil
}

// orphanAttempt is one delete attempt on a network that exists: who, where and which network.
type orphanAttempt struct {
	d         AddressSpaceDeps
	p         *rbac.Principal
	envID     string
	userAgent string
	// source is what asked for the removal: sourceOrphanCleanup or sourceJanitor.
	source string
	row    envs.OrphanRow
	// node is the node a local network is on, empty for a swarm-wide one.
	node string
}

// record audits the attempt as action: a success when code is codeRemoved, else a failure whose
// reason is the code. reason is orphan or unused for a removal.
func (o orphanAttempt) record(ctx context.Context, action, code, reason string) {
	details := map[string]string{
		"name": o.row.Name, "driver": o.row.Driver, "scope": o.row.Scope, "code": code, "source": o.source,
	}
	if o.node != "" {
		details["node"] = o.node
	}
	if reason != "" {
		details["reason"] = reason
	}
	recordNetwork(ctx, NetworksDeps{Audit: o.d.Audit, Log: o.d.Log}, o.p, o.envID, o.userAgent, action, o.row.NetworkID, o.row.Name, code, details)
}

// kept audits a network left alone and answers it. code is what the audit row carries, reason what
// the person is told.
func (o orphanAttempt) kept(ctx context.Context, code, reason string) OrphanDeleteBody {
	o.record(ctx, audit.ActionNetworkRemoved, code, "")
	return OrphanDeleteBody{Outcome: outcomeKept, Reason: reason, Name: o.row.Name}
}

// readInspect reads the network's inspect document as the engine returns it (the plain inspect,
// not the verbose one), on the node the network was read from.
func (o orphanAttempt) readInspect(ctx context.Context) ([]byte, error) {
	var raw []byte
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := o.d.Aggregator.OnNode(readCtx, o.envID, o.row.NodeID, func(c *dockerx.Client) error {
		var e error
		_, raw, e = c.NetworkDetail(readCtx, o.row.NetworkID, false)
		return e
	})
	switch {
	case err == nil:
		return raw, nil
	case dockerx.IsNotFound(err):
		return nil, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, o.row.Name+" is already gone.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		o.record(ctx, audit.ActionNetworkRemoved, problemNodeUnavailable, "")
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to TierPlan.")
	}
	o.record(ctx, audit.ActionNetworkRemoved, codeActionFailed, "")
	return nil, snapshotProblem(o.d.Log, o.envID, err)
}

// saveInspect writes the network.inspect_saved entry. The remove follows only when it returns nil.
func (o orphanAttempt) saveInspect(ctx context.Context, doc string, masked bool) error {
	details := map[string]string{
		"name": o.row.Name, "source": o.source, "inspect": doc, "masked": strconv.FormatBool(masked),
	}
	if o.node != "" {
		details["node"] = o.node
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	return o.d.Audit.Record(wctx, audit.Entry{
		Actor: o.p.AuditActor(), Action: audit.ActionNetworkInspectSaved, TargetType: networkTargetType, TargetID: o.row.NetworkID,
		EnvID: o.envID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: o.userAgent, Details: details,
	})
}

// inspectRecord is the network's inspect document as it is written to the audit log (D-12): the
// raw JSON object the engine returned, whole, every field in it (the typed detail knows fewer),
// with only the values that may be secrets masked. Labels, Options and IPAM.Options are replaced
// by redact.MaskLabels when each is a map of strings, and withheld whole as redact.Placeholder
// when it has another shape. masked is true when anything was withheld. The document is
// compact, with HTML escaping off and no trailing newline; the control characters encoding/json
// leaves raw (DEL and U+0080 to U+009F) are written as \u escapes, so the audit writer stores it
// unchanged. A raw document that is not one JSON object is an error.
func inspectRecord(raw []byte) (doc string, masked bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", false, fmt.Errorf("network inspect is not a JSON object: %w", err)
	}
	if obj == nil {
		return "", false, errors.New("network inspect is not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", false, errors.New("network inspect has data after the JSON object")
	}

	// maskStrings masks the map at holder[key] when it is a map of strings and withholds it whole
	// when it is anything else but absent or null.
	maskStrings := func(holder map[string]any, key string) {
		v, ok := holder[key]
		if !ok || v == nil {
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			holder[key], masked = redact.Placeholder, true
			return
		}
		strs := make(map[string]string, len(m))
		for k, x := range m {
			s, ok := x.(string)
			if !ok {
				holder[key], masked = redact.Placeholder, true
				return
			}
			strs[k] = s
		}
		if shown, changed := redact.MaskLabels(strs, false); changed {
			holder[key], masked = shown, true
		}
	}
	maskStrings(obj, "Labels")
	maskStrings(obj, "Options")
	switch ipam := obj["IPAM"].(type) {
	case nil:
	case map[string]any:
		maskStrings(ipam, "Options")
	default:
		obj["IPAM"], masked = redact.Placeholder, true
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return "", false, fmt.Errorf("encode network inspect: %w", err)
	}
	var out strings.Builder
	out.Grow(buf.Len())
	for _, r := range strings.TrimRight(buf.String(), "\n") {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&out, `\u%04x`, r)
			continue
		}
		out.WriteRune(r)
	}
	return out.String(), masked, nil
}
