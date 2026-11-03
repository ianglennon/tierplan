// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// janitorNetworks is janitor.Networks over the Address space page's own code: a network is judged
// by the orphan check (Phase 10) and removed by removeOrphanNetwork, the very function the page's
// delete route calls, so the janitor has no second way to remove one.
type janitorNetworks struct {
	d AddressSpaceDeps
}

var _ janitor.Networks = janitorNetworks{}

// NewJanitorNetworks is the janitor's way to judge and remove networks, over the same dependencies
// as the Address space routes, for the Runner the server builds.
func NewJanitorNetworks(d AddressSpaceDeps) janitor.Networks { return janitorNetworks{d: d} }

// Report implements janitor.Networks: every network of the environment, judged from a fresh read.
// A swarm-wide network belongs to no node, so it is given none: the node the check read it from is
// whichever manager answered, and an ID that changed between a preview and a run would not match.
func (n janitorNetworks) Report(ctx context.Context, envID string) (janitor.NetworkReport, error) {
	rep, err := n.d.AddressSpaces.Judge(ctx, envID)
	if err != nil {
		return janitor.NetworkReport{}, err
	}
	out := janitor.NetworkReport{
		Rows: make([]janitor.NetworkRow, 0, len(rep.Rows)), Complete: rep.Complete,
		WithheldNodes: rep.WithheldNodes, WithheldStacks: rep.WithheldStacks,
	}
	for _, r := range rep.Rows {
		row := janitor.NetworkRow{
			ID: r.NetworkID, Name: r.Name, NodeID: r.NodeID, Node: r.Node, Scope: r.Scope, Class: r.Class,
			FormerOwner: r.FormerOwner, FormerOwnerKind: r.FormerOwnerKind,
			KeptByLabel: r.KeptByLabel, KeptByRow: r.KeptByRow, IdleSince: r.IdleSince,
		}
		if r.Scope == dockerx.ScopeSwarm {
			row.NodeID, row.Node = "", envs.NodeCluster
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// Remove implements janitor.Networks: the Address space page's delete, as by, with the source
// janitor in its audit rows. Its answers are read back into the janitor's own: the page's problems
// and its kept outcomes are the same facts the run records.
func (n janitorNetworks) Remove(ctx context.Context, envID string, by *rbac.Principal, row janitor.NetworkRow) error {
	body, err := removeOrphanNetwork(ctx, n.d, by, envID, "", sourceJanitor, row.ID, row.NodeID)
	if err != nil {
		return networkRemoveError(err)
	}
	if body.Outcome != outcomeKept {
		return nil
	}
	switch body.Reason {
	case reasonInUseAgain:
		return janitor.ErrInUse
	case reasonNodeMissing:
		return janitor.ErrNodeOffline
	}
	return janitor.ErrNoLongerStale
}

// networkRemoveError reads the problem removeOrphanNetwork answered as the janitor's own error.
func networkRemoveError(err error) error {
	var m *huma.ErrorModel
	if !errors.As(err, &m) {
		return &janitor.CodedError{Code: janitor.CodeActionFailed, Err: err}
	}
	switch {
	case m.Status == http.StatusNotFound:
		return errors.Join(janitor.ErrGone, err)
	case m.Status == http.StatusServiceUnavailable && m.Type == problemNodeUnavailable:
		return errors.Join(janitor.ErrNodeOffline, err)
	case m.Status == http.StatusConflict && m.Type == problemInspectTooLarge:
		return &janitor.CodedError{Code: janitor.CodeInspectTooLarge, Err: err}
	case m.Status == http.StatusConflict && m.Type == problemNetworkBuiltin:
		return janitor.ErrNoLongerStale
	case m.Status == http.StatusConflict && m.Type == problemNetworkInUse:
		return janitor.ErrInUse
	}
	return &janitor.CodedError{Code: janitor.CodeActionFailed, Err: err}
}
