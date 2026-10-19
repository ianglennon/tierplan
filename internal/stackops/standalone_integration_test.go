// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package stackops_test

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// TestStandaloneDeployOnARealEngine reviews and deploys a stack on a real engine through the Compose SDK, the
// way the server does for its own host (plan 07-20, STK-02, STK-06, STK-09): the environment is a local
// socket (a proxy to a docker:<N>-dind container), the image is in a registry of the node's own, and the SDK
// is the real one over Bayplan's client behind the guard. It shows that the first review lists the service as
// added with a new pin, that the deploy runs one container labelled with the project and the stack, that the
// config hash the review computes is the one the SDK stamps, that a redeploy nobody edited keeps the same
// container (even after the tag moved), and that an edit recreates it.
func TestStandaloneDeployOnARealEngine(t *testing.T) {
	// The SDK's process-wide state: its private configuration directory and its logging.
	if err := dockerx.InitCompose(t.TempDir(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("InitCompose: %v", err)
	}
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/bp/web:1"
	pushed := dockertest.PushImage(t, node, "busybox:1.36.1", image, nil)
	// Bayplan's own client to the node, for what the test reads back: the project's containers.
	cli, err := dockerx.New(dockertest.TCPTransportFor(node.Host()))
	if err != nil {
		t.Fatalf("dockerx.New: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	rig := newRigOver(t, dockertest.UnixProxy(t, node.Host()))
	rig.deps.ConvergeTimeout = 3 * time.Minute
	pins := &memPins{}
	const name = "itest"
	compose := func(port string) string {
		return "services:\n  web:\n    image: " + image + "\n    command: [\"httpd\", \"-f\", \"-p\", \"80\"]\n    ports:\n      - \"" + port + ":80\"\n"
	}
	ctx := testsupport.Context(t)
	containers := func() []dockerx.ContainerRow {
		t.Helper()
		rows, err := cli.ContainersWithLabel(ctx, stacks.LabelComposeProject, name)
		if err != nil {
			t.Fatalf("list the project's containers: %v", err)
		}
		return rows
	}

	// 1. The review of a stack nobody deployed: the service is added with a pin the registry gave.
	rv, err := rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18080"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"added web"}) || rv.Problems.Errors() != 0 {
		t.Fatalf("first review: services %v problems %v, want web added and nothing blocking", got, rv.Problems)
	}
	wantPin := image + "@" + pushed
	if rv.Pins["web"] != wantPin || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageNew {
		t.Fatalf("first review: pins %v images %+v, want a new pin %s", rv.Pins, rv.Images, wantPin)
	}

	// 2. The deploy, with the review's pins: one container, labelled, running; the hash is the review's.
	revision := 0
	deploy := func(compose string, reviewed map[string]string) stackops.DeployResult {
		t.Helper()
		revision++
		in := standaloneInput(name, compose)
		in.Pins, in.RevisionID, in.Reviewed = pins, "rev-"+strconv.Itoa(revision), reviewed
		res, job := rig.deployStandalone(t, in)
		if !res.OK() {
			var words []string
			for _, it := range job.Items {
				if it.Detail != "" {
					words = append(words, it.ID+": "+it.Detail)
				}
			}
			t.Fatalf("deploy: %+v items %v details %q", res, itemStates(job), words)
		}
		return res
	}
	res := deploy(compose("18080"), rv.Pins)
	if res.Services != 1 || res.Updated != 0 || res.Networks != 1 {
		t.Fatalf("first deploy = %+v, want one service and one network created", res)
	}
	first := containers()
	if len(first) != 1 || first[0].State != "running" {
		t.Fatalf("containers after the deploy = %+v, want one running", first)
	}
	labels := first[0].Labels
	if labels[stacks.LabelComposeProject] != name || labels[stacks.LabelStackID] != "stack-"+name ||
		labels[stacks.LabelComposeService] != "web" || labels[stacks.LabelStackImage] != image {
		t.Errorf("labels = %v, want the project, the stack's ID, the service and the image as written", labels)
	}
	if want := preparedHash(t, name, compose("18080"), map[string]string{"web": wantPin})["web"]; labels[stacks.LabelComposeConfigHash] != want {
		t.Errorf("config hash label = %q, want the hash the review computes %q", labels[stacks.LabelComposeConfigHash], want)
	}
	if first[0].Image != wantPin {
		t.Errorf("the container runs %q, want the pinned reference %q", first[0].Image, wantPin)
	}

	// 3. Right after the deploy the review has nothing to do, and the digest is kept.
	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18080"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImagePinned {
		t.Fatalf("review after the deploy: services %v images %+v, want nothing to do and web pinned", changesOf(rv), rv.Images)
	}

	// 4. A redeploy nobody edited keeps the container, even after the tag moved to another image.
	otherDigest := dockertest.PushImage(t, node, "alpine:3", image, nil)
	if otherDigest == pushed {
		t.Fatalf("the tag did not move: %s", otherDigest)
	}
	res = deploy(compose("18080"), nil)
	if res.Services != 0 || res.Updated != 0 {
		t.Errorf("redeploy = %+v, want nothing created or updated", res)
	}
	again := containers()
	if len(again) != 1 || again[0].ID != first[0].ID || again[0].Image != wantPin {
		t.Fatalf("containers after a redeploy = %+v, want the same container %s on the same pin", again, first[0].ID)
	}

	// 5. An edit recreates it: the review says changed, the deploy replaces the container.
	rv, err = rig.deps.Review(ctx, rig.standaloneReviewInput(t, name, compose("18081"), pins))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("review of an edit: %v, want web changed", got)
	}
	res = deploy(compose("18081"), rv.Pins)
	if res.Services != 0 || res.Updated != 1 {
		t.Errorf("deploy of an edit = %+v, want one service updated", res)
	}
	edited := containers()
	if len(edited) != 1 || edited[0].ID == first[0].ID || edited[0].State != "running" {
		t.Fatalf("containers after an edit = %+v, want one new running container (was %s)", edited, first[0].ID)
	}
	if !strings.HasSuffix(edited[0].Image, pushed) {
		t.Errorf("the recreated container runs %q, want the pin %s kept through the edit", edited[0].Image, pushed)
	}
}
