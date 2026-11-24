// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"
	"net/netip"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// VIPFacts reads what the header bell needs to know of an environment's virtual IP (A-07): the configuration row, what the
// reader knows of each node, the instant the address lost its holder, the moves of the last hour and the manager table. The
// second result is false when the environment has no virtual IP row. It reads no log and asks the swarm nothing, so the bell
// can poll it. The row's sealed key is never read into the facts (PROH-26-03). A manager table that cannot be read leaves the
// privileged-services item out and is logged; it does not fail the read.
func (s *Service) VIPFacts(ctx context.Context, envID string) (envs.VIPFacts, bool, error) {
	cfg, err := s.d.Store.GetVIP(ctx, envID)
	if errors.Is(err, ingressstore.ErrVIPNotFound) {
		return envs.VIPFacts{}, false, nil
	}
	if err != nil {
		return envs.VIPFacts{}, false, err
	}
	f := envs.VIPFacts{
		Enabled:  cfg.Enabled,
		Removing: !cfg.Enabled || s.vipIsDisabling(envID),
		Address:  cfg.Prefix().String(),
		RouterID: cfg.RouterID,
	}
	f.AuthWindow = ingressbundle.VIPAuthWindow
	states := s.vipNodeStates(envID)
	for _, n := range states {
		host := n.Hostname
		if host == "" {
			host = n.NodeID
		}
		if n.Foreign {
			f.ForeignRouter = true
			if sender, err := netip.ParseAddr(n.ForeignSender); err == nil && f.ForeignSender == "" {
				f.ForeignSender = sender.String()
			}
		}
		if n.ClockSkew {
			f.ClockNodes = append(f.ClockNodes, envs.VIPClockFact{NodeID: n.NodeID, Host: host})
		}
		switch n.State {
		case ingressbundle.VIPStateMaster:
			f.Holders = append(f.Holders, envs.VIPHolderFact{NodeID: n.NodeID, Host: host})
		case ingressbundle.VIPStateFault:
			f.Faulting = append(f.Faulting, envs.VIPFaultFact{Host: host, TaskStarted: n.TaskStarted})
		}
	}
	if len(f.Holders) == 0 {
		f.NoHolderSince = s.vipHolderlessSince(envID)
	}
	if !f.Enabled || f.Removing {
		// Nothing is said while the virtual IP is off or going, so nothing more is read for it.
		return f, true, nil
	}

	moves, err := s.d.Store.VIPMovesSince(ctx, envID, s.d.Clock.Now().Add(-envs.VIPMovedWindow))
	if err != nil {
		return envs.VIPFacts{}, false, err
	}
	for _, m := range moves {
		f.Moves = append(f.Moves, envs.VIPMoveFact{At: m.At, From: m.FromNodeName, To: m.ToNodeName, Reason: m.Reason})
	}

	if s.d.Managers != nil {
		rows, err := s.d.Managers.ManagerPolicies(ctx, envID)
		if err != nil {
			s.log().Warn("could not read the manager policies for the virtual IP bell", "environment", envID, "error", err)
		}
		for _, r := range rows {
			// Only a manager whose connected agent says it refuses counts: an unknown one has no agent to ask, and the
			// offline items already cover it (U-07).
			if r.State == envs.ManagerRefuses {
				host := r.Hostname
				if host == "" {
					host = r.NodeID
				}
				f.PrivilegedLost = append(f.PrivilegedLost, host)
			}
		}
	}
	return f, true, nil
}
