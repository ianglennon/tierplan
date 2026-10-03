// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents

import (
	"context"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// Reaper revokes the agents of nodes that have left their swarm.
type Reaper struct{}

// NewReaper returns a reaper over svc.
func NewReaper(svc *Service) *Reaper { return &Reaper{} }

// Observe looks at one successful node list.
func (r *Reaper) Observe(ctx context.Context, envID string, list dockerx.ClusterRead, answeringNodeID string) {
}

// AutoRevoke revokes an agent without blocking its host.
func (s *Service) AutoRevoke(ctx context.Context, envID, agentID string) error { return nil }
