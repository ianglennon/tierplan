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

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/server/servertest"
)

// actionResponse is the body of POST /api/env/{id}/containers/actions as far as
// these tests read it.
type actionResponse struct {
	Results []struct {
		NodeID string `json:"nodeId"`
		ID     string `json:"id"`
		Name   string `json:"name"`
		Result string `json:"result"`
		Code   string `json:"code"`
	} `json:"results"`
}

// act stops or starts one container through the real endpoint and returns the one
// result it answers.
func act(t *testing.T, srv *servertest.Server, envID, action, nodeID, id string) (result, code string) {
	t.Helper()
	status, body := srv.Do(http.MethodPost, "/api/env/"+envID+"/containers/actions", map[string]any{
		"action":  action,
		"targets": []map[string]string{{"nodeId": nodeID, "id": id}},
	})
	if status != http.StatusOK {
		t.Fatalf("POST actions %s = %d %s", action, status, body)
	}
	var r actionResponse
	if err := json.Unmarshal(body, &r); err != nil || len(r.Results) != 1 {
		t.Fatalf("actions answer %s: %v", body, err)
	}
	return r.Results[0].Result, r.Results[0].Code
}

// TestStandaloneAgentContainerActions stops and starts a container on a standalone
// host through a real agent built from this branch: the write crosses the tunnel and
// the agent's route table, and the container's state changes (plan 04-03, RES-01).
// A second host whose agent runs with COAMING_AGENT_READ_ONLY=true is refused by name
// before any call, and its container keeps running (D-35, Pitfall 3).
func TestStandaloneAgentContainerActions(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dockertest.LoadImage(t, "alpine", host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})

	env := srv.CreateAgentEnvironment("")
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)
	agents := srv.Agents(env.ID)
	if len(agents) != 1 {
		t.Fatalf("agents = %+v, want the one that enrolled", agents)
	}
	node := agents[0].ID

	sh(t, host, "docker run -d --name web alpine sleep 600")
	webID := strings.TrimSpace(sh(t, host, "docker inspect -f '{{.Id}}' web"))
	state := func() string {
		return strings.TrimSpace(sh(t, host, "docker inspect -f '{{.State.Status}}' web"))
	}
	if got := state(); got != "running" {
		t.Fatalf("web is %q before the test acts, want running", got)
	}

	if res, code := act(t, srv, env.ID, "stop", node, webID); res != "done" {
		t.Fatalf("stop = %q (%q), want done", res, code)
	}
	if got := state(); got != "exited" {
		t.Errorf("web is %q after the stop, want exited", got)
	}
	if res, code := act(t, srv, env.ID, "start", node, webID); res != "done" {
		t.Fatalf("start = %q (%q), want done", res, code)
	}
	if got := state(); got != "running" {
		t.Errorf("web is %q after the start, want running", got)
	}

	rows := append(srv.AuditRows("container.stopped"), srv.AuditRows("container.started")...)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want one for the stop and one for the start", rows)
	}
	for _, r := range rows {
		if r.Result != "success" || r.TargetID != webID {
			t.Errorf("audit row = %+v, want a success naming the container", r)
		}
	}

	// A host whose owner set the read-only flag: the refusal is named before any call.
	ro := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, ro)
	dockertest.LoadImage(t, "alpine", ro)
	dumpOnFailure(t, ro)
	roEnv := srv.CreateAgentEnvironment("")
	cmd := strings.Replace(roEnv.Install.Standalone.Command, " "+roEnv.Install.Image, " -e COAMING_AGENT_READ_ONLY=true "+roEnv.Install.Image, 1)
	if cmd == roEnv.Install.Standalone.Command {
		t.Fatal("could not add the read-only flag to the command")
	}
	sh(t, ro, cmd)
	srv.WaitOnline(roEnv.ID, 60*time.Second)
	roAgents := srv.Agents(roEnv.ID)
	if len(roAgents) != 1 {
		t.Fatalf("read-only agents = %+v", roAgents)
	}
	sh(t, ro, "docker run -d --name web alpine sleep 600")
	roWeb := strings.TrimSpace(sh(t, ro, "docker inspect -f '{{.Id}}' web"))

	res, code := act(t, srv, roEnv.ID, "stop", roAgents[0].ID, roWeb)
	if res != "failed" || code != "host_read_only" {
		t.Fatalf("stop on the read-only host = %q (%q), want failed with host_read_only", res, code)
	}
	if got := strings.TrimSpace(sh(t, ro, "docker inspect -f '{{.State.Status}}' web")); got != "running" {
		t.Errorf("web on the read-only host is %q, want it untouched and running", got)
	}
}
