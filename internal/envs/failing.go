// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"context"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
)

// DefaultFailingServiceGrace is how long after a swarm service was updated it is
// not counted as failing (D-34, RESEARCH A12).
const DefaultFailingServiceGrace = 2 * time.Minute

// MaxPolicyInspects is how many containers one cycle inspects for their restart
// policy (RESEARCH Pitfall 21, T-02-71).
const MaxPolicyInspects = 50

// FailingServices counts the swarm services that are failing and the services
// that could be.
func FailingServices(services []dockerx.ServiceRow, now time.Time, grace time.Duration) (failing, total int) {
	return 0, 0
}

// PolicyCache remembers the restart policy of each container by its ID.
type PolicyCache struct{}

// NewPolicyCache returns an empty cache.
func NewPolicyCache() *PolicyCache { return &PolicyCache{} }

// Len is how many containers the cache knows.
func (c *PolicyCache) Len() int { return 0 }

// InspectFunc reads the restart policies of containers by ID.
type InspectFunc func(ctx context.Context, ids []string) (map[string]dockerx.RestartPolicy, error)

// FailingContainers counts the standalone containers that are failing and those
// that have a restart policy.
func FailingContainers(ctx context.Context, rows []dockerx.ContainerRow, cache *PolicyCache, inspect InspectFunc) (failing, total int) {
	return 0, 0
}
