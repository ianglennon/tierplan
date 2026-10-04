// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// TestStandaloneUninstallOnRemove removes an environment with uninstallAgents on a real
// standalone host (D-30): the real agent tells its helper, started from the real agent
// image with nothing but the Docker socket, to remove the agent's container and then
// its state volume. A volume that is somebody else's is still there afterwards, and the
// removal is audited per agent. Everything happens inside the dind host.
func TestStandaloneUninstallOnRemove(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	sh(t, host, "docker volume create decoy-data")

	if got := strings.Fields(sh(t, host, "docker volume ls -q")); len(got) != 2 {
		t.Fatalf("volumes before the removal = %v, want the agent's and the decoy", got)
	}
	agentID := srv.Agents(env.ID)[0].ID

	code, body := srv.Do(http.MethodDelete, "/api/environments/"+env.ID+"?uninstallAgents=true", nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", code, body)
	}

	var containers, volumes string
	srv.WaitFor("the host to be clean of the agent", 90*time.Second, func() (bool, string) {
		c, cerr := host.Sh(execCtx(t, time.Minute), "docker ps -a --format '{{.Names}}'")
		v, verr := host.Sh(execCtx(t, time.Minute), "docker volume ls -q")
		containers, volumes = strings.TrimSpace(c), strings.TrimSpace(v)
		return cerr == nil && verr == nil && containers == "" && volumes == "decoy-data", "containers: " + containers + "; volumes: " + volumes
	})

	if rows := srv.AuditRows("agent.uninstalled"); len(rows) != 1 || rows[0].TargetID != agentID || rows[0].ActorID == "" {
		t.Errorf("agent.uninstalled rows = %+v, want one for %s", rows, agentID)
	}
	if rows := srv.AuditRows("environment.removed"); len(rows) != 1 {
		t.Errorf("environment.removed rows = %+v", rows)
	}
	if code, _ := srv.Do(http.MethodGet, "/api/env/"+env.ID+"/agents", nil); code != http.StatusNotFound {
		t.Errorf("the removed environment answers %d", code)
	}
}

// TestStandaloneRemoveWithoutUninstallLeavesTheAgentRunning is the other half of the
// checkbox: with uninstallAgents false the environment is gone and the agent is revoked
// and refused, but nothing on the host is touched (D-20).
func TestStandaloneRemoveWithoutUninstallLeavesTheAgentRunning(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	if code, body := srv.Do(http.MethodDelete, "/api/environments/"+env.ID, nil); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", code, body)
	}
	time.Sleep(5 * time.Second)
	if got := strings.TrimSpace(sh(t, host, "docker ps --format '{{.Names}}' --filter name=bayplan-agent")); got != "bayplan-agent" {
		t.Errorf("running containers = %q, want the agent left alone", got)
	}
	if got := strings.Fields(sh(t, host, "docker volume ls -q")); len(got) != 1 || got[0] != "bayplan-agent" {
		t.Errorf("volumes = %v, want the agent's volume kept", got)
	}
	if rows := srv.AuditRows("agent.uninstalled"); len(rows) != 0 {
		t.Errorf("agent.uninstalled rows = %+v, want none", rows)
	}
}
