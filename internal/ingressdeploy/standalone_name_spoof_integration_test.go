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

	// A deploy that skips the pre-check still ends refused and touches nothing.
	st, _, err := e.repo.CreateStack(ctx, e.envID, "spoof", stacks.TargetStandalone, "user:test", copy)
	if err != nil {
		t.Fatalf("CreateStack: %v", err)
	}
	rev, err := e.repo.NewRevision(ctx, st.ID, "user:test", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	_, res, _ := e.runDeploy(st, rev, stackops.StandaloneDeployInput{
		EnvID: e.envID, NodeID: e.nodeID, Stack: st, Copy: copy, Pins: e.repo, Subnets: e.repo, RevisionID: rev.ID, Caller: e.rootCaller(),
	})
	if res.OK() || res.Code != stackops.CodeStackInvalid {
		t.Fatalf("the spoof deploy ended %+v, want %s", res, stackops.CodeStackInvalid)
	}
	if got := strings.TrimSpace(e.sh("docker ps -a --filter name=^" + alias + "$ --format '{{.Names}}'")); got != "" {
		t.Errorf("a container named %s exists on the host: %q", alias, got)
	}
	for range 4 {
		if page := e.awaitServed(zooHost); !strings.Contains(page, webBody("zoo")) || strings.Contains(page, "spoofed") {
			t.Fatalf("%s answered %q after the spoof attempt, want zoo's web", zooHost, strings.TrimSpace(page))
		}
	}
}
