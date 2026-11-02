// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// runningAgentLog is the log of the agent containers running on a node now. Exited
// task containers from before a service change are not included, so a line found
// here was written by an agent of the current spec.
func runningAgentLog(t *testing.T, n *dockertest.Node) string {
	t.Helper()
	out, _ := n.Sh(execCtx(t, time.Minute), "for c in $(docker ps -q --filter label=tierplan.agent=true); do docker logs $c 2>&1; done")
	return out
}

// agentIDs are the sorted IDs of the environment's agents.
func agentIDs(as []servertest.Agent) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ID)
	}
	slices.Sort(out)
	return out
}

// waitServiceUpdated waits until the agent service's rolling update has completed.
func waitServiceUpdated(t *testing.T, mgr *dockertest.Node) {
	t.Helper()
	dockertest.Wait(t, 3*time.Minute, "the agent service update to complete", func() (bool, string) {
		out, _ := mgr.Sh(execCtx(t, time.Minute), "docker service inspect -f '{{if .UpdateStatus}}{{.UpdateStatus.State}}{{end}}' "+agentName)
		state := strings.TrimSpace(out)
		return state == "completed", "update state " + state
	})
}

// TestSwarmAgentsRestartWithoutAJoinToken takes the join-token secret and the
// TIERPLAN_AGENT_JOIN_TOKEN_FILE variable off a running swarm service, as
// docs/agent.md says an operator may once every agent is enrolled, and expects
// every agent to restart as the same agent with no new enrolment (G-02-5, UAT
// test 5, D-03). The service keeps the install's capability set (all dropped but
// SETUID and SETGID), so the agent starts as root without CAP_DAC_OVERRIDE and
// cannot read UID 1000's 0700 state directory until it has dropped its
// privileges (D-22).
func TestSwarmAgentsRestartWithoutAJoinToken(t *testing.T) {
	f := startSwarmFix(t, 1, 1)
	mgr := f.sw.Managers[0]
	f.install(t)
	f.waitSwarmOnline(t, 2)
	ids := agentIDs(f.srv.Agents(f.env.ID))
	enrolled := len(f.srv.AuditRows("agent.enrolled"))
	if len(ids) != 2 || enrolled != 2 {
		t.Fatalf("after the install: agent IDs %v, agent.enrolled rows %d, want 2 of each", ids, enrolled)
	}

	spec := inspectService(t, mgr, agentName)
	if got := stripCap(spec.TaskTemplate.ContainerSpec.CapabilityDrop); !slices.Equal(got, []string{"ALL"}) {
		t.Fatalf("CapabilityDrop = %v, want [ALL]", got)
	}
	if got := sorted(stripCap(spec.TaskTemplate.ContainerSpec.CapabilityAdd)); !slices.Equal(got, []string{"SETGID", "SETUID"}) {
		t.Fatalf("CapabilityAdd = %v, want [SETGID SETUID]", got)
	}

	// The commands docs/agent.md shows ("Run enrolled agents without the join token").
	t.Logf("service update:\n%s", quiet(sh(t, mgr, " docker service update --secret-rm "+f.env.Install.SecretName+" --env-rm TIERPLAN_AGENT_JOIN_TOKEN_FILE "+agentName)))
	sh(t, mgr, " docker secret rm "+f.env.Install.SecretName)
	waitServiceUpdated(t, mgr)

	spec = inspectService(t, mgr, agentName)
	if len(spec.TaskTemplate.ContainerSpec.Secrets) != 0 {
		t.Errorf("service secrets = %+v, want none", spec.TaskTemplate.ContainerSpec.Secrets)
	}
	for _, e := range spec.TaskTemplate.ContainerSpec.Env {
		if strings.HasPrefix(e, "TIERPLAN_AGENT_JOIN_TOKEN") {
			t.Errorf("the service still carries %s", e)
		}
	}
	if got := stripCap(spec.TaskTemplate.ContainerSpec.CapabilityDrop); !slices.Equal(got, []string{"ALL"}) {
		t.Errorf("CapabilityDrop after the update = %v, want [ALL]", got)
	}

	f.waitSwarmOnline(t, 2)
	for _, n := range f.sw.Nodes() {
		log := runningAgentLog(t, n)
		for _, want := range []string{"token_present=false", "enrolled=true", "connected to TierPlan"} {
			if !strings.Contains(log, want) {
				t.Errorf("%s: the running agent's log lacks %q:\n%s", n.Hostname, want, log)
			}
		}
		for _, bad := range []string{"read the enrolled state", "enrolled with TierPlan"} {
			if strings.Contains(log, bad) {
				t.Errorf("%s: the running agent's log has %q:\n%s", n.Hostname, bad, log)
			}
		}
	}
	if got := agentIDs(f.srv.Agents(f.env.ID)); !slices.Equal(got, ids) {
		t.Errorf("agent IDs changed:\n before %v\n after  %v", ids, got)
	}
	if got := len(f.srv.AuditRows("agent.enrolled")); got != enrolled {
		t.Errorf("agent.enrolled audit rows = %d, want the same %d", got, enrolled)
	}

	// Put the token back with the command docs/agent.md shows. The rotate command is
	// not used: it reads the current secret from the service and fails with none.
	rot := f.srv.RotateToken(f.env.ID)
	t.Logf("add the token back:\n%s", quiet(sh(t, mgr, " printf %s '"+rot.Install.JoinToken+"' | docker secret create "+rot.Install.SecretName+" - "+
		"&& docker service update --secret-add source="+rot.Install.SecretName+",target=tierplan-agent-token,mode=0400 --env-add TIERPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/tierplan-agent-token "+agentName)))
	waitServiceUpdated(t, mgr)
	spec = inspectService(t, mgr, agentName)
	secrets := spec.TaskTemplate.ContainerSpec.Secrets
	if len(secrets) != 1 || secrets[0].SecretName != rot.Install.SecretName || secrets[0].File.Mode != 0o400 {
		t.Errorf("service secrets = %+v, want only %s with mode 0400", secrets, rot.Install.SecretName)
	}
	if !slices.Contains(spec.TaskTemplate.ContainerSpec.Env, "TIERPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/tierplan-agent-token") {
		t.Errorf("service env = %v, want TIERPLAN_AGENT_JOIN_TOKEN_FILE", spec.TaskTemplate.ContainerSpec.Env)
	}
	f.waitSwarmOnline(t, 2)
	for _, n := range f.sw.Nodes() {
		if log := runningAgentLog(t, n); !strings.Contains(log, "token_present=true") {
			t.Errorf("%s: the running agent's log lacks token_present=true after the token was put back:\n%s", n.Hostname, log)
		}
	}
	if got := agentIDs(f.srv.Agents(f.env.ID)); !slices.Equal(got, ids) {
		t.Errorf("agent IDs changed after the token was put back:\n before %v\n after  %v", ids, got)
	}
	if got := len(f.srv.AuditRows("agent.enrolled")); got != enrolled {
		t.Errorf("agent.enrolled audit rows = %d after the token was put back, want the same %d", got, enrolled)
	}
}
