// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/agents"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/server/servertest"
)

// rawSpec is `docker service inspect`'s Spec as generic JSON, so that a field this
// code has never heard of is compared too.
func rawSpec(t *testing.T, n *dockertest.Node, name string) map[string]any {
	t.Helper()
	var spec map[string]any
	out := sh(t, n, "docker service inspect --format '{{json .Spec}}' "+name)
	if err := json.Unmarshal([]byte(out), &spec); err != nil {
		t.Fatalf("parse the spec of service %s: %v\n%s", name, err, out)
	}
	return spec
}

// takeImage takes the image out of a spec and returns it, leaving "" in its place.
func takeImage(t *testing.T, spec map[string]any) string {
	t.Helper()
	tt, _ := spec["TaskTemplate"].(map[string]any)
	cs, _ := tt["ContainerSpec"].(map[string]any)
	img, ok := cs["Image"].(string)
	if !ok {
		t.Fatalf("the spec has no TaskTemplate.ContainerSpec.Image: %v", spec)
	}
	cs["Image"] = ""
	return img
}

// flatten lists every leaf of a JSON value by its path, as path=value lines.
func flatten(prefix string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			flatten(prefix+"."+k, c, out)
		}
		if len(x) == 0 {
			out[prefix] = "{}"
		}
	case []any:
		for i, c := range x {
			flatten(fmt.Sprintf("%s[%d]", prefix, i), c, out)
		}
		if len(x) == 0 {
			out[prefix] = "[]"
		}
	default:
		b, _ := json.Marshal(x)
		out[prefix] = string(b)
	}
}

// specDiff is every path whose value differs between two specs, one per line.
func specDiff(a, b map[string]any) []string {
	fa, fb := map[string]string{}, map[string]string{}
	flatten("", a, fa)
	flatten("", b, fb)
	var out []string
	for k, va := range fa {
		if vb, ok := fb[k]; !ok {
			out = append(out, fmt.Sprintf("%s: %s -> (gone)", k, va))
		} else if va != vb {
			out = append(out, fmt.Sprintf("%s: %s -> %s", k, va, vb))
		}
	}
	for k, vb := range fb {
		if _, ok := fa[k]; !ok {
			out = append(out, fmt.Sprintf("%s: (absent) -> %s", k, vb))
		}
	}
	sort.Strings(out)
	return out
}

// TestSwarmSelfUpdate upgrades a server from v0.2.0 to v0.2.1 while a real swarm of
// three managers and a worker runs the v0.2.0 agent as a global service (D-24,
// RESEARCH Pitfall 19): the server sends one update, through a manager, the agent
// rolls its own service, and every node's agent comes back as v0.2.1. The service
// spec after the update is the spec before it in every field but the image, which is
// the guarantee that the agent's raw-JSON patch drops and invents nothing.
func TestSwarmSelfUpdate(t *testing.T) {
	sw := dockertest.StartSwarm(t, "", 3, 1)
	loadReleases(t, sw.Nodes()...)
	dumpOnFailure(t, sw.Nodes()...)
	srv := servertest.Start(t, servertest.Options{Host: sw.Gateway, Version: oldRelease})
	f := &swarmFix{sw: sw, srv: srv, env: srv.CreateAgentEnvironment("")}
	mgr := sw.Managers[0]

	if want := agents.AgentImage(oldRelease); f.env.Install.Image != want {
		t.Fatalf("install image = %q, want %q", f.env.Install.Image, want)
	}
	f.install(t)
	f.waitSwarmOnline(t, 4)
	before := rawSpec(t, mgr, agentName)
	imageBefore := takeImage(t, before)
	if !strings.HasPrefix(imageBefore, agents.AgentImage(oldRelease)) {
		t.Fatalf("the service runs %q, want %s", imageBefore, agents.AgentImage(oldRelease))
	}
	var idsBefore []string
	for _, a := range srv.Agents(f.env.ID) {
		if a.AgentVersion != oldRelease {
			t.Fatalf("agent %s runs %s before the upgrade, want %s", a.Node, a.AgentVersion, oldRelease)
		}
		idsBefore = append(idsBefore, a.ID)
	}
	slices.Sort(idsBefore)

	// The server is upgraded. Nobody asks for an update.
	srv.Restart(newRelease)
	srv.WaitFor("every agent to report "+newRelease, 3*time.Minute, func() (bool, string) {
		rows := srv.Agents(f.env.ID)
		if len(rows) != 4 {
			return false, summariseAgents(rows)
		}
		for _, a := range rows {
			if !a.Connected || a.AgentVersion != newRelease {
				return false, summariseAgents(rows)
			}
		}
		return true, ""
	})
	f.waitSwarmOnline(t, 4)

	after := rawSpec(t, mgr, agentName)
	imageAfter := takeImage(t, after)
	if !strings.HasPrefix(imageAfter, agents.AgentImage(newRelease)) {
		t.Errorf("after the update the service runs %q, want %s", imageAfter, agents.AgentImage(newRelease))
	}
	if diff := specDiff(before, after); len(diff) != 0 {
		t.Errorf("the service spec changed in more than its image (Pitfall 19):\n%s", strings.Join(diff, "\n"))
	}

	var idsAfter []string
	for _, a := range srv.Agents(f.env.ID) {
		idsAfter = append(idsAfter, a.ID)
		if a.UpdateState.State != "idle" {
			t.Errorf("update state of %s = %+v, want idle", a.Node, a.UpdateState)
		}
	}
	slices.Sort(idsAfter)
	if !slices.Equal(idsBefore, idsAfter) {
		t.Errorf("agent IDs before %v, after %v: the update enrolled someone anew", idsBefore, idsAfter)
	}

	// One message rolled the whole service, and it is recorded once, under the updater.
	updated := srv.AuditRows("agent.updated")
	if len(updated) != 1 || updated[0].ActorID != "system:agent-update" || updated[0].Details["image"] != agents.AgentImage(newRelease) || updated[0].Details["mode"] != "auto" {
		t.Errorf("agent.updated rows = %+v, want exactly one by system:agent-update", updated)
	}
	if failed := srv.AuditRows("agent.update_failed"); len(failed) != 0 {
		t.Errorf("agent.update_failed rows = %+v, want none", failed)
	}
}
