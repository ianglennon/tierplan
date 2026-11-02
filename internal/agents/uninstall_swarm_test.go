// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// sharedUninstallOnRemove removes an environment with uninstallAgents on a real
// swarm of three managers and a worker (D-30): every node's agent starts a helper that
// removes its own tierplan-agent volume once the agent's container is gone, and one
// manager agent removes its own tierplan-agent service and the token secret that service
// names. A secret and volumes that are somebody else's are still there afterwards. The
// removal is audited per agent. Everything happens inside the dind nodes.
func sharedUninstallOnRemove(t *testing.T, f *swarmFix) {
	f.install(t)
	f.waitSwarmOnline(t, 4)
	mgr := f.sw.Managers[0]

	// What belongs to somebody else: a secret on the swarm and a volume on every node.
	sh(t, mgr, "printf x | docker secret create decoy-secret -")
	for _, n := range f.sw.Nodes() {
		sh(t, n, "docker volume create decoy-data")
		if got := strings.Fields(sh(t, n, "docker volume ls -q")); !slices.Contains(got, "tierplan-agent") {
			t.Fatalf("%s has no tierplan-agent volume before the removal: %v", n.Hostname, got)
		}
	}
	secretsBefore := strings.Fields(sh(t, mgr, "docker secret ls --format '{{.Name}}'"))
	if len(secretsBefore) != 2 {
		t.Fatalf("secrets before the removal = %v, want the token secret and the decoy", secretsBefore)
	}
	var agentIDs []string
	for _, a := range f.srv.Agents(f.env.ID) {
		agentIDs = append(agentIDs, a.ID)
	}

	code, body := f.srv.Do(http.MethodDelete, "/api/environments/"+f.env.ID+"?uninstallAgents=true", nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", code, body)
	}

	f.srv.WaitFor("every node to be clean of the agent and the manager of its service and secret", 3*time.Minute, func() (bool, string) {
		var report []string
		clean := true
		for _, n := range f.sw.Nodes() {
			c, cerr := n.Sh(execCtx(t, time.Minute), "docker ps -a --format '{{.Names}}'")
			v, verr := n.Sh(execCtx(t, time.Minute), "docker volume ls -q")
			c, v = strings.TrimSpace(c), strings.TrimSpace(v)
			report = append(report, n.Hostname+": containers ["+c+"] volumes ["+v+"]")
			if cerr != nil || verr != nil || c != "" || v != "decoy-data" {
				clean = false
			}
		}
		svc, _ := mgr.Sh(execCtx(t, time.Minute), "docker service ls --format '{{.Name}}'")
		sec, _ := mgr.Sh(execCtx(t, time.Minute), "docker secret ls --format '{{.Name}}'")
		report = append(report, "services ["+strings.TrimSpace(svc)+"] secrets ["+strings.TrimSpace(sec)+"]")
		if strings.TrimSpace(svc) != "" || strings.TrimSpace(sec) != "decoy-secret" {
			clean = false
		}
		return clean, strings.Join(report, "\n")
	})

	if rows := f.srv.AuditRows("agent.uninstalled"); len(rows) != len(agentIDs) {
		t.Errorf("agent.uninstalled rows = %+v, want one per agent %v", rows, agentIDs)
	}
	if rows := f.srv.AuditRows("environment.removed"); len(rows) != 1 {
		t.Errorf("environment.removed rows = %+v", rows)
	}
	if rows := f.srv.AuditRows("agent.revoked"); len(rows) != len(agentIDs) {
		t.Errorf("agent.revoked rows = %d, want %d", len(rows), len(agentIDs))
	}
}
