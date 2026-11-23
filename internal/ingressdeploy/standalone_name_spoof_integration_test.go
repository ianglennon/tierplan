// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package ingressdeploy_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestStandaloneContainerNameCannotTakeARoute is T-25-47 on a real engine: the engine resolves a container's own name on
// the shared bridge, and zoo's web is reached there by ingress.Alias("zoo", "web"), which anyone can compute. A second
// stack that joins tierplan-ingress as an external network and names a container after that alias is refused before
// anything reaches the host (Check, ingress_container_name; the deploy guard is the backstop), and zoo's hostname keeps
// answering with zoo's page.
func TestStandaloneContainerNameCannotTakeARoute(t *testing.T) {
	e := startHostEnv(t, false)
	e.startRegistry()
	requireJobDone(t, e.enable())
	e.awaitReady()

	zooHost := "zoo." + testDomain
	if _, res, job := e.deployStack("zoo", "services:\n"+e.routedService("web", webBody("zoo"), zooHost)); !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the deploy of zoo ended %+v", res)
	}
	e.uploadCertificate(zooHost)
	if page := e.awaitServed(zooHost); !strings.Contains(page, webBody("zoo")) {
		t.Fatalf("%s answered %q, want zoo's web", zooHost, strings.TrimSpace(page))
	}

	alias := ingress.Alias("zoo", "web")
	spoof := "services:\n  web:\n    image: " + e.webImage + "\n    command: " + backendCommand("spoofed") + "\n" +
		"    container_name: " + alias + "\n    networks: [edge]\n" +
		"networks:\n  edge:\n    external: true\n    name: " + ingress.NetworkName + "\n"

	// The check refuses the file at container_name.
	ctx := testsupport.Context(t)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(spoof)}}}
	attach, err := e.ops.AttachFor(ctx, e.envID, stacks.TargetStandalone)
	if err != nil {
		t.Fatalf("AttachFor: %v", err)
	}
	checked := stacks.Check(ctx, stacks.CheckInput{Name: "spoof", Target: stacks.TargetStandalone, Copy: copy, Ingress: attach})
	refused := false
	for _, p := range checked.Problems {
		if p.Code == stacks.CodeIngressContainerName && p.Key == "services.web.container_name" {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("the check gave %+v, want ingress_container_name at services.web.container_name", checked.Problems)
	}

	// A deploy that skips the pre-check still ends refused and touches nothing, and so does each way round the check the
	// re-audit found: the network as network mode, the network declared by its ID (which only the guard can see), and a
	// hostname instead of a container name (the engine answers a hostname on a user bridge since Engine 25).
	ingressID := strings.TrimSpace(e.sh("docker network inspect " + ingress.NetworkName + " --format '{{.ID}}'"))
	svc := func(extra string) string {
		return "services:\n  web:\n    image: " + e.webImage + "\n    command: " + backendCommand("spoofed") + "\n" + extra
	}
	for _, tc := range []struct{ stack, compose string }{
		{"spoof", spoof},
		{"spoofmode", svc("    container_name: " + alias + "\n    network_mode: " + ingress.NetworkName + "\n")},
		{"spoofid", svc("    container_name: "+alias+"\n    networks: [edge]\n") +
			"networks:\n  edge:\n    external: true\n    name: " + ingressID + "\n"},
		{"spoofhn", svc("    hostname: "+alias+"\n    networks: [edge]\n") +
			"networks:\n  edge:\n    external: true\n    name: " + ingress.NetworkName + "\n"},
	} {
		copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(tc.compose)}}}
		st, _, err := e.repo.CreateStack(ctx, e.envID, tc.stack, stacks.TargetStandalone, "user:test", copy)
		if err != nil {
			t.Fatalf("CreateStack %s: %v", tc.stack, err)
		}
		rev, err := e.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
		if err != nil {
			t.Fatalf("NewRevision: %v", err)
		}
		_, res, _ := e.runDeploy(st, rev, stackops.StandaloneDeployInput{
			EnvID: e.envID, NodeID: e.nodeID, Stack: st, Copy: copy, Pins: e.repo, Subnets: e.repo, RevisionID: rev.ID, Caller: e.rootCaller(),
		})
		t.Logf("%s: code %q, failed item %q, wrote %v", tc.stack, res.Code, res.FailedService, res.Wrote)
		if res.OK() {
			t.Errorf("the %s deploy succeeded, want it refused", tc.stack)
		}
		running := strings.TrimSpace(e.sh("docker ps -q --filter label=com.docker.compose.project=" + tc.stack))
		if running != "" {
			t.Errorf("the %s deploy left a running container on the host: %q", tc.stack, running)
		}
	}
	// The engine looks a network up again when a container starts: by name, then by ID prefix. A first revision makes a
	// decoy network named with the start of the ingress network's ID; the second renames it away and puts a container
	// named after zoo's alias on that name, behind a service it waits for, so the decoy is gone by the time the container
	// starts and the name now reaches the ingress network. The guard takes any prefix of the ingress ID as the ingress
	// network, so the second revision is refused (T-25-47, second re-audit).
	prefix := ingressID[:12]
	racer, res, job := e.deployStack("racer", "services:\n  holder:\n    image: "+e.webImage+"\n    command: [\"sleep\", \"3600\"]\n"+
		"    networks: [shadow]\nnetworks:\n  shadow:\n    name: "+prefix+"\n")
	if !res.OK() {
		requireJobDone(t, job)
		t.Fatalf("the first revision of racer ended %+v", res)
	}
	rev2 := "services:\n  holder:\n    image: " + e.webImage + "\n    command: [\"sleep\", \"3600\"]\n    networks: [shadow]\n" +
		"  gate:\n    image: " + e.webImage + "\n    command: [\"sleep\", \"8\"]\n    restart: \"no\"\n" +
		"  web:\n    image: " + e.webImage + "\n    command: " + backendCommand("spoofed") + "\n    container_name: " + alias + "\n    networks: [edge]\n" +
		"    depends_on:\n      gate:\n        condition: service_completed_successfully\n" +
		"networks:\n  shadow:\n    name: racer-other\n  edge:\n    external: true\n    name: " + prefix + "\n"
	_, res, _ = e.redeploy(racer, rev2)
	t.Logf("racer second revision: code %q, wrote %v", res.Code, res.Wrote)
	if res.OK() {
		t.Errorf("the second revision of racer succeeded, want it refused")
	}
	if got := strings.TrimSpace(e.sh("docker ps -a --filter name=^" + alias + "$ --format '{{.Names}}'")); got != "" {
		t.Errorf("a container named %s exists on the host: %q", alias, got)
	}
	for range 8 {
		if page := e.awaitServed(zooHost); !strings.Contains(page, webBody("zoo")) || strings.Contains(page, "spoofed") {
			t.Fatalf("%s answered %q after the spoof attempts, want zoo's web", zooHost, strings.TrimSpace(page))
		}
	}
}
