// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package envs

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx"
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
		if isJobMode(s.Mode) {
			continue
		}
		total++
		if serviceFailing(s, now, grace) {
			failing++
		}
	}
	return failing, total
}

// isJobMode is whether a service is meant to stop, which is never failing.
func isJobMode(mode string) bool { return mode == "replicated-job" || mode == "global-job" }

// serviceFailing is the rule of FailingServices for one service that is not a job: fewer
// running tasks than desired, unless it is changing on purpose or was updated less than grace ago.
func serviceFailing(s dockerx.ServiceRow, now time.Time, grace time.Duration) bool {
	if s.UpdateState == updateUpdating || s.UpdateState == updateRollbackStarted {
		return false
	}
	if !s.UpdatedAt.IsZero() && now.Sub(s.UpdatedAt) < grace {
		return false
	}
	return s.Running < s.Desired
}

// MaxFailingErrorLen is how many characters of a task's error a failing row keeps (T-12-80).
const MaxFailingErrorLen = 200

// FailingServiceRow is one swarm service that FailingServices counts as failing.
type FailingServiceRow struct {
	ID, Name, Stack  string
	Running, Desired uint64
	// Since is when the service last failed: the time of its newest failed task, else the time
	// it was last updated. Zero when neither is known.
	Since time.Time
	// LastError is the error of the newest failed task of the service, cleaned of control
	// characters and cut to MaxFailingErrorLen; empty when the history holds none.
	LastError string
}

// FailingServiceRows are the services FailingServices counts as failing, with what a person
// wants to know about each: how many of its tasks run, since when it fails and the error of its
// newest failed task. failed is the cluster's failed tasks, newest first as ClusterRead gives
// them. The rows are in the order of services.
func FailingServiceRows(services []dockerx.ServiceRow, failed []dockerx.FailedTaskRow, now time.Time, grace time.Duration) []FailingServiceRow {
	newest := map[string]dockerx.FailedTaskRow{}
	for _, t := range failed {
		if cur, ok := newest[t.ServiceID]; !ok || t.At.After(cur.At) {
			newest[t.ServiceID] = t
		}
	}
	rows := []FailingServiceRow{}
	for _, s := range services {
		if isJobMode(s.Mode) || !serviceFailing(s, now, grace) {
			continue
		}
		row := FailingServiceRow{ID: s.ID, Name: s.Name, Stack: s.Stack, Running: s.Running, Desired: s.Desired, Since: s.UpdatedAt}
		if t, ok := newest[s.ID]; ok {
			row.Since, row.LastError = t.At, cleanText(t.Err, MaxFailingErrorLen)
		}
		rows = append(rows, row)
	}
	return rows
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

// Container states a failing row reports.
const (
	FailingRestarting = "restarting"
	FailingUnhealthy  = "unhealthy"
	FailingExited     = "exited"
)

// composeProjectLabel is the label of the Compose project a container belongs to.
const composeProjectLabel = "com.docker.compose.project"

// FailingContainerRow is one standalone container that FailingContainers counts as failing.
type FailingContainerRow struct {
	ID, Name, Node, NodeID, Stack string
	// State is FailingRestarting, FailingUnhealthy or FailingExited.
	State string
	// Since is when the container reached its state, worked out from the age in its status text
	// ("Exited (1) 5 minutes ago"), so only about right. Zero when the status carries none, as an
	// unhealthy container's does not.
	Since time.Time
	// LastError says in words why the container is listed, with its exit code when it has one.
	LastError string
}

// FailingContainerRows are the containers FailingContainers counts as failing: those whose
// restart policy is known and is not "no" and that are restarting, unhealthy or exited with a
// non-zero code. It reads the policies cache holds and inspects nothing, so a container the
// cache has not met yet is left out until a cycle has; at is when the rows were read. The rows
// are in the order of rows.
func FailingContainerRows(rows []ContainerRow, cache *PolicyCache, at time.Time) []FailingContainerRow {
	out := []FailingContainerRow{}
	if cache == nil {
		return out
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, r := range rows {
		p, known := cache.m[r.ID]
		if !known || p.Name == "" || p.Name == "no" || !isFailingState(r.ContainerRow) {
			continue
		}
		row := FailingContainerRow{ID: r.ID, Name: r.Name, Node: r.Node, NodeID: r.NodeID, Stack: r.Labels[composeProjectLabel]}
		switch code := exitCodeOf(r.Status); {
		case r.State == "restarting":
			row.State, row.Since = FailingRestarting, agoOf(r.Status, at)
			row.LastError = "Keeps restarting"
			if code != "" {
				row.LastError += ", last exit code " + code
			}
		case strings.Contains(r.Status, "(unhealthy)"):
			row.State, row.LastError = FailingUnhealthy, "Its health check says it is unhealthy"
		default:
			row.State, row.Since = FailingExited, agoOf(r.Status, at)
			row.LastError = "Exited with code " + code
		}
		out = append(out, row)
	}
	return out
}

// statusCode matches the exit code in "Exited (137) 3 minutes ago" or "Restarting (1) 5 seconds ago".
var statusCode = regexp.MustCompile(`^(?:Exited|Restarting) \((-?\d+)\)`)

func exitCodeOf(status string) string {
	if m := statusCode.FindStringSubmatch(status); m != nil {
		return m[1]
	}
	return ""
}

// statusAgo matches the age the engine ends a status with: "3 minutes ago", "About an hour ago".
var statusAgo = regexp.MustCompile(`(?:(\d+)|[Aa]bout an?|[Ll]ess than a) (second|minute|hour|day|week|month|year)s? ago$`)

// agoOf is the time a status text says its state began, relative to at, or zero when it says
// none. The engine rounds ("About an hour"), so this is an estimate.
func agoOf(status string, at time.Time) time.Time {
	m := statusAgo.FindStringSubmatch(status)
	if m == nil || at.IsZero() {
		return time.Time{}
	}
	n := 1
	if m[1] != "" {
		var err error
		if n, err = strconv.Atoi(m[1]); err != nil {
			return time.Time{}
		}
	}
	unit := map[string]time.Duration{
		"second": time.Second, "minute": time.Minute, "hour": time.Hour, "day": 24 * time.Hour,
		"week": 7 * 24 * time.Hour, "month": 30 * 24 * time.Hour, "year": 365 * 24 * time.Hour,
	}[m[2]]
	if unit == 0 || n > 1000 {
		return time.Time{}
	}
	return at.Add(-time.Duration(n) * unit)
}
