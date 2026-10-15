// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package envs

import (
	"sort"
	"time"
)

// The IDs of the environment warnings (D-39).
const (
	WarnEnvOffline         = "env_offline"
	WarnCertNotRenewing    = "cert_not_renewing"
	WarnManualUpdateNeeded = "manual_update_needed"
	WarnClockSkew          = "clock_skew"
	WarnPendingLong        = "pending_long"
	WarnClusterChanged     = "cluster_changed"
	WarnFoldOffered        = "fold_offered"
	// WarnRestartLoop is a swarm service whose tasks keep failing and whose alerts nobody
	// muted (plan 06-35, RES-13, D-15).
	WarnRestartLoop = "restart_loop"
)

// maxWarnError is the longest latest-error text a restart-loop warning carries, in characters.
const maxWarnError = 300

// The severities of an environment warning.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// EnvWarning is one environment problem worth a bell item.
type EnvWarning struct {
	ID, Severity, EnvID, EnvName, Node, Reason, AgentID string
	Since                                               time.Time
	Days, Count                                         int
	Offset                                              time.Duration
	ExpiresAt                                           time.Time
	// For a restart loop: the service, how long a window the count is over, and Docker's
	// latest task error. Count is how many tasks failed in that window.
	Service, ServiceID, LatestError string
	WindowMinutes                   int
}

// AgentState is what the warnings need to know of one agent.
type AgentState struct {
	ID, Node     string
	Revoked      bool
	CertNotAfter time.Time
	ClockOffset  time.Duration
	// UpdateFailed is true when the agent's last update did not finish.
	UpdateFailed bool
	// ManualUpdate is true when the agent is behind and Bayplan cannot update it.
	ManualUpdate bool
}

// PendingLongAfter is how long an environment may wait for its first agent before the
// bell says so (D-37).
const PendingLongAfter = 7 * 24 * time.Hour

// Warnings derives the environment warnings of the header bell from live state (D-37,
// D-39). It is pure: the environments, their agents and the clock are the input, and the
// same input always gives the same list. An item exists only while its cause does, so
// there is nothing to dismiss. The caller passes only the environments the reader may
// see, and the counts and the swarm a fold names stay inside that set.
//
// A revoked environment says nothing here, its own page explains it. Names an agent
// supplied are capped, so a hostile node name cannot fill the bell (T-02-99). The list is
// ordered errors first, then newest first, then by environment, node and ID.
func Warnings(envs []Environment, agents map[string][]AgentState, now time.Time) []EnvWarning {
	var out []EnvWarning
	var manual []EnvWarning // one entry per agent that needs a manual update
	for _, e := range envs {
		name := cleanText(e.Name, maxNodeLen)
		base := EnvWarning{EnvID: e.ID, EnvName: name}
		switch e.Health.State {
		case StateOffline:
			w := base
			w.ID, w.Severity, w.Reason, w.Since = WarnEnvOffline, SeverityError, e.Health.Reason, e.Health.Since
			out = append(out, w)
		case StateClusterChanged:
			w := base
			w.ID, w.Severity, w.Reason, w.Since = WarnClusterChanged, SeverityError, e.Health.Reason, e.Health.Since
			out = append(out, w)
		case StatePending:
			if age := now.Sub(e.CreatedAt); e.Kind == KindAgent && age >= PendingLongAfter {
				w := base
				w.ID, w.Severity, w.Since = WarnPendingLong, SeverityWarning, e.CreatedAt
				w.Days = int(age / (24 * time.Hour))
				out = append(out, w)
			}
		}
		// The offer is between the local host and a swarm environment and both report it;
		// it is told once, on the swarm, which is the environment the fold happens in.
		if e.FoldOffered && e.Kind == KindAgent {
			w := base
			w.ID, w.Severity = WarnFoldOffered, SeverityWarning
			out = append(out, w)
		}
		for _, a := range agents[e.ID] {
			if a.Revoked {
				continue
			}
			node := cleanText(a.Node, maxNodeLen)
			if !a.CertNotAfter.IsZero() && a.CertNotAfter.Sub(now) < CertWarnBefore {
				w := base
				w.ID, w.Severity, w.Node, w.AgentID = WarnCertNotRenewing, SeverityWarning, node, a.ID
				if !a.CertNotAfter.After(now) {
					w.Severity = SeverityError
				}
				w.ExpiresAt, w.Since = a.CertNotAfter, a.CertNotAfter.Add(-CertWarnBefore)
				out = append(out, w)
			}
			if abs(a.ClockOffset) > ClockWarnAbove {
				w := base
				w.ID, w.Severity, w.Node, w.AgentID, w.Offset = WarnClockSkew, SeverityWarning, node, a.ID, a.ClockOffset
				// The rollup knows since when, under the name it gave the node.
				for _, is := range e.Health.Issues {
					if is.Code == ReasonClockSkew && is.Node == node {
						w.Since = is.Since
						break
					}
				}
				out = append(out, w)
			}
			if a.UpdateFailed || a.ManualUpdate {
				manual = append(manual, base)
			}
		}
	}
	if len(manual) > 0 {
		// One item for the count. It names an environment only when they are all one.
		w := EnvWarning{ID: WarnManualUpdateNeeded, Severity: SeverityWarning, Count: len(manual), EnvID: manual[0].EnvID, EnvName: manual[0].EnvName}
		for _, m := range manual {
			if m.EnvID != w.EnvID {
				w.EnvID, w.EnvName = "", ""
				break
			}
		}
		out = append(out, w)
	}
	SortWarnings(out)
	return out
}

// RestartLoopWarnings are the bell items of the restart loops one environment's last cluster
// read found: one error per flagged service whose alerts nobody muted. The loops are by
// service ID. The caller passes only the loops of an environment it may show.
func RestartLoopWarnings(e Environment, loops map[string]RestartLoop) []EnvWarning {
	var out []EnvWarning
	for id, l := range loops {
		if l.Muted {
			continue
		}
		out = append(out, EnvWarning{
			ID: WarnRestartLoop, Severity: SeverityError, EnvID: e.ID, EnvName: cleanText(e.Name, maxNodeLen),
			Service: cleanText(l.Name, maxNodeLen), ServiceID: id, Count: l.Count, WindowMinutes: int(l.Window / time.Minute),
			LatestError: cleanText(l.LatestError, maxWarnError), Since: l.Since,
		})
	}
	SortWarnings(out)
	return out
}

// SortWarnings orders warnings errors first, then newest first, then by environment, node,
// service and ID, so the same warnings always come out in the same order.
func SortWarnings(out []EnvWarning) {
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := a.Severity == SeverityError, b.Severity == SeverityError; ra != rb {
			return ra
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.After(b.Since)
		}
		if a.EnvID != b.EnvID {
			return a.EnvID < b.EnvID
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.ServiceID != b.ServiceID {
			return a.ServiceID < b.ServiceID
		}
		return a.ID < b.ID
	})
}
