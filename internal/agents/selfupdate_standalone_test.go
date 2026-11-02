// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
)

// agentContainer is the name the standalone install gives the agent container.
const agentContainer = "tierplan-agent"

// containerView is the part of `docker inspect` of the agent container that an
// update must carry over.
type containerView struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Image  string
		Env    []string
		Labels map[string]string
		User   string
	}
	HostConfig struct {
		RestartPolicy  struct{ Name string }
		ReadonlyRootfs bool
		CapAdd         []string
		CapDrop        []string
		SecurityOpt    []string
		NetworkMode    string
		PortBindings   map[string]any
		Privileged     bool
	}
	Mounts []struct {
		Type        string
		Name        string
		Source      string
		Destination string
		RW          bool
	}
	State struct {
		Running bool
		Status  string
	}
}

func inspectContainer(t *testing.T, n *dockertest.Node, name string) containerView {
	t.Helper()
	var got []containerView
	out := sh(t, n, "docker inspect "+name)
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 {
		t.Fatalf("parse docker inspect %s: %v\n%s", name, err, out)
	}
	return got[0]
}

// operators is what the operator chose and an update must keep: the TIERPLAN_ settings
// (the engine and the image add the rest), the tierplan.* labels (the image adds
// org.opencontainers.*), the mounts, the restart policy and the hardening flags.
func (c containerView) operators() map[string]any {
	var env []string
	for _, e := range c.Config.Env {
		if strings.HasPrefix(e, "TIERPLAN_") {
			env = append(env, e)
		}
	}
	slices.Sort(env)
	labels := map[string]string{}
	for k, v := range c.Config.Labels {
		if strings.HasPrefix(k, "tierplan.") {
			labels[k] = v
		}
	}
	var mounts []string
	for _, m := range c.Mounts {
		rw := "ro"
		if m.RW {
			rw = "rw"
		}
		mounts = append(mounts, strings.Join([]string{m.Type, m.Name, m.Source, m.Destination, rw}, "|"))
	}
	slices.Sort(mounts)
	// Newer engines report capabilities as CAP_SETGID and sorted, older ones as written.
	norm := func(in []string) []string {
		var out []string
		for _, c := range in {
			out = append(out, strings.TrimPrefix(c, "CAP_"))
		}
		slices.Sort(out)
		return out
	}
	netMode := c.HostConfig.NetworkMode
	if netMode == "default" {
		netMode = "bridge"
	}
	return map[string]any{
		"name": c.Name, "env": env, "labels": labels, "mounts": mounts, "user": c.Config.User,
		"restart": c.HostConfig.RestartPolicy.Name, "readonly": c.HostConfig.ReadonlyRootfs,
		"capAdd": norm(c.HostConfig.CapAdd), "capDrop": norm(c.HostConfig.CapDrop),
		"securityOpt": c.HostConfig.SecurityOpt, "network": netMode,
		"ports": len(c.HostConfig.PortBindings), "privileged": c.HostConfig.Privileged,
	}
}

// TestStandaloneSelfUpdate upgrades a server from v0.2.0 to v0.2.1 while a real
// standalone agent of v0.2.0 is connected (D-24): the server updates the agent by
// itself, through a helper container started from the real agent image, which
// recreates the agent with the same name, mounts, environment, labels, restart policy
// and hardening on the same state volume. The agent keeps its identity (no new
// enrolment), comes back as v0.2.1, and no helper or old container is left behind.
func TestStandaloneSelfUpdate(t *testing.T) {
	host := dockertest.StartHost(t, "")
	loadReleases(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway, Version: oldRelease})

	env := srv.CreateAgentEnvironment("")
	if want := agents.AgentImage(oldRelease); env.Install.Image != want {
		t.Fatalf("install image = %q, want %q", env.Install.Image, want)
	}
	// The command is run exactly as generated.
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	before := inspectContainer(t, host, agentContainer)
	rows := srv.Agents(env.ID)
	if len(rows) != 1 || rows[0].AgentVersion != oldRelease {
		t.Fatalf("agents before the upgrade = %+v, want one at %s", rows, oldRelease)
	}
	agentID := rows[0].ID
	volume := strings.TrimSpace(sh(t, host, "docker volume ls -q"))

	// The server is upgraded; the agent reconnects and is updated, with nobody asking.
	srv.Restart(newRelease)
	srv.WaitFor("the agent to run "+newRelease, 4*time.Minute, func() (bool, string) {
		rows := srv.Agents(env.ID)
		return len(rows) == 1 && rows[0].Connected && rows[0].AgentVersion == newRelease, summariseAgents(rows)
	})

	rows = srv.Agents(env.ID)
	if len(rows) != 1 || rows[0].ID != agentID {
		t.Fatalf("agents after the update = %+v, want the same single agent %s (no new enrolment)", rows, agentID)
	}
	if st := rows[0].UpdateState; st.State != "idle" {
		t.Errorf("update state after the update = %+v, want idle", st)
	}

	after := inspectContainer(t, host, agentContainer)
	if after.ID == before.ID {
		t.Error("the agent container was not replaced")
	}
	if !after.State.Running {
		t.Errorf("the recreated agent is %q, want running", after.State.Status)
	}
	if want := agents.AgentImage(newRelease); after.Config.Image != want {
		t.Errorf("recreated image = %q, want %q", after.Config.Image, want)
	}
	if b, a := before.operators(), after.operators(); !jsonEqual(b, a) {
		t.Errorf("the recreated agent differs from the original:\nbefore %s\nafter  %s", mustJSON(b), mustJSON(a))
	}
	if got := strings.TrimSpace(sh(t, host, "docker volume ls -q")); got != volume {
		t.Errorf("volumes after the update = %q, want the same as before (%q): the state volume must be kept and no other made", got, volume)
	}

	// Nothing is left behind: no helper, no renamed old agent. The helper removes the
	// old agent once the new one has connected and then exits, a moment after the
	// agent is back, so this waits for it.
	var left string
	srv.WaitFor("the helper and the old agent to be gone", 90*time.Second, func() (bool, string) {
		out, err := host.Sh(execCtx(t, time.Minute), "docker ps -a --format '{{.Names}}'")
		left = strings.TrimSpace(out)
		return err == nil && left == agentContainer, left
	})
	if left != agentContainer {
		t.Errorf("containers after the update:\n%s\nwant only %s (no helper, no %s-old)", left, agentContainer, agentContainer)
	}

	updated := srv.AuditRows("agent.updated")
	if len(updated) != 1 || updated[0].ActorID != "system:agent-update" || updated[0].TargetID != agentID ||
		updated[0].Details["image"] != agents.AgentImage(newRelease) || updated[0].Details["mode"] != "auto" {
		t.Errorf("agent.updated rows = %+v, want one by system:agent-update", updated)
	}
	if failed := srv.AuditRows("agent.update_failed"); len(failed) != 0 {
		t.Errorf("agent.update_failed rows = %+v, want none", failed)
	}
}
