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

// TestStandaloneAgent runs the generated standalone command in a dind host, for
// the engine BAYPLAN_TEST_ENGINE names (24 to 29): the real agent image enrols
// with the join token, the environment comes online with the engine's version
// and the dashboard summary is read through the tunnel (D-58, ENV-01). The same
// run checks that the documented hardening flags work with the real image
// (RESEARCH A1, A6, A9, A16) and that nothing publishes a port (ENV-03).
func TestStandaloneAgent(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	env := srv.CreateAgentEnvironment("")
	if !strings.HasPrefix(env.Install.Image, "ghcr.io/ianglennon/bayplan-agent:edge") {
		t.Fatalf("a development server should install the edge image, got %q", env.Install.Image)
	}
	if want := "wss://" + strings.TrimPrefix(srv.URL, "https://") + "/agent/connect"; env.Install.AgentURL != want {
		t.Fatalf("agent URL = %q, want %q", env.Install.AgentURL, want)
	}
	if env.Install.Pin == "" {
		t.Error("the self-signed server's install should carry a pin")
	}

	// The command is run exactly as generated: nothing is replaced.
	out := sh(t, host, env.Install.Standalone.Command)
	t.Logf("docker run: %s", strings.TrimSpace(out))

	got := srv.WaitOnline(env.ID, 60*time.Second)

	// The engine version through the tunnel is the one the node's own Docker reports.
	wantVersion := strings.TrimSpace(sh(t, host, "docker version --format '{{.Server.Version}}'"))
	if got.Engine.Version != wantVersion {
		t.Errorf("engine version through the tunnel = %q, the node's docker says %q", got.Engine.Version, wantVersion)
	}
	if got.Kind != "agent" || got.EnvType != "standalone" || got.Type != "standalone" {
		t.Errorf("environment = kind %q, envType %q, type %q; want agent, standalone, standalone", got.Kind, got.EnvType, got.Type)
	}
	if got.Name != host.Hostname {
		t.Errorf("environment name = %q, want the agent's hostname %q (D-23)", got.Name, host.Hostname)
	}
	// Agent counts are a swarm's; a standalone host shows one node of one.
	if got.Agents != nil || got.Nodes == nil || got.Nodes.Ready != 1 || got.Nodes.Total != 1 {
		t.Errorf("agents = %+v, nodes = %+v, want no agent counts and 1 of 1 nodes", got.Agents, got.Nodes)
	}

	// The dashboard summary is read through the tunnel and describes this host.
	sum, code, body := srv.Summary(env.ID)
	if code != http.StatusOK {
		t.Fatalf("GET summary = %d %s", code, body)
	}
	if sum.Engine.Version != wantVersion {
		t.Errorf("summary engine version = %q, want %q", sum.Engine.Version, wantVersion)
	}
	// The only container on the host is the agent itself.
	if sum.Counts.Containers != 1 || sum.Counts.ContainersRunning != 1 {
		t.Errorf("summary containers = %d (%d running), want the agent alone", sum.Counts.Containers, sum.Counts.ContainersRunning)
	}
	list, code, body := srv.Containers(env.ID, "")
	if code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Name != "bayplan-agent" {
		t.Errorf("containers = %d %s, want the agent alone", code, body)
	}

	// The agent drops from root to UID 1000 and keeps no capability it was not
	// given. The image is distroless, so there is no `docker exec` into it: the
	// process is read from the node's own /proc (A1, A6, A9).
	pid := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.State.Pid}}' bayplan-agent"))
	status := sh(t, host, "cat /proc/"+pid+"/status")
	if uid := fieldOf(status, "Uid:"); uid != "1000 1000 1000 1000" {
		t.Errorf("agent Uid = %q, want 1000 for real, effective, saved and fs", uid)
	}
	if gid := fieldOf(status, "Gid:"); gid != "1000 1000 1000 1000" {
		t.Errorf("agent Gid = %q, want 1000", gid)
	}
	if caps := fieldOf(status, "CapEff:"); strings.Trim(caps, "0") != "" {
		t.Errorf("agent effective capabilities = %s, want none once the privilege drop is done", caps)
	}
	if nnp := fieldOf(status, "NoNewPrivs:"); nnp != "1" {
		t.Errorf("agent NoNewPrivs = %q, want 1", nnp)
	}
	// The flags the command documents are what Docker applied (A16).
	// Newer engines report the capabilities as CAP_SETGID and sorted, older ones as
	// they were written, so the names are compared bare and sorted.
	hc := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.HostConfig.CapAdd}} {{.HostConfig.SecurityOpt}} {{.HostConfig.RestartPolicy.Name}}' bayplan-agent"))
	hc = strings.ReplaceAll(hc, "CAP_", "")
	hc = strings.Replace(hc, "[SETUID SETGID]", "[SETGID SETUID]", 1)
	if want := "true [ALL] [SETGID SETUID] [no-new-privileges] unless-stopped"; hc != want {
		t.Errorf("host config = %q, want %q", hc, want)
	}
	// The state volume is written by UID 1000: the agent's identity is on it.
	if files := sh(t, host, "ls -ln $(docker volume inspect -f '{{.Mountpoint}}' bayplan-agent)"); !strings.Contains(files, " 1000 ") {
		t.Errorf("the state volume holds no file owned by 1000:\n%s", files)
	}

	// No install command opens an inbound port (ENV-03).
	if ports := strings.TrimSpace(sh(t, host, "docker ps --filter name=bayplan-agent --format '{{.Ports}}'")); ports != "" {
		t.Errorf("the agent publishes ports %q, want none", ports)
	}
	// The default bridge is "bridge" on Engine 29 and "default" on 24.
	if nm := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.HostConfig.NetworkMode}} {{len .HostConfig.PortBindings}}' bayplan-agent")); nm != "bridge 0" && nm != "default 0" {
		t.Errorf("agent network mode and bindings = %q, want the default bridge with no published port", nm)
	}
}
