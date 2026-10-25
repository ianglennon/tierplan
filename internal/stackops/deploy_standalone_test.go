// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The console of a standalone deploy (plan 07-20 task 3, D-13): the SDK's events become rows, and decide
// nothing. The Compose SDK itself is replaced by a stand-in that plays a script of events.

// scriptedUp plays steps of events. After each step but the last it says the step is in the job, and waits to
// be told to go on, so a test reads the job between two steps without sleeping.
type scriptedUp struct {
	steps   [][]dockerx.ComposeEvent
	err     error
	reached chan int
	proceed chan struct{}
}

func newScriptedUp(err error, steps ...[]dockerx.ComposeEvent) *scriptedUp {
	return &scriptedUp{steps: steps, err: err, reached: make(chan int), proceed: make(chan struct{})}
}

func (u *scriptedUp) up(ctx context.Context, _ *dockerx.Client, _ *types.Project, _ dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
	for i, evs := range u.steps {
		for _, ev := range evs {
			on(ev)
		}
		if i == len(u.steps)-1 {
			break
		}
		select {
		case u.reached <- i:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-u.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return u.err
}

// startStandalone starts a standalone deploy as a job and returns it before it ends.
func (r *swarmRig) startStandalone(t *testing.T, in stackops.StandaloneDeployInput) (string, <-chan stackops.DeployResult) {
	t.Helper()
	in.EnvID, in.NodeID = r.envID, r.envID
	if in.Caller.Principal == nil {
		in.Caller = stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
	}
	done := make(chan stackops.DeployResult, 1)
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test", Items: stackops.StandaloneItemSpecs(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		done <- r.deps.DeployStandalone(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	return job.ID, done
}

func (r *swarmRig) snapshot(t *testing.T, id string) jobs.Job {
	t.Helper()
	j, ok := r.hub.Get(id)
	if !ok {
		t.Fatalf("no job %s", id)
	}
	return j
}

func container(name, service string, status string, done bool) dockerx.ComposeEvent {
	return dockerx.ComposeEvent{Kind: dockerx.ComposeKindContainer, Name: name, Container: name, Service: service, Status: status, Done: done}
}

const withNetworkAndVolume = "services:\n  web:\n    image: nginx:1.27-alpine\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n"

func TestStandaloneDeployPutsTheStepsAndTheRowsOfTheConsoleInTheJob(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	rig.deps.Up = newScriptedUp(nil,
		[]dockerx.ComposeEvent{
			{Kind: dockerx.ComposeKindNetwork, Name: "demo_default", Status: "Created", Done: true},
			{Kind: dockerx.ComposeKindVolume, Name: "demo_data", Status: "Created", Done: true},
			container("demo-web-1", "web", "Created", true),
			container("demo-web-1", "web", "Started", true),
		}).up

	res, job := rig.deployStandalone(t, standaloneInput("demo", withNetworkAndVolume))

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result %+v, job %s %v", res, job.State, itemStates(job))
	}
	for id, want := range map[string][2]string{
		"check":                {"Check the stack", ""},
		"lookup":               {"Look up images", ""},
		"network:demo_default": {"Create network demo_default", "networks"},
		"volume:demo_data":     {"Create volume demo_data", "volumes"},
		"services":             {"Deploy services", ""},
		"service:web":          {"web", "services"},
		"cleanup":              {"Remove containers no longer in the stack", ""},
	} {
		it := itemByID(t, job, id)
		if it.Label != want[0] || it.Parent != want[1] || it.State != jobs.ItemDone {
			t.Errorf("item %s = {label %q parent %q state %s}, want {%q %q done}", id, it.Label, it.Parent, it.State, want[0], want[1])
		}
	}
	if k := itemByID(t, job, "service:web").Kind; k != stackops.KindService {
		t.Errorf("service row kind = %q", k)
	}
	if k := itemByID(t, job, "container:demo-web-1"); k.Parent != "service:web" || k.Kind != stackops.KindContainer || k.Label != "demo-web-1" || k.State != jobs.ItemDone {
		t.Errorf("container row = %+v, want a done container row under service:web", k)
	}
}

func TestStandaloneDeployShowsEachContainerGoingThroughCreatingStartingRunningHealthyAndTheServiceSummarisesThem(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := newScriptedUp(nil,
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Creating", false)},
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Created", true), container("demo-web-1", "web", "Starting", false)},
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Started", true), container("demo-web-1", "web", "Running", true)},
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Healthy", true)},
	)
	rig.deps.Up = stand.up
	id, done := rig.startStandalone(t, standaloneInput("demo", webStandalone))

	read := func() (svc, ctr jobs.Item) {
		j := rig.snapshot(t, id)
		return itemByID(t, j, "service:web"), itemByID(t, j, "container:demo-web-1")
	}
	for step, want := range []struct {
		container string
		phase     string
		running   int64
	}{{"creating", "creating", 0}, {"starting", "starting", 0}, {"running", "running", 1}} {
		if got := testsupport.Await(t, "the script to reach a step", stand.reached); got != step {
			t.Fatalf("reached step %d, want %d", got, step)
		}
		svc, ctr := read()
		if ctr.Result["state"] != want.container || ctr.State != jobs.ItemRunning || ctr.Parent != "service:web" {
			t.Errorf("step %d: container = %+v, want state %q still working", step, ctr, want.container)
		}
		if svc.Result["phase"] != want.phase || svc.Unit != "containers" || svc.Total != 1 || svc.Current != want.running || svc.State != jobs.ItemRunning {
			t.Errorf("step %d: service = %+v, want phase %q with %d of 1 containers", step, svc, want.phase, want.running)
		}
		stand.proceed <- struct{}{}
	}
	res := testsupport.Await(t, "the deploy to end", done)
	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	svc, ctr := read()
	if svc.State != jobs.ItemDone || svc.Result["phase"] != "healthy" || svc.Current != 1 || ctr.State != jobs.ItemDone || ctr.Result["state"] != "healthy" {
		t.Errorf("after the deploy: service %+v container %+v, want both done and healthy", svc, ctr)
	}
}

func TestStandaloneDeployHidesASecretInTheWordsOfAFailedContainerAndCleansThem(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	failed := container("demo-web-1", "web", "Error", false)
	failed.Failed = true
	failed.Text = "failed to start with token s3cr3t-value-123\x1b[31m\nsecond line"
	rig.deps.Up = newScriptedUp(errString("Error response from daemon: start failed: s3cr3t-value-123"),
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Creating", false), failed}).up
	in := standaloneInput("demo", webStandalone)
	in.Copy.Vars = []stacks.Var{{Name: "API_TOKEN", Value: "s3cr3t-value-123", Secret: true}}
	in.Masker = stacks.NewMasker(in.Copy.Vars, in.Copy.Files)

	res, job := rig.deployStandalone(t, in)

	if res.OK() || res.FailedService != "web" || res.Code != stackops.CodeEngineError {
		t.Fatalf("result = %+v, want engine_error at web", res)
	}
	ctr := itemByID(t, job, "container:demo-web-1")
	if ctr.State != jobs.ItemFailed || ctr.Code != stackops.CodeEngineError {
		t.Errorf("container row = %+v, want failed with engine_error", ctr)
	}
	for _, text := range []string{ctr.Detail, itemByID(t, job, "service:web").Detail, itemByID(t, job, "service:web").Message} {
		if strings.Contains(text, "s3cr3t") || strings.ContainsAny(text, "\x1b\n") {
			t.Errorf("text %q holds the secret or a control character", text)
		}
	}
	if !strings.Contains(ctr.Detail, "<hidden>") || !strings.Contains(ctr.Detail, "failed to start with token") {
		t.Errorf("container detail = %q, want the SDK's words with the secret hidden", ctr.Detail)
	}
	out, _ := rig.hub.Output(job.ID)
	for _, line := range out {
		if strings.Contains(line.Text, "s3cr3t") {
			t.Errorf("output line %q holds the secret", line.Text)
		}
	}
}

func TestStandaloneDeployShowsTheRemovalOfAContainerTheFileNoLongerDeclares(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t,
		deployedContainer("demo", "web", hash, "nginx:1.27-alpine", pin),
		dockertest.FakeContainer{
			ID: "c-old", Names: []string{"/demo-old-1"}, Image: "busybox:1", State: "running",
			Labels: map[string]string{
				stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "old", stacks.LabelStackID: "stack-demo", stacks.LabelComposeConfigHash: "0123",
			},
		})
	stand := newScriptedUp(nil,
		[]dockerx.ComposeEvent{container("demo-old-1", "old", "Removing", false)},
		[]dockerx.ComposeEvent{container("demo-old-1", "old", "Removed", true)},
	)
	rig.deps.Up = stand.up
	id, done := rig.startStandalone(t, standaloneInput("demo", webStandalone))

	// Before the SDK runs, the container is a step that waits under the cleanup step.
	testsupport.Await(t, "the script to reach the first step", stand.reached)
	step := itemByID(t, rig.snapshot(t, id), "remove-container:demo-old-1")
	if step.Parent != "cleanup" || step.Label != "Remove container demo-old-1" || step.State != jobs.ItemRunning {
		t.Errorf("while it is removed: %+v, want a running step under cleanup", step)
	}
	stand.proceed <- struct{}{}
	res := testsupport.Await(t, "the deploy to end", done)
	if !res.OK() || len(res.RemovedServices) != 1 || res.RemovedServices[0] != "old" {
		t.Fatalf("result = %+v, want old removed", res)
	}
	j := rig.snapshot(t, id)
	if got := itemByID(t, j, "remove-container:demo-old-1").State; got != jobs.ItemDone {
		t.Errorf("removal step = %s, want done", got)
	}
	if it := itemByID(t, j, "cleanup"); it.State != jobs.ItemDone || it.Result["removedServices"] != "1" {
		t.Errorf("cleanup = %+v, want done with one service removed", it)
	}
}

func TestStandaloneDeployDecidesFromTheSDKsAnswerAndNotFromItsWords(t *testing.T) {
	t.Parallel()
	t.Run("an error event with a clean return is a deploy that worked", func(t *testing.T) {
		t.Parallel()
		rig := newStandaloneRig(t)
		warn := container("demo-web-1", "web", "Error", false)
		warn.Failed, warn.Text = true, "a warning the SDK worded as an error"
		rig.deps.Up = newScriptedUp(nil, []dockerx.ComposeEvent{warn}).up
		res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))
		if !res.OK() || job.State != jobs.JobDone {
			t.Errorf("result %+v job %s %v, want the deploy to have worked: Up returned nil", res, job.State, itemStates(job))
		}
	})
	t.Run("an error return with only good words is a deploy that failed", func(t *testing.T) {
		t.Parallel()
		rig := newStandaloneRig(t)
		rig.deps.Up = newScriptedUp(errString("boom"), []dockerx.ComposeEvent{container("demo-web-1", "web", "Running", true)}).up
		res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))
		if res.OK() || res.Code != stackops.CodeEngineError || itemStates(job)["services"] != "failed:engine_error" {
			t.Errorf("result %+v items %v, want engine_error: Up returned an error", res, itemStates(job))
		}
	})
}

func TestStandaloneDeployAddsRowsForContainersTheSDKLeftAloneFromWhatTheHostHoldsAfterwards(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", pin))
	rig.deps.Up = newScriptedUp(nil).up

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	ctr := itemByID(t, job, "container:demo-web-1")
	if ctr.Parent != "service:web" || ctr.State != jobs.ItemDone || ctr.Result["state"] != "running" {
		t.Errorf("container row = %+v, want a done row that reads running", ctr)
	}
	svc := itemByID(t, job, "service:web")
	if svc.Result["action"] != "unchanged" || svc.Current != 1 || svc.Total != 1 {
		t.Errorf("service row = %+v, want unchanged with 1 of 1 containers", svc)
	}
}

// ---- an agent that refuses, a host that is read-only (plan 07-21, D-27)

func TestStandaloneDeployNamesTheServiceAnAgentRefusedAndKeepsTheRefusalAsItsCode(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	refused := container("demo-web-1", "web", "Error", false)
	refused.Failed = true
	refused.Text = "Error response from daemon: agent_policy_denied: POST /v1.52/containers/create"
	rig.deps.Up = newScriptedUp(
		errString("Error response from daemon: agent_policy_denied: POST /v1.52/containers/create"),
		[]dockerx.ComposeEvent{container("demo-web-1", "web", "Creating", false), refused}).up

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if res.OK() || res.Code != stackops.CodeAgentPolicyDenied || res.FailedService != "web" {
		t.Fatalf("result = %+v, want agent_policy_denied at web", res)
	}
	svc := itemByID(t, job, "service:web")
	if svc.State != jobs.ItemFailed || svc.Code != stackops.CodeAgentPolicyDenied {
		t.Errorf("service row = %s %s, want failed with agent_policy_denied", svc.State, svc.Code)
	}
	// The agent's own words, the request it refused, are on the row for the console to show.
	if !strings.Contains(svc.Detail, "agent_policy_denied") || !strings.Contains(svc.Detail, "POST /v1.52/containers/create") {
		t.Errorf("service detail = %q, want the refused request", svc.Detail)
	}
}

func TestStandaloneDeployOnAReadOnlyHostFailsTheCheckStepAndRunsNothing(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	rig.deps.Writable = func(context.Context, string, string) error {
		return fmt.Errorf("environment e1: %w", envs.ErrHostReadOnly)
	}

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if res.OK() || res.Code != stackops.CodeHostReadOnly {
		t.Fatalf("result = %+v, want host_read_only", res)
	}
	if got := itemStates(job); got["check"] != "failed:host_read_only" || got["lookup"] != "skipped:not_run" || got["services"] != "skipped:not_run" {
		t.Errorf("items = %v, want the check step failed with host_read_only and the rest not run", got)
	}
	if stand.calls != 0 || len(rig.fake.Writes()) != 0 || len(rig.fake.DistributionRefs()) != 0 {
		t.Errorf("SDK calls %d, writes %v, lookups %v, want nothing asked of the host", stand.calls, rig.fake.Writes(), rig.fake.DistributionRefs())
	}
}

// A standalone roll back sends the images the revision stored (plan 08-05, STK-08, D-06, T-08-18).
func TestStandaloneRollbackSendsTheStoredDigest(t *testing.T) {
	t.Parallel()
	older := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	newer := "nginx:1.27-alpine@sha256:" + strings.Repeat("b", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": newer})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", newer))
	// The tag has moved again on the registry: a roll back must not follow it.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", "sha256:"+strings.Repeat("c", 64))
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	pins := &memPins{}
	in := standaloneInput("demo", webStandalone)
	in.Pins, in.RevisionID, in.Reviewed, in.Rollback = pins, "rev-3", map[string]string{"web": older}, true

	res, job := rig.deployStandalone(t, in)

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("result %+v, job %s %v", res, job.State, itemStates(job))
	}
	if stand.project == nil || stand.project.Services["web"].Image != older {
		t.Fatalf("the SDK was given %+v, want web at exactly the stored reference %q", stand.project, older)
	}
	if got := pins.pinsOf("rev-3"); got["web"] != older {
		t.Errorf("stored pins = %v, want the roll back's revision to hold the stored reference", got)
	}
	if refs := rig.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("distribution checks = %v, a roll back asks no registry", refs)
	}
	if res.Updated != 1 {
		t.Errorf("result = %+v, want the web container recreated from the older image", res)
	}
}

func TestAReviewedStandaloneDeployRefusesAnOlderDigestButARollbackSendsIt(t *testing.T) {
	t.Parallel()
	older := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	newer := "nginx:1.27-alpine@sha256:" + strings.Repeat("b", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": newer})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", newer))
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	in := standaloneInput("demo", webStandalone)
	in.Reviewed = map[string]string{"web": older}

	res, _ := rig.deployStandalone(t, in)

	if res.OK() || res.Code != stackops.CodePinsInvalid || stand.calls != 0 {
		t.Fatalf("result %+v after %d runs, want a reviewed deploy of an older digest refused (pins_invalid) before anything runs", res, stand.calls)
	}
	in.Rollback = true
	if res, _ = rig.deployStandalone(t, in); !res.OK() || stand.calls != 1 {
		t.Errorf("result %+v after %d runs, want the same pins accepted as a roll back", res, stand.calls)
	}
}

func TestAStandaloneRollbackWithoutAStoredImageFailsTheLookupStepAndRunsNothing(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	in := standaloneInput("demo", webStandalone)
	in.Reviewed, in.Rollback = map[string]string{}, true

	res, job := rig.deployStandalone(t, in)

	if res.OK() || res.Code != stackops.CodePinsInvalid || res.FailedService != "web" || stand.calls != 0 {
		t.Fatalf("result %+v after %d runs, want pins_invalid at web and nothing run", res, stand.calls)
	}
	if states := itemStates(job); states["lookup"] != "failed:pins_invalid" {
		t.Errorf("items = %v, want the lookup step failed", states)
	}
}

// standaloneDeployInput is the input of the adopted project's first deploy, with the images its review chose.
func (a *adopted) standaloneDeployInput(t *testing.T, rv stackops.Review) stackops.StandaloneDeployInput {
	t.Helper()
	st, err := a.repo.Stack(testsupport.Context(t), a.rig.envID, a.stack.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stackops.StandaloneDeployInput{Stack: st, Copy: a.draft.Copy, Reviewed: rv.Pins, Pins: a.repo}
}

// TestFirstDeployOfAnAdoptedStandaloneStack: the first deploy of an adopted docker compose project is not refused as
// a foreign project, and hands the SDK the project's network and volume under the names recorded at adoption with the
// config hashes the host's labels hold, so the SDK recreates the containers for their labels and keeps the network
// and the volume. Bayplan itself creates, removes and writes nothing but what the SDK does (D-16, D-18, T-08-65).
func TestFirstDeployOfAnAdoptedStandaloneStack(t *testing.T) {
	t.Parallel()
	h := stackopstest.Foreign(t, "shop", foreignAdoptable)
	a := adoptRigOn(t, hostRig(t, h), stacks.TargetStandalone)
	rv := a.review(t)
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v, want the first review of an adopted project to refuse nothing", rv.Problems)
	}
	stand := &upStandIn{}
	a.rig.deps.Up = stand.up
	before := len(a.rig.fake.Writes())

	res, job := a.rig.deployStandalone(t, a.standaloneDeployInput(t, rv))

	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}
	if res.Networks != 0 || len(res.CreatedNetworks) != 0 || res.Services != 0 || len(res.RemovedServices) != 0 {
		t.Errorf("deploy created networks %v and services %d and removed %v, want only the recreate for the labels", res.CreatedNetworks, res.Services, res.RemovedServices)
	}
	if res.Updated != 2 {
		t.Errorf("updated = %d, want both services recreated for the ownership labels", res.Updated)
	}
	if got := a.rig.fake.Writes()[before:]; len(got) != 0 {
		t.Errorf("Bayplan wrote %v, want everything left to the SDK", got)
	}
	p := stand.project
	if p == nil || stand.calls != 1 {
		t.Fatalf("the SDK stand-in was called %d times", stand.calls)
	}
	if len(stand.opts.ForceRecreate) != 0 {
		t.Errorf("force recreate = %v, want none: the hashes already differ", stand.opts.ForceRecreate)
	}
	net, vol := p.Networks["back"], p.Volumes["data"]
	if net.Name != "shop_back" || vol.Name != "shop_data" || bool(net.External) || bool(vol.External) {
		t.Fatalf("network %+v volume %+v, want shop_back and shop_data declared as the project's own", net, vol)
	}
	netHash, err := dockerx.ComposeNetworkHash(&net)
	if err != nil {
		t.Fatal(err)
	}
	volHash, err := dockerx.ComposeVolumeHash(vol)
	if err != nil {
		t.Fatal(err)
	}
	if want := h.Networks[0].Labels[stacks.LabelComposeConfigHash]; netHash != want {
		t.Errorf("network hash = %s, want the host's %s: the SDK would recreate the network", netHash, want)
	}
	if want := h.Volumes[0].Labels[stacks.LabelComposeConfigHash]; volHash != want {
		t.Errorf("volume hash = %s, want the host's %s: the SDK would ask to recreate the volume", volHash, want)
	}
	for name, svc := range p.Services {
		if svc.CustomLabels[stacks.LabelStackID] != a.stack.ID {
			t.Errorf("service %s is sent without Bayplan's ID: %v", name, svc.CustomLabels)
		}
	}
}
