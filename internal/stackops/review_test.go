// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The review (plan 07-14, STK-06): what a deploy would do against what the fake swarm holds.

// review reviews a compose file with its extra files and variables as the stack name.
func (r *swarmRig) review(t *testing.T, name, compose string, files []stacks.File, vars ...stacks.Var) stackops.Review {
	t.Helper()
	rv, err := r.deps.Review(testsupport.Context(t), r.reviewInput(name, compose, files, vars...))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

func (r *swarmRig) reviewInput(name, compose string, files []stacks.File, vars ...stacks.Var) stackops.ReviewInput {
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	return stackops.ReviewInput{
		EnvID: r.envID, Stack: stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm},
		Copy: stacks.WorkingCopy{Files: all, Vars: vars}, DraftVersion: 3,
	}
}

func objectsOf(rv stackops.Review) []string {
	var out []string
	for _, o := range rv.Objects {
		out = append(out, o.Change+" "+o.Kind+" "+o.Name)
	}
	return out
}

func TestReviewOfAStackNeverDeployedAddsEverythingAndWritesNothing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rv := rig.review(t, "tracer", stacks.StarterCompose, nil)
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	if rv.Summary.Services != (stackops.Counts{Added: 1}) || rv.Summary.Networks != (stackops.Counts{Added: 1}) || rv.Summary.ImagesNew != 1 || rv.Summary.ImagesPinned != 0 {
		t.Errorf("summary = %+v, want one service and one network added and one new image", rv.Summary)
	}
	if rv.DraftVersion != 3 || len(rv.Services) != 1 || rv.Services[0].Change != "added" || rv.Services[0].Short != "web" {
		t.Errorf("review = %+v", rv)
	}
	want := "nginx:1.27-alpine@" + dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine")
	if rv.Pins["tracer_web"] != want {
		t.Errorf("pins = %v, want %s", rv.Pins, want)
	}
	if got := objectsOf(rv); !slices.Equal(got, []string{"added network tracer_default"}) {
		t.Errorf("objects = %v", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("a review wrote %v", w)
	}
}

func TestReviewRightAfterADeployFindsNothingToDo(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "web", poolsCompose, nil)
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	rv := rig.review(t, "web", poolsCompose, nil)
	if len(rv.Services) != 0 || len(rv.Objects) != 0 || len(rv.Problems) != 0 {
		t.Errorf("review after a deploy = services %+v objects %v problems %+v, want none", rv.Services, objectsOf(rv), rv.Problems)
	}
	if rv.Summary.ImagesPinned != 1 || rv.Summary.ImagesNew != 0 {
		t.Errorf("summary = %+v, want the one image pinned", rv.Summary)
	}
}

func TestReviewCountsAChangedSecretAsANewVersionAndARemovedOneAsRemoved(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, first := stackInput(t, "web", tokCompose, tokFile("one"))
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}

	rv := rig.review(t, "web", tokCompose, tokFile("one"))
	if len(rv.Objects) != 0 {
		t.Errorf("unchanged secret: objects = %v, want none", objectsOf(rv))
	}

	rv = rig.review(t, "web", tokCompose, tokFile("two"))
	if got := objectsOf(rv); !slices.Equal(got, []string{"changed secret tok"}) || rv.Objects[0].Detail != "New version. The old one is removed after the deploy." {
		t.Errorf("changed secret: objects = %+v", rv.Objects)
	}
	if rv.Summary.Secrets != (stackops.Counts{Changed: 1}) {
		t.Errorf("secrets = %+v, want one changed", rv.Summary.Secrets)
	}
	// The service moves to the new secret, so it is a change of the service as well.
	if len(rv.Services) != 1 || rv.Services[0].Change != "changed" {
		t.Errorf("services = %+v, want web changed (its secret reference)", rv.Services)
	}

	plain := "services:\n  web:\n    image: nginx:1.27-alpine\n"
	rv = rig.review(t, "web", plain, nil)
	if got := objectsOf(rv); !slices.Equal(got, []string{"removed secret tok"}) || rv.Objects[0].Detail != "Removed from the stack. Deleted with the deploy." {
		t.Errorf("removed secret: objects = %+v", rv.Objects)
	}
}

func TestReviewNamesAnExternalNetworkAndBlocksOnOneThatIsMissing(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rv := rig.review(t, "web", externalNetworkCompose, nil)
	if got := objectsOf(rv); !slices.Contains(got, "external network org_shared") {
		t.Errorf("objects = %v, want the external network named", got)
	}
	var blocked bool
	for _, p := range rv.Problems {
		if p.Severity == stacks.SeverityError && p.Code == "external_missing" && strings.Contains(p.Message, "org_shared") {
			blocked = true
		}
	}
	if !blocked {
		t.Errorf("problems = %+v, want a blocking external_missing", rv.Problems)
	}

	rig = newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "x1", Name: "org_shared", Driver: "overlay", Scope: "swarm"}}
	})
	rv = rig.review(t, "web", externalNetworkCompose, nil)
	for _, p := range rv.Problems {
		if p.Severity == stacks.SeverityError {
			t.Errorf("an external network that exists blocked the review: %+v", p)
		}
	}
	if rv.Pins["web_web"] == "" {
		t.Errorf("pins = %v", rv.Pins)
	}
}

func TestReviewReportsAStackNetworkThatDiffersFromTheFile(t *testing.T) {
	t.Parallel()
	owned := map[string]string{"com.docker.stack.namespace": "web", "bayplan.stack.id": "stack-web"}
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		c.Networks = []dockertest.FakeNetwork{{ID: "n1", Name: "web_backend", Driver: "overlay", Scope: "swarm", Labels: owned, Subnets: []string{"10.99.0.0/24"}}}
	})
	rv := rig.review(t, "web", poolsCompose, nil)
	if got := objectsOf(rv); !slices.Equal(got, []string{"changed network web_backend"}) {
		t.Fatalf("objects = %v, want the network changed: the file asks for pools and flags it does not have", got)
	}
	if rv.Objects[0].Detail == "" {
		t.Errorf("a changed network says nothing about what to do: %+v", rv.Objects[0])
	}
}

func TestReviewOfAFileThatDoesNotLoadAnswersTheProblemsOnly(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rv := rig.review(t, "bad", "services: [", nil)
	if rv.Problems.Errors() == 0 || len(rv.Services) != 0 || len(rv.Pins) != 0 || len(rv.Images) != 0 {
		t.Errorf("review = %+v, want only the check's errors", rv)
	}
}

func TestReviewBlocksOnAnImageItCannotLookUp(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionFailure("docker.io/library/nginx:1.27-alpine", 404, "manifest unknown")
	rv := rig.review(t, "tracer", stacks.StarterCompose, nil)
	var blocked bool
	for _, p := range rv.Problems {
		if p.Severity == stacks.SeverityError && p.Code == stackops.CodeImageUnresolved && p.Line > 0 {
			blocked = true
		}
	}
	if !blocked || len(rv.Images) != 1 || rv.Images[0].State != stackops.ImageUnresolved || rv.Images[0].Digest != "" {
		t.Errorf("review = problems %+v images %+v, want a blocking image_unresolved at a line", rv.Problems, rv.Images)
	}
	if len(rv.Pins) != 0 {
		t.Errorf("pins = %v, an image that cannot be pinned has no pin", rv.Pins)
	}
}

func TestReviewInUpdateModeShowsOnlyTheChosenServicesMoving(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("c"))
	_, in := stackInput(t, "site", twoServiceCompose, nil)
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("b"))
	rig.fake.SetDistributionDigest("docker.io/library/redis:7", digestOf("d"))

	ri := rig.reviewInput("site", twoServiceCompose, nil)
	ri.Mode, ri.Services = stackops.ReviewModeUpdate, []string{"web"}
	rv, err := rig.deps.Review(testsupport.Context(t), ri)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, r := range rv.Images {
		states[r.Service] = r.State
	}
	if states["web"] != stackops.ImageNew || states["cache"] != stackops.ImagePinned {
		t.Errorf("image states = %v, want web new and cache pinned", states)
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" {
		t.Errorf("services = %+v, want only web to change", rv.Services)
	}

	// Nothing moved for a tag that did not: "already up to date".
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	rv, err = rig.deps.Review(testsupport.Context(t), ri)
	if err != nil {
		t.Fatal(err)
	}
	if len(rv.Services) != 0 {
		t.Errorf("services = %+v, want no change", rv.Services)
	}
	for _, r := range rv.Images {
		if r.Service == "web" && (r.State != stackops.ImagePinned || r.Note != "Already up to date") {
			t.Errorf("web image = %+v, want pinned and already up to date", r)
		}
	}
}

func TestADeployWithReviewedPinsMakesNoRegistryLookupAndRefusesOnesItWouldNotChoose(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rv := rig.review(t, "tracer", stacks.StarterCompose, nil)
	lookups := len(rig.fake.DistributionRefs())

	_, in := starterPlan(t, "tracer")
	in.Reviewed = rv.Pins
	res, job := rig.deploy(t, in)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}
	if got := len(rig.fake.DistributionRefs()); got != lookups {
		t.Errorf("the deploy made %d registry lookups", got-lookups)
	}

	// A pin of another repository stops the deploy at the lookup step before anything is written.
	rig = newSwarmRig(t, nil)
	_, bad := starterPlan(t, "tracer")
	bad.Reviewed = map[string]string{"tracer_web": "busybox:1@" + digestOf("a")}
	res, job = rig.deploy(t, bad)
	if res.OK() || res.Code != stackops.CodePinsInvalid || res.FailedService != "tracer_web" {
		t.Fatalf("deploy = %+v, want pins_invalid for tracer_web", res)
	}
	if got := itemStates(job)["lookup"]; got != "failed:pins_invalid" {
		t.Errorf("lookup item = %q", got)
	}
	if w := rig.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, a refused pin writes nothing", w)
	}
}
