// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import "github.com/ianglennon/tierplan/internal/envs"

// CodeVIPPrivilegedNotAllowed is the refusal of a virtual IP while any manager of the swarm does not allow
// privileged services (CONTEXT D-19, A-12; UI-SPEC S5).
const CodeVIPPrivilegedNotAllowed = "vip_privileged_not_allowed"

// vipPrivilegedNotAllowedText is the vip_privileged_not_allowed sentence (UI-SPEC S5). It names the setting; the
// managers and where each stands are the finding's items.
const vipPrivilegedNotAllowedText = "Every manager's agent must allow privileged services before a virtual IP can run. " +
	"Set TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true on the agents that don't, then try again. " +
	"The managers and where each stands are listed."

// EvaluateManagerGate decides whether the virtual IP may run, from the table of every manager of the swarm
// (envs.Clusters.ManagerPolicies). It is ok only when the table is not empty and every manager allows: a manager whose
// agent refuses, and one no agent speaks for, both keep it closed, and so does a swarm whose managers could not be
// listed (no rows). Only TierPlan's own Docker socket counts as allowing without an agent, and that is already a row
// that allows. It never sets the opt-in: the agent's own host policy stays the enforcement point.
//
// A refusal is a SeverityError finding with the S5 sentence, and its items are one "<host name>: <state>" per
// manager in the order of the rows, so the text is the same on every read of the same swarm. With no rows there are no items.
func EvaluateManagerGate(rows []envs.ManagerPolicyRow) (ok bool, f *Finding) {
	allowed := len(rows) > 0
	items := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.State != envs.ManagerAllows {
			allowed = false
		}
		name := vipName(r.Hostname)
		if name == "" {
			name = vipName(r.NodeID)
		}
		items = append(items, name+": "+r.State)
	}
	if allowed {
		return true, nil
	}
	return false, &Finding{
		Code:     CodeVIPPrivilegedNotAllowed,
		Message:  vipPrivilegedNotAllowedText,
		Items:    items,
		Severity: SeverityError,
	}
}
