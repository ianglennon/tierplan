// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/registry"
)

// sharedNodeIDTemplate is the swarm leg of spike S6 (D-01, D-30), through the real agent: a global service whose env holds
// {{.Node.ID}} and {{.Node.Hostname}} is created through TierPlan's create route, so it travels to the manager's agent and
// through the service-body validator. The swarm holds the literal templates, and on each of the four nodes the task
// container sees that node's own swarm node ID.
func sharedNodeIDTemplate(t *testing.T, f *swarmFix) {
	const user, password = "bot", "node-id-pass-0123456789" //nolint:gosec // G101: a throwaway login for the fixture registry
	const (
		nodeIDEnv   = "TIERPLAN_INGRESS_NODE_ID={{.Node.ID}}"
		nodeNameEnv = "TIERPLAN_INGRESS_NODE_NAME={{.Node.Hostname}}"
		svcName     = "tp-s6-node-id"
	)
	mgr := f.sw.Managers[0]
	dockertest.LoadImage(t, dockertest.ServiceTestImage, f.sw.Nodes()...)
	// The image comes from a fixture registry, as in the service lifecycle, so the create route resolves its digest and
	// no step reaches Docker Hub.
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{Htpasswd: dockertest.HtpasswdFor(user, password)})
	ref := addr + "/s6/busybox:1"
	login := &dockertest.Login{User: user, Password: password}
	dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, ref, "s6", login)
	dockertest.Logout(t, mgr)
	f.install(t)
	f.waitSwarmOnline(t, 4)

	creds := registry.NewStore(f.srv.Core().Store(), f.srv.Core().Vault(), nil)
	if _, err := creds.Create(context.Background(), registry.CreateInput{
		Name: "s6-bot", Type: registry.TypeGeneric, Host: addr, Prefix: "s6", Username: user, Secret: password,
	}, "test", nil); err != nil {
		t.Fatal(err)
	}

	spec := quickServiceSpec(svcName, ref, 1)
	spec.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}
	spec.TaskTemplate.ContainerSpec.Env = []string{nodeIDEnv, nodeNameEnv}
	code, body := f.createService(t, spec)
	if code != http.StatusCreated {
		t.Fatalf("POST services = %d %s", code, body)
	}
	var made createdService
	if err := json.Unmarshal(body, &made); err != nil || made.ID == "" {
		t.Fatalf("created service = %s (%v)", body, err)
	}
	t.Cleanup(func() {
		// Leave the swarm as the case found it, whichever way the case ended.
		f.srv.Do(http.MethodDelete, "/api/env/"+f.env.ID+"/services/"+made.ID, nil)
	})

	// The swarm holds the literal templates: the expansion is per task, on the node.
	env, _ := containerSpecOf(t, f.specOf(t, made.ID).Spec)["Env"].([]any)
	var got []string
	for _, e := range env {
		s, _ := e.(string)
		got = append(got, s)
	}
	if want := []string{nodeIDEnv, nodeNameEnv}; !slices.Equal(got, want) {
		t.Fatalf("the spec env = %q, want the literal templates %q", got, want)
	}

	seen := map[string]bool{}
	for _, n := range f.sw.Nodes() {
		info, err := n.Info(execCtx(t, time.Minute), t)
		if err != nil {
			t.Fatal(err)
		}
		var ctr string
		dockertest.Wait(t, taskWait, "the task container of "+svcName+" on "+n.Hostname, func() (bool, string) {
			out, err := n.Sh(execCtx(t, time.Minute), "docker ps -q --filter label=com.docker.swarm.service.name="+svcName)
			if err != nil {
				return false, err.Error()
			}
			ids := strings.Fields(out)
			if len(ids) != 1 {
				return false, "containers: " + strings.TrimSpace(out)
			}
			ctr = ids[0]
			return true, ""
		})
		gotID := strings.TrimSpace(sh(t, n, "docker exec "+ctr+" printenv TIERPLAN_INGRESS_NODE_ID"))
		gotName := strings.TrimSpace(sh(t, n, "docker exec "+ctr+" printenv TIERPLAN_INGRESS_NODE_NAME"))
		t.Logf("engine %s node %s: node ID %s; task saw TIERPLAN_INGRESS_NODE_ID=%q TIERPLAN_INGRESS_NODE_NAME=%q", f.sw.Engine, n.Hostname, info.NodeID, gotID, gotName)
		if gotID != info.NodeID || gotName != info.Name {
			t.Errorf("%s: the task saw node ID %q and name %q, want %q and %q", n.Hostname, gotID, gotName, info.NodeID, info.Name)
		}
		seen[gotID] = true
	}
	if len(seen) != 4 {
		t.Errorf("the four nodes' tasks saw %d distinct node IDs, want 4", len(seen))
	}
}
