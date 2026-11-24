// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"errors"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// VIPFacts reads what the header bell needs to know of an environment's virtual IP (A-07): the configuration row, what the
// reader knows of each node and the instant the address lost its holder. The second result is false when the environment has
// no virtual IP row. It reads no log and asks the swarm nothing, so the bell can poll it. It never returns the key, and the
// row's sealed key is never read into the facts.
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
	states := s.vipNodeStates(envID)
	for _, n := range states {
		host := n.Hostname
		if host == "" {
			host = n.NodeID
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
	return f, true, nil
}
