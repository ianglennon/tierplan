// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	mobycontainer "github.com/moby/moby/api/types/container"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// Drift of a standalone stack (plan 08-20, STK-14, D-21, RESEARCH Pattern 6): the deployed revision, prepared with its
// stored pins, against the containers of the project on the fake host, by config hash and field diff, with the image of
// each container checked against the revision's pin because the field diff leaves the image out.

const driftHostedCompose = `services:
  web:
    image: nginx:1.27-alpine
    ports:
      - "8080:80"
    environment:
      MODE: prod
    restart: unless-stopped
  worker:
    image: busybox:1.36
    command: ["sleep", "1000"]
`

// standaloneDriftRig is a stack deployed from driftHostedCompose on a fake host, and the input of a drift check of it.
type standaloneDriftRig struct {
	*swarmRig
	h  *stackopstest.Hosted
	st stackops.Stack
}

// hostedDrift stands the stack up on a fake host after edit has changed what the host holds (a document, a container),
// as somebody outside Bayplan would. The Compose SDK is a stand-in that fails the test if it is asked for anything.
func hostedDrift(t *testing.T, compose string, edit func(*stackopstest.Hosted)) *standaloneDriftRig {
	t.Helper()
	h := hostStack(t, "demo", compose, nil)
	if edit != nil {
		edit(h)
	}
	rig := &standaloneDriftRig{swarmRig: hostRig(t, h), h: h, st: standaloneStackOf("demo")}
	refuse := func(what string) {
		t.Errorf("a drift check asked the Compose SDK to %s", what)
	}
	rig.deps.Up = func(context.Context, *dockerx.Client, *types.Project, dockerx.ComposeUpOptions, func(dockerx.ComposeEvent)) error {
		refuse("up")
		return nil
	}
	rig.deps.StopProject = func(context.Context, *dockerx.Client, *types.Project) error { refuse("stop"); return nil }
	rig.deps.StartProject = func(context.Context, *dockerx.Client, *types.Project) error { refuse("start"); return nil }
	rig.deps.DownProject = func(context.Context, *dockerx.Client, string) error { refuse("down"); return nil }
	return rig
}

func (r *standaloneDriftRig) input() stackops.DriftInput {
	rev := stackops.Snapshot{
		ID: "rev-1", StackID: r.st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(driftHostedCompose)}}},
	}
	return stackops.DriftInput{EnvID: r.envID, Stack: r.st, Deployed: &rev, Pins: maps.Clone(r.h.Pins)}
}

func (r *standaloneDriftRig) drift(t *testing.T) stackops.DriftResult {
	t.Helper()
	res, err := r.deps.Drift(testsupport.Context(t), r.input())
	if err != nil {
		t.Fatalf("drift: %v", err)
	}
	return res
}

// secondContainer adds a container of service to the host, made from the first one the way `docker compose up --scale`
// makes it: the same hash, the next number.
func secondContainer(h *stackopstest.Hosted, service string) {
	h.Containers = append(h.Containers, cloneContainer(h, service, service, 2))
}

// cloneContainer is the container of from, as a container of service number n of the same project.
func cloneContainer(h *stackopstest.Hosted, from, service string, n int) dockertest.FakeContainer {
	var src dockertest.FakeContainer
	for _, c := range h.Containers {
		if c.ID == h.IDs[from] {
			src = c
		}
	}
	doc := *h.Docs[from]
	cfg := *doc.Config
	cfg.Labels = maps.Clone(cfg.Labels)
	cfg.Labels["com.docker.compose.service"] = service
	cfg.Labels["com.docker.compose.container-number"] = fmt.Sprint(n)
	doc.Config = &cfg
	doc.ID = fmt.Sprintf("%064x", len(h.Containers)+100*n)
	doc.Name = "/" + h.Name + "-" + service + "-" + fmt.Sprint(n)
	src.ID, src.Names, src.Labels = doc.ID, []string{doc.Name}, cfg.Labels
	src.Inspect = dockertest.FakeInspect{Document: &doc, ImageID: src.ImageID}
	return src
}

func withoutService(h *stackopstest.Hosted, service string) {
	h.Containers = slices.DeleteFunc(h.Containers, func(c dockertest.FakeContainer) bool {
		return c.Labels["com.docker.compose.service"] == service
	})
}

func TestStandaloneDriftHandChangedContainer(t *testing.T) {
	t.Parallel()

	// Right after a deploy the host holds what the revision deployed.
	if res := hostedDrift(t, driftHostedCompose, nil).drift(t); res.State != stackops.DriftNone || len(res.Services) != 0 || res.Count != 0 || res.Revision != 1 || res.CheckedAt.IsZero() {
		t.Fatalf("drift of what was just deployed = %+v, want none at revision 1", res)
	}

	// docker update --restart always: every compose label and the config hash stay, and the environment is edited in
	// place as `docker run` would have it.
	rig := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) {
		h.Docs["web"].Config.Env = append(h.Docs["web"].Config.Env, "EXTRA=1")
		h.Docs["web"].HostConfig.RestartPolicy = mobycontainer.RestartPolicy{Name: "always"}
	})

	res := rig.drift(t)

	if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) {
		t.Fatalf("drift = %s %v, want drifted with web changed", res.State, driftNames(res))
	}
	web := res.Services[0]
	if web.Name != "web" || web.Diff.Settings != 2 || res.Count != 2 {
		t.Errorf("service = %+v count %d, want web with the two settings counted", web, res.Count)
	}
	// Current is what runs now, Next what the revision deployed.
	if cur := string(web.Diff.Current); !strings.Contains(cur, "EXTRA") || !strings.Contains(cur, `"restart":"always"`) {
		t.Errorf("what runs now = %s, want the environment entry and the restart policy", cur)
	}
	if next := string(web.Diff.Next); strings.Contains(next, "EXTRA") || !strings.Contains(next, `"restart":"unless-stopped"`) {
		t.Errorf("what was deployed = %s, want the revision's restart policy and no EXTRA", next)
	}
	if !strings.HasPrefix(res.Fingerprint, "rev1,") {
		t.Errorf("fingerprint = %q, want the revision and the containers", res.Fingerprint)
	}
}

func TestStandaloneDriftImage(t *testing.T) {
	t.Parallel()
	other := "nginx:1.27-alpine@" + stackopstest.Digest("e")

	// The container was made from another digest of the same tag; nothing else of it differs, so the field diff, which
	// leaves the image out, sees nothing.
	rig := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) { h.Docs["web"].Config.Image = other })
	res := rig.drift(t)

	if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) || res.Count != 1 {
		t.Fatalf("drift = %s %v count %d, want drifted with web changed at its image alone", res.State, driftNames(res), res.Count)
	}
	web := res.Services[0]
	if !strings.Contains(string(web.Diff.Current), strings.Repeat("e", 64)) || strings.Contains(string(web.Diff.Next), strings.Repeat("e", 64)) {
		t.Errorf("current = %s\nnext = %s, want the running digest as current and the pinned one as next", web.Diff.Current, web.Diff.Next)
	}
	if !strings.Contains(string(web.Diff.Next), `"image":"nginx:1.27-alpine@`+stackopstest.Digest("a")) {
		t.Errorf("next = %s, want the revision's pin at the image path", web.Diff.Next)
	}

	// An image that is only the same pin written another way is no drift: the container was made with the tag and the
	// image it runs carries the pinned digest.
	same := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) { h.Docs["web"].Config.Image = "nginx:1.27-alpine" })
	if res := same.drift(t); res.State != stackops.DriftNone {
		t.Errorf("a container made with the tag of the image the pin names: drift = %s %v, want none", res.State, driftNames(res))
	}

	// The image change comes with a field change: one row, with the image as one more setting.
	both := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) {
		h.Docs["web"].Config.Image = other
		h.Docs["web"].Config.Env = append(h.Docs["web"].Config.Env, "EXTRA=1")
	})
	res = both.drift(t)
	if !slices.Equal(driftNames(res), []string{"changed web"}) || res.Services[0].Diff.Settings != 2 || res.Count != 2 {
		t.Errorf("drift = %v settings %d count %d, want one row for web with the environment entry and the image counted", driftNames(res), res.Services[0].Diff.Settings, res.Count)
	}
	if cur := string(res.Services[0].Diff.Current); !strings.Contains(cur, "EXTRA") || !strings.Contains(cur, strings.Repeat("e", 64)) {
		t.Errorf("current = %s, want both the environment entry and the running image", cur)
	}
}

func TestStandaloneDriftScaleMissingExtra(t *testing.T) {
	t.Parallel()
	// Each kind is its own host, judged in turn in this one test: a failure names the kind and the others still report.
	judge := func(label string, edit func(*stackopstest.Hosted), check func(stackops.DriftResult) []string) {
		res := hostedDrift(t, driftHostedCompose, edit).drift(t)
		for _, p := range check(res) {
			t.Error(label + ": " + p)
		}
	}
	judge("a scale", func(h *stackopstest.Hosted) { secondContainer(h, "web") }, func(res stackops.DriftResult) []string {
		if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) || res.Count != 1 {
			return []string{fmt.Sprintf("drift = %s %v count %d, want drifted with web changed by its scale", res.State, driftNames(res), res.Count)}
		}
		if cur, next := string(res.Services[0].Diff.Current), string(res.Services[0].Diff.Next); !strings.Contains(cur, `"scale":2`) || strings.Contains(next, `"scale"`) {
			return []string{fmt.Sprintf("current = %s\nnext = %s, want two containers running against the one deployed", cur, next)}
		}
		return nil
	})
	judge("a service with no container", func(h *stackopstest.Hosted) { withoutService(h, "worker") }, func(res stackops.DriftResult) []string {
		if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"missing worker"}) || res.Count != 1 {
			return []string{fmt.Sprintf("drift = %s %v count %d, want drifted with worker missing, counted once", res.State, driftNames(res), res.Count)}
		}
		if len(res.Services[0].Diff.Next) == 0 || len(res.Services[0].Diff.Current) != 0 {
			return []string{"a missing service holds the spec the revision deployed and nothing running"}
		}
		return nil
	})
	judge("an owned container the revision does not declare", func(h *stackopstest.Hosted) {
		h.Containers = append(h.Containers, cloneContainer(h, "worker", "ghost", 1))
	}, func(res stackops.DriftResult) []string {
		if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"extra ghost"}) || res.Count != 1 {
			return []string{fmt.Sprintf("drift = %s %v count %d, want drifted with ghost extra, counted once", res.State, driftNames(res), res.Count)}
		}
		return nil
	})
	judge("a container of the project that another owner made", func(h *stackopstest.Hosted) {
		stranger := cloneContainer(h, "worker", "stranger", 1)
		stranger.Labels[stacks.LabelStackID] = "someone-else"
		stranger.Inspect.Document.Config.Labels[stacks.LabelStackID] = "someone-else"
		h.Containers = append(h.Containers, stranger)
	}, func(res stackops.DriftResult) []string {
		if res.State != stackops.DriftNone {
			return []string{fmt.Sprintf("drift = %s %v, want none: ownership decides what is the stack's", res.State, driftNames(res))}
		}
		return nil
	})
}

func TestStandaloneDriftIgnoresOwnershipLabels(t *testing.T) {
	t.Parallel()
	// A project somebody ran with the docker compose command and Bayplan adopted: its containers carry no label of
	// Bayplan's, so every config hash differs from the one the prepared revision has, and nothing the person wrote differs.
	h := stackopstest.Foreign(t, "shop", foreignAdoptable)
	rig := hostRig(t, h)
	st := stackops.Stack{
		ID: "stack-shop", Name: "shop", Target: stacks.TargetStandalone,
		Source: stacks.Source{Kind: stacks.SourceAdopted, Ref: "shop"}, Adoption: stacks.Adoption{Pending: true},
	}
	rev := stackops.Snapshot{
		ID: "rev-1", StackID: st.ID, Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(foreignAdoptable)}}},
	}
	in := stackops.DriftInput{EnvID: rig.envID, Stack: st, Deployed: &rev, Pins: maps.Clone(h.Pins)}

	res, err := rig.deps.Drift(testsupport.Context(t), in)

	if err != nil || res.State != stackops.DriftNone || len(res.Services) != 0 || res.Count != 0 {
		t.Fatalf("drift = %+v, %v, want none: Bayplan's labels say who owns a container, not how it is set up", res, err)
	}

	// A real change on the same host still shows.
	h2 := stackopstest.Foreign(t, "shop", foreignAdoptable)
	h2.Docs["web"].Config.Env = append(h2.Docs["web"].Config.Env, "EXTRA=1")
	rig = hostRig(t, h2)
	in.EnvID = rig.envID
	res, err = rig.deps.Drift(testsupport.Context(t), in)
	if err != nil || res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"changed web"}) {
		t.Fatalf("drift = %+v, %v, want web changed by its environment entry", res, err)
	}
}

func TestStandaloneDriftStoppedReadsOnly(t *testing.T) {
	t.Parallel()
	rig := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) {
		// A stopped stack has its containers exited, which is not drift.
		for i := range h.Containers {
			h.Containers[i].State = "exited"
		}
		withoutService(h, "worker")
	})
	before := len(rig.fake.Requests())

	in := rig.input()
	in.Stack.Stopped = true
	stopped, err := rig.deps.Drift(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}

	if stopped.State != stackops.DriftStopped || stopped.Revision != 1 || len(stopped.Services) != 0 {
		t.Errorf("a stopped stack = %+v, want stopped with nothing listed", stopped)
	}
	if n := len(rig.fake.Requests()); n != before {
		t.Errorf("the host was asked %d more times, want a stopped stack not to read it", n-before)
	}

	// A check of the same stack running reads the host and writes nothing to it, and the Compose SDK (a stand-in that
	// fails the test) is not asked for anything.
	res := rig.drift(t)
	if res.State != stackops.DriftDrifted || !slices.Equal(driftNames(res), []string{"missing worker"}) {
		t.Errorf("the same stack running = %s %v, want drifted: the missing container is only excused by a stop", res.State, driftNames(res))
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("a drift check wrote %v", w)
	}
}
