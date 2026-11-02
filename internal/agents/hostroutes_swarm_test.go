// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/proto/testsupport"
	"github.com/ianglennon/tierplan/proto/wire"
)

// sharedHostRoutes proves RESEARCH Assumption A2 on a real swarm: a service accepts the bind mount of the single
// procfs file /proc/1/net/route the generated swarm command carries, every agent reads it (hostNet "ok"), and a
// worker's routes come back through the server and include its docker_gwbridge subnet (D-17, NET-05). The command
// is run exactly as the server generated it.
func sharedHostRoutes(t *testing.T, f *swarmFix) {
	worker := f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)

	tun := f.srv.Core().Tunnel()
	f.srv.WaitFor("every agent to report that it can read its host's routes", installWait, func() (bool, string) {
		views := tun.Views(f.env.ID)
		ok := len(views) == 4
		var seen []string
		for _, v := range views {
			seen = append(seen, v.AgentID+"="+v.HostNet)
			if v.HostNet != wire.HostNetOK {
				ok = false
			}
		}
		return ok, strings.Join(seen, " ")
	})

	// The worker's agent, by the node it reports.
	var workerAgent string
	for _, a := range f.srv.Agents(f.env.ID) {
		if a.Node == worker.Hostname {
			workerAgent = a.ID
		}
	}
	if workerAgent == "" {
		t.Fatalf("no agent row for the worker %s: %+v", worker.Hostname, f.srv.Agents(f.env.ID))
	}

	// The subnet the worker's own docker gives docker_gwbridge: the route table must hold it, on that interface.
	gw := strings.TrimSpace(sh(t, worker, "docker network inspect docker_gwbridge -f '{{(index .IPAM.Config 0).Subnet}}'"))
	if gw == "" {
		t.Fatal("the worker reports no subnet for docker_gwbridge")
	}

	ctx := testsupport.Context(t)
	doc, err := tun.HostNet(ctx, workerAgent)
	if err != nil {
		t.Fatalf("read the worker's routes through the server: %v", err)
	}
	if doc.Error != "" {
		t.Fatalf("the worker's agent answered %q instead of routes", doc.Error)
	}
	found := false
	for _, r := range doc.Routes {
		if r.Iface == "docker_gwbridge" && r.Dst == gw {
			found = true
		}
	}
	if !found {
		t.Errorf("the worker's routes hold no docker_gwbridge route for %s: %+v", gw, doc.Routes)
	}

	// The mount is what the command said it was: one file, read-only, and nothing else of the host's /proc.
	for _, line := range strings.Split(strings.TrimSpace(sh(t, f.sw.Managers[0],
		"docker service inspect tierplan-agent -f '{{range .Spec.TaskTemplate.ContainerSpec.Mounts}}{{.Type}} {{.Source}} {{.Target}} ro={{.ReadOnly}}{{println}}{{end}}'")), "\n") {
		if strings.Contains(line, "/proc") && line != "bind /proc/1/net/route /host/route ro=true" {
			t.Errorf("the service mounts %q; only /proc/1/net/route read-only may be mounted from /proc", line)
		}
	}
}
