// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package envs

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
)

// DefaultFailingServiceGrace is how long after a swarm service was updated it is
// not counted as failing (D-34, RESEARCH A12).
const DefaultFailingServiceGrace = 2 * time.Minute

// MaxPolicyInspects is how many containers one cycle inspects for their restart
// policy. A host with hundreds of containers is covered over a few cycles instead
// of by a storm of inspects (RESEARCH Pitfall 21, T-02-71).
const MaxPolicyInspects = 50

// Update states of a swarm service that mean it is changing on purpose.
const (
	updateUpdating        = "updating"
	updateRollbackStarted = "rollback_started"
)

// FailingServices counts the swarm services that are failing, and the services
// that could be (D-34). A replicated service with fewer running tasks than desired
// and a global service missing on a node it should run on are failing; a service
// that is meant to stop (a job mode) is not counted at all; one that is updating or
// rolling back, or was updated less than grace ago, is counted as a service but is
// not failing yet, because its tasks are on their way (RESEARCH Pitfall 21).
func FailingServices(services []dockerx.ServiceRow, now time.Time, grace time.Duration) (failing, total int) {
	for _, s := range services {
		switch s.Mode {
		case "replicated-job", "global-job":
			continue
		}
		total++
		if s.UpdateState == updateUpdating || s.UpdateState == updateRollbackStarted {
			continue
		}
		if !s.UpdatedAt.IsZero() && now.Sub(s.UpdatedAt) < grace {
			continue
		}
		if s.Running < s.Desired {
			failing++
		}
	}
	return failing, total
}

// PolicyCache remembers the restart policy of each container by its ID, so a
// container is inspected once and not every cycle. It is safe for concurrent use.
type PolicyCache struct {
	mu sync.Mutex
	m  map[string]dockerx.RestartPolicy
}

// NewPolicyCache returns an empty cache.
func NewPolicyCache() *PolicyCache { return &PolicyCache{m: map[string]dockerx.RestartPolicy{}} }

// Len is how many containers the cache knows.
func (c *PolicyCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// InspectFunc reads the restart policies of containers by ID. An error means none
// of the answer is trusted.
type InspectFunc func(ctx context.Context, ids []string) (map[string]dockerx.RestartPolicy, error)

// exitedWith matches the status of an exited container, "Exited (137) 3 minutes ago".
var exitedWith = regexp.MustCompile(`^Exited \((-?\d+)\)`)

// isFailingState is whether a container's own state is a failing one: it keeps
// restarting, its health check says unhealthy, or it exited with a non-zero code.
// A container stopped by hand exits 0 and is not failing, whatever its restart
// policy (RESEARCH Open Question 9: the one rule to change if the literal reading of
// D-34 is wanted).
func isFailingState(r dockerx.ContainerRow) bool {
	switch {
	case r.State == "restarting":
		return true
	case strings.Contains(r.Status, "(unhealthy)"):
		return true
	case r.State == "exited":
		m := exitedWith.FindStringSubmatch(r.Status)
		return m != nil && m[1] != "0"
	}
	return false
}

// FailingContainers counts the standalone containers that are failing and the
// containers that have a restart policy (D-34). Only a container with a policy
// other than "no" can fail: it is restarting, unhealthy, or exited with a non-zero
// code.
//
// The restart policy is not in the container list, so each container is inspected
// once and remembered by its ID in cache. One cycle inspects at most
// MaxPolicyInspects containers that are not remembered yet, those in a failing state
// first, so a failing container is found in the cycle it appears and a host with
// hundreds of containers is covered over a few cycles. total counts only containers
// whose policy is known, so it grows to the real number as the cache fills. A
// container that is no longer listed leaves the cache. When inspect fails, the
// containers it could not read are left out until the next cycle.
func FailingContainers(ctx context.Context, rows []dockerx.ContainerRow, cache *PolicyCache, inspect InspectFunc) (failing, total int) {
	present := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		present[r.ID] = struct{}{}
	}

	cache.mu.Lock()
	for id := range cache.m {
		if _, ok := present[id]; !ok {
			delete(cache.m, id)
		}
	}
	var todo []string
	for _, pass := range []bool{true, false} {
		for _, r := range rows {
			if _, known := cache.m[r.ID]; known || isFailingState(r) != pass {
				continue
			}
			if len(todo) < MaxPolicyInspects {
				todo = append(todo, r.ID)
			}
		}
	}
	cache.mu.Unlock()

	if len(todo) > 0 && inspect != nil {
		got, err := inspect(ctx, todo)
		if err != nil {
			// One container that vanished since the list fails the whole batch.
			// Asking one at a time loses only that container.
			got = map[string]dockerx.RestartPolicy{}
			for _, id := range todo {
				if one, err := inspect(ctx, []string{id}); err == nil {
					got[id] = one[id]
				}
			}
		}
		cache.mu.Lock()
		for id, p := range got {
			cache.m[id] = p
		}
		cache.mu.Unlock()
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, r := range rows {
		p, known := cache.m[r.ID]
		if !known || p.Name == "" || p.Name == "no" {
			continue
		}
		total++
		if isFailingState(r) {
			failing++
		}
	}
	return failing, total
}
