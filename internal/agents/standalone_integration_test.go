// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build integration

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/agents"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/server/servertest"
)

// TestStandaloneAgent runs the generated standalone command in a dind host, for
// the engine COAMING_TEST_ENGINE names (24 to 29): the real agent image enrols
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
	if !strings.HasPrefix(env.Install.Image, "ghcr.io/ianglennon/coaming-agent:edge") {
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
	if code != http.StatusOK || len(list.Items) != 1 || list.Items[0].Name != "coaming-agent" {
		t.Errorf("containers = %d %s, want the agent alone", code, body)
	}

	// The agent drops from root to UID 1000 and keeps no capability it was not
	// given. The image is distroless, so there is no `docker exec` into it: the
	// process is read from the node's own /proc (A1, A6, A9).
	pid := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.State.Pid}}' coaming-agent"))
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
	hc := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.HostConfig.CapAdd}} {{.HostConfig.SecurityOpt}} {{.HostConfig.RestartPolicy.Name}}' coaming-agent"))
	hc = strings.ReplaceAll(hc, "CAP_", "")
	hc = strings.Replace(hc, "[SETUID SETGID]", "[SETGID SETUID]", 1)
	if want := "true [ALL] [SETGID SETUID] [no-new-privileges] unless-stopped"; hc != want {
		t.Errorf("host config = %q, want %q", hc, want)
	}
	// The state volume is written by UID 1000: the agent's identity is on it.
	if files := sh(t, host, "ls -ln $(docker volume inspect -f '{{.Mountpoint}}' coaming-agent)"); !strings.Contains(files, " 1000 ") {
		t.Errorf("the state volume holds no file owned by 1000:\n%s", files)
	}

	// No install command opens an inbound port (ENV-03).
	if ports := strings.TrimSpace(sh(t, host, "docker ps --filter name=coaming-agent --format '{{.Ports}}'")); ports != "" {
		t.Errorf("the agent publishes ports %q, want none", ports)
	}
	// The default bridge is "bridge" on Engine 29 and "default" on 24.
	if nm := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.HostConfig.NetworkMode}} {{len .HostConfig.PortBindings}}' coaming-agent")); nm != "bridge 0" && nm != "default 0" {
		t.Errorf("agent network mode and bindings = %q, want the default bridge with no published port", nm)
	}
}

// TestStandaloneAgentWithDockerUnreachable starts the agent where it cannot reach
// the Docker socket it was told to use (02-19: "start an agent with Docker
// stopped"). The server cannot tell what such a host is, so it refuses the agent
// with engine_unreachable, which the agent logs with advice and the audit records;
// the environment stays pending and no agent is enrolled.
func TestStandaloneAgentWithDockerUnreachable(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	env := srv.CreateAgentEnvironment("")

	cmd := strings.Replace(env.Install.Standalone.Command, " "+env.Install.Image, " -e COAMING_AGENT_SOCKET=/var/run/missing.sock "+env.Install.Image, 1)
	if cmd == env.Install.Standalone.Command {
		t.Fatal("could not add the socket override to the command")
	}
	sh(t, host, cmd)

	srv.WaitFor("the agent to be refused with engine_unreachable", 60*time.Second, func() (bool, string) {
		out, _ := host.Sh(execCtx(t, time.Minute), "docker logs --tail 5 coaming-agent 2>&1")
		return strings.Contains(out, "code=engine_unreachable") && strings.Contains(out, "Check that the Docker socket is mounted"), out
	})
	srv.WaitFor("the audit log to record engine_unreachable", 30*time.Second, func() (bool, string) {
		code, body := srv.Do(http.MethodGet, "/api/audit?action=agent.enrol_refused", nil)
		return code == http.StatusOK && strings.Contains(string(body), "engine_unreachable"), string(body)
	})
	got, ok := srv.Environment(env.ID)
	if !ok || got.Status.State != "pending" || got.Engine.Version != "" {
		t.Errorf("environment = %s, want it still pending: no agent was accepted", summarise(got))
	}
	if agents := srv.Agents(env.ID); len(agents) != 0 {
		t.Errorf("agents = %+v, want none enrolled", agents)
	}
}

// TestStandaloneFailingAndManyContainers reads a standalone host through its agent
// when it holds a crash-looping container with a restart policy and more containers
// than a list answers with. The failing count needs /containers/{id}/json, which
// the agent's allowlist must let through; the list is capped at 500 rows and says
// how many there really are (02-20, 02-21, 02-22).
func TestStandaloneFailingAndManyContainers(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	// A container with a restart policy that fails at once, over and over: the
	// agent image started with a flag it does not know exits with an error.
	sh(t, host, "docker run -d --name flaky --restart unless-stopped "+env.Install.Image+" --no-such-flag")
	srv.WaitFor("one failing container of two with a restart policy", 90*time.Second, func() (bool, string) {
		e, ok := srv.Environment(env.ID)
		return ok && e.Failing != nil && e.Failing.Failing == 1 && e.Failing.Total == 2, summarise(e)
	})

	// 520 more containers, never started: with the two above, 522.
	started := time.Now()
	sh(t, host, "i=1; while [ $i -le 520 ]; do docker create --name bulk$i "+env.Install.Image+" >/dev/null || exit 1; i=$((i+1)); done")
	t.Logf("created 520 containers in %s", time.Since(started).Round(time.Second))
	var list servertest.ContainerList
	srv.WaitFor("the capped container list", 60*time.Second, func() (bool, string) {
		l, code, body := srv.Containers(env.ID, "")
		list = l
		return code == http.StatusOK && l.Total == 522, string(body[:min(len(body), 300)])
	})
	if len(list.Items) != 500 || !list.Truncated {
		t.Errorf("list = %d rows, truncated %v, want 500 rows and truncated", len(list.Items), list.Truncated)
	}
	readStart := time.Now()
	if _, code, _ := srv.Containers(env.ID, ""); code != http.StatusOK {
		t.Errorf("list code = %d", code)
	}
	t.Logf("a 522-container list read through the tunnel took %s", time.Since(readStart).Round(time.Millisecond))
	if sum, code, body := srv.Summary(env.ID); code != http.StatusOK || sum.Counts.Containers != 522 {
		t.Errorf("summary containers = %d (%d): %.300s, want the full 522 even though the list is capped", sum.Counts.Containers, code, body)
	}
	// The failing count never overstates while the policies of the new containers are read, a few at a time.
	if e, _ := srv.Environment(env.ID); e.Failing == nil || e.Failing.Failing != 1 {
		t.Errorf("failing = %+v, want one failing container", e.Failing)
	}
}

// TestStandaloneAgentRestartsWithoutAJoinToken recreates an enrolled standalone
// agent the way docs/agent-updates.md tells an operator to update one by hand:
// the same hardening flags (every capability dropped but SETUID and SETGID), the
// same state volume, and no join token. It must come back as the same agent with
// no new enrolment (G-02-5, D-03). Before the fix the agent read its identity
// file as root without CAP_DAC_OVERRIDE, was refused by the 0700 state
// directory, and exited 2 in a loop.
func TestStandaloneAgentRestartsWithoutAJoinToken(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	identityOf := func() string {
		out := sh(t, host, `cat "$(docker volume inspect -f '{{.Mountpoint}}' coaming-agent)/identity.json"`)
		var id struct {
			AgentID string `json:"agentId"`
		}
		if err := json.Unmarshal([]byte(out), &id); err != nil || id.AgentID == "" {
			t.Fatalf("identity.json = %q (%v), want an agentId", out, err)
		}
		return id.AgentID
	}
	agentID := identityOf()
	enrolled := len(srv.AuditRows("agent.enrolled"))
	if enrolled != 1 {
		t.Fatalf("agent.enrolled audit rows after the install = %d, want 1", enrolled)
	}
	before := srv.Agents(env.ID)
	if len(before) != 1 {
		t.Fatalf("agent rows after the install = %+v, want 1", before)
	}
	oldContainer := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.Id}}' coaming-agent"))

	// The token-less update an operator is shown, minus its docker pull: the image
	// is loaded into the node already and ghcr.io does not resolve to a registry here.
	manual := agents.ManualUpdateCommand("standalone", env.Install.Image, agents.InstallParams{Endpoint: env.Install.AgentURL, Pin: env.Install.Pin})
	recreate, ok := strings.CutPrefix(manual, " docker pull "+env.Install.Image+" && ")
	if !ok {
		t.Fatalf("the manual update command does not start with its docker pull: %q", manual)
	}
	for _, want := range []string{"--cap-drop ALL --cap-add SETUID --cap-add SETGID", "-v coaming-agent:/var/lib/coaming-agent"} {
		if !strings.Contains(recreate, want) {
			t.Fatalf("the recreate command lacks %q: %s", want, recreate)
		}
	}
	if strings.Contains(recreate, "JOIN_TOKEN") {
		t.Fatalf("the recreate command carries a join token: %s", recreate)
	}
	sh(t, host, recreate)

	var log string
	srv.WaitFor("the recreated agent to connect as the same agent", 90*time.Second, func() (bool, string) {
		id, _ := host.Sh(execCtx(t, time.Minute), "docker inspect -f '{{.Id}}' coaming-agent")
		out, _ := host.Sh(execCtx(t, time.Minute), "docker logs coaming-agent 2>&1")
		log = out
		ok := strings.TrimSpace(id) != "" && strings.TrimSpace(id) != oldContainer &&
			strings.Contains(out, "connected to Coaming") && strings.Contains(out, "agent_id="+agentID)
		return ok, out[max(0, len(out)-600):]
	})

	if n := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.RestartCount}}' coaming-agent")); n != "0" {
		t.Errorf("RestartCount = %s, want 0: the agent started at once", n)
	}
	capDrop := strings.ReplaceAll(strings.TrimSpace(sh(t, host, "docker inspect -f '{{.HostConfig.CapDrop}}' coaming-agent")), "CAP_", "")
	if capDrop != "[ALL]" {
		t.Errorf("CapDrop = %s, want [ALL] (D-22)", capDrop)
	}
	for _, want := range []string{"token_present=false", "enrolled=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("the agent log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "enrolled with Coaming") {
		t.Errorf("the agent enrolled again instead of reconnecting:\n%s", log)
	}

	srv.WaitOnline(env.ID, 60*time.Second)
	after := srv.Agents(env.ID)
	if len(after) != 1 || !after[0].Connected || after[0].ID != before[0].ID {
		t.Errorf("agents after the recreate = %+v, want the one agent %s connected", after, before[0].ID)
	}
	if got := identityOf(); got != agentID {
		t.Errorf("identity.json names agent %s after the recreate, want %s", got, agentID)
	}
	if got := len(srv.AuditRows("agent.enrolled")); got != enrolled {
		t.Errorf("agent.enrolled audit rows = %d after the recreate, want the same %d", got, enrolled)
	}
}
