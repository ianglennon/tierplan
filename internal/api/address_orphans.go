// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

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

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/netspace"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/redact"
)

const (
	// sourceOrphanCleanup is the source detail of what the orphan list removes (Phase 11's
	// janitor records its own).
	sourceOrphanCleanup = "orphan_cleanup"
	// problemInspectTooLarge: the network's masked inspect document is over what the audit log
	// stores whole, so the delete is refused (D-12).
	problemInspectTooLarge = "inspect_too_large"

	outcomeDeleted = "deleted"
	outcomeKept    = "kept"

	reasonInUseAgain  = "in_use_again"
	reasonNotOrphaned = "not_orphaned"
	reasonNodeMissing = "node_missing"
)

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
	Outcome string `json:"outcome" enum:"deleted,kept" doc:"deleted: the network was removed. kept: Coaming checked again just before and left it alone, which is a safe answer and not an error"`
	Reason  string `json:"reason" enum:"in_use_again,not_orphaned,node_missing," doc:"Why a network was kept, empty when it was deleted. in_use_again: something uses it now, or the engine refused the remove as in use. not_orphaned: its stack exists again, it was kept, or it is inside the grace period. node_missing: a node or the manager stopped answering, so Coaming could not be sure"`
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
		Description: "Removes one network from the orphan list, once and never retried. Coaming first reads the environment again and judges this one network from that fresh read, " +
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
}

// deleteOrphan removes one orphaned or unused network. It judges the network again from a fresh
// read just before it acts and removes it only if it is still orphaned or unused (D-12). The
// network's whole masked inspect document is in the audit log before the remove is sent, and a
// document that cannot be stored whole refuses the delete. The remove is sent once.
func deleteOrphan(ctx context.Context, d AddressSpaceDeps, p *rbac.Principal, envID string, in *OrphanDeleteInput) (OrphanDeleteBody, error) {
	row, class, err := d.AddressSpaces.Recheck(ctx, envID, in.NetworkID, in.NodeID)
	switch {
	case errors.Is(err, envs.ErrNetworkGone):
		return OrphanDeleteBody{}, httpx.Problem(http.StatusNotFound, problemNetworkNotFound, "This network does not exist. It may have been removed.")
	case errors.Is(err, envs.ErrNetworkAmbiguous):
		return OrphanDeleteBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
			"More than one node has a network with this ID. Say which one (nodeId).")
	case err != nil:
		return OrphanDeleteBody{}, snapshotProblem(d.Log, envID, err)
	}
	o := orphanAttempt{d: d, p: p, envID: envID, userAgent: in.UserAgent, row: row}
	if row.Scope != dockerx.ScopeSwarm {
		o.node = row.NodeID
	}

	var reason string
	switch class {
	case netspace.ClassOrphaned:
		reason = "orphan"
	case netspace.ClassUnused:
		reason = "unused"
	default:
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
				"Coaming didn't delete "+row.Name+": its details are over 64 KB, too large to save in the audit log first.")
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
	row       envs.OrphanRow
	// node is the node a local network is on, empty for a swarm-wide one.
	node string
}

// record audits the attempt as action: a success when code is codeRemoved, else a failure whose
// reason is the code. reason is orphan or unused for a removal.
func (o orphanAttempt) record(ctx context.Context, action, code, reason string) {
	details := map[string]string{
		"name": o.row.Name, "driver": o.row.Driver, "scope": o.row.Scope, "code": code, "source": sourceOrphanCleanup,
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
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Coaming.")
	}
	o.record(ctx, audit.ActionNetworkRemoved, codeActionFailed, "")
	return nil, snapshotProblem(o.d.Log, o.envID, err)
}

// saveInspect writes the network.inspect_saved entry. The remove follows only when it returns nil.
func (o orphanAttempt) saveInspect(ctx context.Context, doc string, masked bool) error {
	details := map[string]string{
		"name": o.row.Name, "source": sourceOrphanCleanup, "inspect": doc, "masked": strconv.FormatBool(masked),
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
