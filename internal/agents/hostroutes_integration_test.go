// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package agents_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/agents"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/server/servertest"
	"github.com/ianglennon/tierplan/proto/wire"
)

// TestStandaloneAgentReportsHostNet runs the generated standalone command on a dind engine and proves both
// states of the host-route check (D-17, NET-05): the agent started with the generated command reports hostNet
// "ok" and its routes come back through the server; the same agent recreated without the route mount reports
// "unmounted"; and the command an operator is shown for an agent installed before this release
// (HostRouteMountCommand) brings it back to "ok". The recreate keeps the state volume, so the same agent
// reconnects each time and no join token is used again.
func TestStandaloneAgentReportsHostNet(t *testing.T) {
	host := dockertest.StartHost(t, "")
	dockertest.LoadAgentImage(t, host)
	dumpOnFailure(t, host)
	srv := servertest.Start(t, servertest.Options{Host: host.Gateway})
	env := srv.CreateAgentEnvironment("")

	const mount = " -v /proc/1/net/route:/host/route:ro"
	if !strings.Contains(env.Install.Standalone.Command, mount) {
		t.Fatalf("the generated standalone command lacks the route mount:\n%s", env.Install.Standalone.Command)
	}
	sh(t, host, env.Install.Standalone.Command)
	srv.WaitOnline(env.ID, 60*time.Second)

	tun := srv.Core().Tunnel()
	// waitHostNet waits until the one agent of the environment is connected with a session that reported want.
	// A recreated agent connects again as the same agent, so the old session's answer must not be mistaken for
	// the new one: the container id changes first and the wait is for the report after that.
	waitHostNet := func(what, want string) string {
		t.Helper()
		var agentID string
		srv.WaitFor(what, 90*time.Second, func() (bool, string) {
			views := tun.Views(env.ID)
			if len(views) != 1 {
				return false, fmt.Sprintf("%d agent views, want 1", len(views))
			}
			agentID = views[0].AgentID
			return views[0].Connected && views[0].HostNet == want, fmt.Sprintf("connected=%v hostNet=%q", views[0].Connected, views[0].HostNet)
		})
		return agentID
	}

	agentID := waitHostNet("the agent installed with the generated command to report hostNet ok", wire.HostNetOK)
	doc, err := tun.HostNet(execCtx(t, time.Minute), agentID)
	if err != nil || doc.Error != "" {
		t.Fatalf("read the host's routes through the server: %v (error %q)", err, doc.Error)
	}
	// The engine's own bridge is a route on the node, whatever the engine version.
	gw := strings.TrimSpace(sh(t, host, "docker network inspect bridge -f '{{(index .IPAM.Config 0).Subnet}}'"))
	found := false
	for _, r := range doc.Routes {
		if r.Dst == gw {
			found = true
		}
	}
	if !found {
		t.Errorf("the routes hold nothing for the default bridge %s: %+v", gw, doc.Routes)
	}

	// The recreate command an operator is shown, with and without the route mount. The docker pull in front is
	// left out: the image is loaded into the node already and ghcr.io does not resolve to a registry here.
	fix := agents.HostRouteMountCommand("standalone", agents.InstallParams{Endpoint: env.Install.AgentURL, Pin: env.Install.Pin})
	_, withMount, ok := strings.Cut(fix, " && docker rm -f ")
	if !ok || !strings.Contains(withMount, strings.TrimSpace(mount)) {
		t.Fatalf("the fix command is not a pull and a recreate that carries the mount:\n%s", fix)
	}
	recreate := " docker rm -f " + withMount
	withoutMount := strings.Replace(recreate, mount, "", 1)
	if withoutMount == recreate {
		t.Fatalf("could not take the route mount out of:\n%s", recreate)
	}

	sh(t, host, withoutMount)
	if got := waitHostNet("the agent recreated without the mount to report unmounted", wire.HostNetUnmounted); got != agentID {
		t.Errorf("the recreated agent is %s, want the same agent %s", got, agentID)
	}
	if _, err := tun.HostNet(execCtx(t, time.Minute), agentID); err == nil {
		t.Error("the server asked an unmounted agent for its routes; it must not open the stream")
	}

	sh(t, host, recreate)
	if got := waitHostNet("the agent recreated with the fix command to report hostNet ok", wire.HostNetOK); got != agentID {
		t.Errorf("the recreated agent is %s, want the same agent %s", got, agentID)
	}
}
