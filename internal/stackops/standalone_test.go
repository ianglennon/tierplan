// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	mobycontainer "github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Standalone stacks on the server's own host (plan 07-20, STK-02, STK-06, STK-09): the review compares the
// config hash of each service with the label its containers carry, and the deploy runs the Compose SDK on a
// project prepared for it. The SDK itself needs a real engine and is proved against one in
// standalone_integration_test.go; here it is replaced by a stand-in that sees what it is given.

const webStandalone = "services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"8080:80\"\n"

func standaloneStackOf(name string) stackops.Stack {
	return stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetStandalone}
}

func newStandaloneRig(t *testing.T, containers ...dockertest.FakeContainer) *swarmRig {
	t.Helper()
	return newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.Containers = containers
	})
}

// preparedHash is the config hash of each service of a compose file, prepared the way a deploy prepares it
// with the given pins: what the SDK stamps on the containers it creates.
func preparedHash(t *testing.T, name, compose string, pins map[string]string) map[string]string {
	t.Helper()
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}}
	checked := stacks.Check(context.Background(), stacks.CheckInput{Name: name, Target: stacks.TargetStandalone, Copy: copy})
	if checked.Loaded == nil {
		t.Fatalf("check: %v", checked.Problems)
	}
	objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, checked.Loaded.Project, stacks.NewFileSet(copy.Files), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stacks.PrepareStandalone(checked.Loaded.Project, "stack-"+name, pins, objs); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for svc, cfg := range checked.Loaded.Project.Services {
		h, err := dockerx.ServiceHash(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out[svc] = h
	}
	return out
}

// deployedContainer is the container a deploy of a service leaves on the host.
func deployedContainer(project, service, hash, asWritten, pin string) dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: "c-" + service, Names: []string{"/" + project + "-" + service + "-1"}, Image: pin, State: "running",
		Labels: map[string]string{
			stacks.LabelComposeProject: project, stacks.LabelComposeService: service, stacks.LabelComposeOneoff: "False",
			stacks.LabelComposeConfigHash: hash, stacks.LabelStackID: "stack-" + project, stacks.LabelStackImage: asWritten,
		},
	}
}

func (r *swarmRig) standaloneReview(t *testing.T, name, compose string, containers ...dockertest.FakeContainer) stackops.Review {
	t.Helper()
	rv, err := r.deps.Review(testsupport.Context(t), r.standaloneReviewInput(t, name, compose, nil))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

func (r *swarmRig) standaloneReviewInput(t *testing.T, name, compose string, pins stackops.PinStore) stackops.ReviewInput {
	t.Helper()
	return stackops.ReviewInput{
		EnvID: r.envID, Stack: standaloneStackOf(name),
		Copy:         stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
		DraftVersion: 3, Pins: pins,
		Caller: stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"},
	}
}

func changesOf(rv stackops.Review) []string {
	var out []string
	for _, c := range rv.Services {
		out = append(out, c.Change+" "+c.Short)
	}
	return out
}

func TestReviewOfAStandaloneStackOnAnEmptyHostAddsTheServiceAndItsNetwork(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	rv := rig.standaloneReview(t, "demo", webStandalone)

	if got := changesOf(rv); !slices.Equal(got, []string{"added web"}) {
		t.Errorf("services = %v, want web added", got)
	}
	if rv.Problems.Errors() != 0 {
		t.Errorf("problems = %v, want none that block", rv.Problems)
	}
	if len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageNew || !strings.Contains(rv.Pins["web"], "nginx:1.27-alpine@sha256:") {
		t.Errorf("images = %+v pins = %v, want a new digest for web", rv.Images, rv.Pins)
	}
	if got := objectsOf(rv); !slices.Equal(got, []string{"added network demo_default"}) {
		t.Errorf("objects = %v, want the default network added", got)
	}
	if rv.Summary.Services.Added != 1 || rv.Summary.Networks.Added != 1 {
		t.Errorf("summary = %+v", rv.Summary)
	}
}

func TestReviewRightAfterADeployShowsNothingToDoAndKeepsTheRunningDigest(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", pin))
	// The tag has moved since: the running digest must stay.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", digestOf("b"))

	rv := rig.standaloneReview(t, "demo", webStandalone)

	if len(rv.Services) != 0 {
		t.Errorf("services = %v, want none changed", changesOf(rv))
	}
	if len(rv.Images) != 1 || rv.Images[0].State != stackops.ImagePinned || rv.Pins["web"] != pin {
		t.Errorf("images = %+v pins = %v, want web to keep %s", rv.Images, rv.Pins, pin)
	}
	if refs := rig.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("the registry was asked about %v, want no lookup for a service that keeps its digest", refs)
	}
}

func TestReviewListsAServiceWhoseConfigurationChangedAndOneTheFileNoLongerDeclares(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	oldHash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t,
		deployedContainer("demo", "web", oldHash, "nginx:1.27-alpine", pin),
		deployedContainer("demo", "old", "0123", "busybox:1", "busybox:1@sha256:"+strings.Repeat("c", 64)),
	)
	edited := strings.Replace(webStandalone, "8080:80", "9090:80", 1)

	rv := rig.standaloneReview(t, "demo", edited)

	if got := changesOf(rv); !slices.Equal(got, []string{"changed web", "removed old"}) {
		t.Errorf("services = %v, want web changed and old removed", got)
	}
	if rv.Summary.Services.Changed != 1 || rv.Summary.Services.Removed != 1 {
		t.Errorf("summary = %+v", rv.Summary)
	}
}

func TestReviewTakesAChangedSecretFileForAChangedService(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets: [tok]\nsecrets:\n  tok:\n    file: tok.txt\n"
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	hashOf := func(content string) string {
		copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}, {Path: "tok.txt", Content: []byte(content), Secret: true}}}
		checked := stacks.Check(context.Background(), stacks.CheckInput{Name: "demo", Target: stacks.TargetStandalone, Copy: copy})
		objs, err := stacks.ResolveObjectsFor(stacks.TargetStandalone, checked.Loaded.Project, stacks.NewFileSet(copy.Files), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := stacks.PrepareStandalone(checked.Loaded.Project, "stack-demo", map[string]string{"web": pin}, objs); err != nil {
			t.Fatal(err)
		}
		h, _ := dockerx.ServiceHash(checked.Loaded.Project.Services["web"])
		return h
	}
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hashOf("first"), "nginx:1.27-alpine", pin))

	review := func(content string) stackops.Review {
		in := rig.standaloneReviewInput(t, "demo", compose, nil)
		in.Copy.Files = append(in.Copy.Files, stacks.File{Path: "tok.txt", Content: []byte(content), Secret: true})
		rv, err := rig.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatal(err)
		}
		return rv
	}
	if got := changesOf(review("first")); len(got) != 0 {
		t.Errorf("the same secret content: %v, want nothing to do", got)
	}
	if got := changesOf(review("second")); !slices.Equal(got, []string{"changed web"}) {
		t.Errorf("new secret content: %v, want web changed", got)
	}
}

func TestReviewBlocksAComposeProjectOfTheSameNameThatTierPlanDoesNotManage(t *testing.T) {
	t.Parallel()
	foreign := dockertest.FakeContainer{
		ID: "c1", Names: []string{"/demo-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "web"},
	}
	rig := newStandaloneRig(t, foreign)
	rv := rig.standaloneReview(t, "demo", webStandalone)
	if !slices.ContainsFunc(rv.Problems, func(p stacks.Problem) bool {
		return p.Code == stackops.CodeNameClash && p.Severity == stacks.SeverityError
	}) {
		t.Errorf("problems = %v, want a blocking name clash", rv.Problems)
	}
}

func TestReviewRefusesAnExternalSecretOnAStandaloneHost(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    secrets: [tok]\nsecrets:\n  tok:\n    external: true\n"
	rig := newStandaloneRig(t)
	rv := rig.standaloneReview(t, "demo", compose)
	if !slices.ContainsFunc(rv.Problems, func(p stacks.Problem) bool { return p.Code == stackops.CodeExternalFileObject }) {
		t.Errorf("problems = %v, want the external secret refused", rv.Problems)
	}
}

// ---- the deploy

// upStandIn replaces the Compose SDK: it records the project it was handed and plays events and an outcome.
type upStandIn struct {
	mu      sync.Mutex
	project *types.Project
	opts    dockerx.ComposeUpOptions
	events  []dockerx.ComposeEvent
	err     error
	calls   int
}

func (u *upStandIn) up(_ context.Context, _ *dockerx.Client, p *types.Project, o dockerx.ComposeUpOptions, on func(dockerx.ComposeEvent)) error {
	u.mu.Lock()
	u.project, u.opts, u.calls = p, o, u.calls+1
	u.mu.Unlock()
	for _, ev := range u.events {
		on(ev)
	}
	return u.err
}

func (r *swarmRig) deployStandalone(t *testing.T, in stackops.StandaloneDeployInput) (stackops.DeployResult, jobs.Job) {
	t.Helper()
	in.EnvID = r.envID
	if in.NodeID == "" {
		in.NodeID = r.envID // the server's local socket is one node, and its ID is the environment's
	}
	if in.Caller.Principal == nil {
		in.Caller = stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"}
	}
	var res stackops.DeployResult
	done := make(chan struct{})
	job, err := r.hub.Start(jobs.Spec{
		Kind: "stack_deploy", EnvID: r.envID, Owner: "user:test", Items: stackops.StandaloneItemSpecs(),
	}, func(ctx context.Context, rep *jobs.Reporter) {
		defer close(done)
		res = r.deps.DeployStandalone(ctx, in, rep)
	})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := r.hub.Wait(testsupport.Context(t), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Await(t, "the deploy function to return", done)
	return res, ended
}

func standaloneInput(name, compose string) stackops.StandaloneDeployInput {
	return stackops.StandaloneDeployInput{
		Stack: standaloneStackOf(name),
		Copy:  stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}},
	}
}

func TestDeployStandaloneHandsTheSDKAPreparedProjectWithThePinnedImage(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	pins := &memPins{}
	in := standaloneInput("demo", webStandalone)
	in.Pins, in.RevisionID = pins, "rev-1"

	res, job := rig.deployStandalone(t, in)

	if !res.OK() || res.Services != 1 || res.Updated != 0 || res.Networks != 1 || res.Total != 1 {
		t.Fatalf("result = %+v, want one service and one network created", res)
	}
	if job.State != jobs.JobDone {
		t.Errorf("job = %s %v, want done", job.State, itemStates(job))
	}
	p := stand.project
	if p == nil || stand.calls != 1 {
		t.Fatalf("the SDK stand-in was called %d times", stand.calls)
	}
	web := p.Services["web"]
	if !strings.HasPrefix(web.Image, "nginx:1.27-alpine@sha256:") {
		t.Errorf("image = %q, want the pinned reference", web.Image)
	}
	if web.CustomLabels[stacks.LabelStackImage] != "nginx:1.27-alpine" || web.CustomLabels[stacks.LabelStackID] != "stack-demo" ||
		web.CustomLabels[stacks.LabelComposeProject] != "demo" {
		t.Errorf("custom labels = %v", web.CustomLabels)
	}
	if got := pins.pinsOf("rev-1"); got["web"] != web.Image {
		t.Errorf("stored pins = %v, want web = %s", got, web.Image)
	}
	if len(rig.fake.ServiceCreates()) != 0 || len(rig.fake.NetworkCreates()) != 0 {
		t.Error("a standalone deploy wrote swarm objects")
	}
}

func TestDeployStandaloneCountsWhatItCreatedUpdatedAndRemoved(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	oldHash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    ports:\n      - \"9090:80\"\n  cache:\n    image: redis:7\n"
	rig := newStandaloneRig(t,
		deployedContainer("demo", "web", oldHash, "nginx:1.27-alpine", pin),
		deployedContainer("demo", "gone", "0123", "busybox:1", "busybox:1@sha256:"+strings.Repeat("c", 64)),
	)
	rig.deps.Up = (&upStandIn{}).up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", compose))

	if !res.OK() || res.Services != 1 || res.Updated != 1 || !slices.Equal(res.RemovedServices, []string{"gone"}) {
		t.Errorf("result = %+v, want cache created, web updated and gone removed", res)
	}
}

func TestDeployStandaloneStopsAtTheServicesWhenTheSDKFailsAndSkipsWhatFollows(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	stand := &upStandIn{
		events: []dockerx.ComposeEvent{{Kind: dockerx.ComposeKindContainer, Name: "demo-web-1", Container: "demo-web-1", Service: "web", Status: "Error", Text: "port is already allocated", Failed: true}},
		err:    errString("Error response from daemon: driver failed programming external connectivity: port is already allocated"),
	}
	rig.deps.Up = stand.up

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if res.OK() || res.Code != stackops.CodeEngineError || res.FailedService != "web" {
		t.Fatalf("result = %+v, want engine_error at web", res)
	}
	states := itemStates(job)
	if states["services"] != "failed:engine_error" || states["service:web"] != "failed:engine_error" || states["cleanup"] != "skipped:not_run" {
		t.Errorf("items = %v", states)
	}
	if it := itemByID(t, job, "service:web"); !strings.Contains(it.Detail, "port is already allocated") || strings.Contains(it.Detail, "Error response from daemon") {
		t.Errorf("service detail = %q, want the engine's words without the client's prefix", it.Detail)
	}
}

func TestDeployStandaloneRefusesAProjectOfTheSameNameTierPlanDoesNotManage(t *testing.T) {
	t.Parallel()
	foreign := dockertest.FakeContainer{
		ID: "c1", Names: []string{"/demo-web-1"}, Image: "nginx", State: "running",
		Labels: map[string]string{stacks.LabelComposeProject: "demo", stacks.LabelComposeService: "web"},
	}
	rig := newStandaloneRig(t, foreign)
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if res.OK() || res.Code != stackops.CodeNameClash {
		t.Fatalf("result = %+v, want stack_name_clash", res)
	}
	if stand.calls != 0 {
		t.Error("the SDK ran for a project TierPlan does not manage")
	}
	if got := itemStates(job)["check"]; got != "failed:stack_name_clash" {
		t.Errorf("check = %s", got)
	}
}

func TestDeployStandaloneKeepsTheRunningDigestWhenTheTagMoved(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", pin))
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", digestOf("b"))
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if !res.OK() || res.Services != 0 || res.Updated != 0 {
		t.Fatalf("result = %+v, want nothing to create or update", res)
	}
	if got := stand.project.Services["web"].Image; got != pin {
		t.Errorf("image = %q, want the running pin %q", got, pin)
	}
	if refs := rig.fake.DistributionRefs(); len(refs) != 0 {
		t.Errorf("the registry was asked about %v, want no lookup", refs)
	}
}

func TestDeployStandaloneJudgesTheHostAccessPolicyAgainForTheCaller(t *testing.T) {
	t.Parallel()
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    privileged: true\n"
	rig := newStandaloneRig(t)
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	in := standaloneInput("demo", compose)
	in.Caller = stackops.Caller{Principal: rig.principal(t, "op", "operator"), Actor: "op"}

	res, job := rig.deployStandalone(t, in)

	if res.OK() || res.Code != stackops.CodeNeedsAdmin || stand.calls != 0 {
		t.Fatalf("result = %+v (SDK calls %d), want needs_admin before anything runs", res, stand.calls)
	}
	if got := itemStates(job)["check"]; got != "failed:needs_admin" {
		t.Errorf("check = %s", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// The deploy applies what the review shows (plan 07-22): a container changed by hand keeps its compose
// labels and its hash, which the SDK compares and nothing else, so the deploy has it recreated.

func TestDeployStandaloneForcesTheRecreateOfAContainerChangedByHand(t *testing.T) {
	t.Parallel()
	h := hostStack(t, "demo", hostedCompose, nil)
	h.Docs["web"].HostConfig.RestartPolicy = mobycontainer.RestartPolicy{Name: "always"}
	rig := hostRig(t, h)
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", hostedCompose))

	if !res.OK() || stand.calls != 1 {
		t.Fatalf("result = %+v, the SDK stand-in was called %d times", res, stand.calls)
	}
	if !slices.Equal(stand.opts.ForceRecreate, []string{"web"}) {
		t.Errorf("force recreate = %v, want web: its running configuration differs from the file and its hash matches", stand.opts.ForceRecreate)
	}
	if res.Updated != 1 || res.Services != 0 {
		t.Errorf("result = %+v, want web counted as updated", res)
	}
}

func TestDeployStandaloneForcesNothingWhenNothingDiffers(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", hostedCompose))

	if !res.OK() || len(stand.opts.ForceRecreate) != 0 || res.Updated != 0 {
		t.Errorf("result = %+v force recreate = %v, want a deploy that changes nothing", res, stand.opts.ForceRecreate)
	}
}

func TestDeployStandaloneLeavesAServiceWhoseHashDiffersToTheSDK(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", strings.Replace(hostedCompose, "8080:80", "9090:80", 1)))

	if !res.OK() || len(stand.opts.ForceRecreate) != 0 || res.Updated != 1 {
		t.Errorf("result = %+v force recreate = %v, want web recreated by its hash and not forced", res, stand.opts.ForceRecreate)
	}
}
