// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/stacks/convert"
	"github.com/ianglennon/coaming/internal/stacks/stackstest"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The review (plan 07-14, STK-06): what a deploy would do against what the fake swarm holds.

// review reviews a compose file with its extra files and variables as the stack name.
func (r *swarmRig) review(t *testing.T, name, compose string, files []stacks.File, vars ...stacks.Var) stackops.Review {
	t.Helper()
	rv, err := r.deps.Review(testsupport.Context(t), r.reviewInput(t, name, compose, files, vars...))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

// reviewInput is a review of a compose file as an instance admin, who may deploy anything.
func (r *swarmRig) reviewInput(t *testing.T, name, compose string, files []stacks.File, vars ...stacks.Var) stackops.ReviewInput {
	t.Helper()
	all := append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...)
	return stackops.ReviewInput{
		EnvID: r.envID, Stack: stackops.Stack{ID: "stack-" + name, Name: name, Target: stacks.TargetSwarm},
		Copy: stacks.WorkingCopy{Files: all, Vars: vars}, DraftVersion: 3,
		Caller: stackops.Caller{Principal: r.principal(t, "root", ""), Actor: "root"},
	}
}

// principal adds a user holding role on the rig's environment, or an instance admin when role is
// empty, and returns it. Asking again for an ID returns the user it made.
func (r *swarmRig) principal(t *testing.T, id, role string) *rbac.Principal {
	t.Helper()
	ctx := testsupport.Context(t)
	p := &rbac.Principal{Type: rbac.PrincipalUser, ID: id, Name: id}
	if _, err := r.store.Write.ExecContext(ctx, `INSERT OR IGNORE INTO users (id, username, password_hash, created_at, updated_at, password_changed_at) VALUES (?, ?, 'x', 1, 1, 1)`, id, id); err != nil {
		t.Fatal(err)
	}
	if role == "" {
		tx, err := r.store.Write.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rbac.GrantInstanceAdminTx(ctx, tx, r.store.Q, id); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := r.store.Write.ExecContext(ctx, `INSERT OR IGNORE INTO role_assignments (id, principal_type, principal_id, role_id, env_id, created_at) VALUES (?, 'user', ?, ?, ?, 1)`,
		"ra-"+id, id, role, r.envID); err != nil {
		t.Fatal(err)
	}
	return p
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
	owned := map[string]string{"com.docker.stack.namespace": "web", "coaming.stack.id": "stack-web"}
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

	ri := rig.reviewInput(t, "site", twoServiceCompose, nil)
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
	// The server shares one memo of the review's answers with the deploy, so the deploy needs no lookup for the platforms
	// of the digests it was given (G-08-4; without a memo it asks once for each, see TestAReviewedDeploySendsThePlatformsItsReviewFound).
	rig.deps.Platforms = stackops.NewPlatformMemo()
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

// The host-escalation policy, judged on the values a deploy would use (plan 07-14, AUTH-10).

const privilegedCompose = `services:
  web:
    image: nginx:1.27-alpine
    privileged: true
`

const dataBindCompose = `services:
  web:
    image: nginx:1.27-alpine
    volumes:
      - "${DATA}:/data"
`

// hostNetworkCompose attaches the swarm's host network by naming it: the compose rules read no
// host access in it, the converted spec does.
const hostNetworkCompose = `services:
  web:
    image: nginx:1.27-alpine
    networks: [outer]
networks:
  outer:
    external: true
    name: host
`

func (r *swarmRig) reviewAs(t *testing.T, p *rbac.Principal, name, compose string, vars ...stacks.Var) stackops.Review {
	t.Helper()
	ri := r.reviewInput(t, name, compose, nil, vars...)
	ri.Caller = stackops.Caller{Principal: p, Actor: p.Name}
	rv, err := r.deps.Review(testsupport.Context(t), ri)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	return rv
}

func needsAdmin(rv stackops.Review) []stacks.Problem {
	var out []stacks.Problem
	for _, p := range rv.Problems {
		if p.Code == stackops.CodeNeedsAdmin {
			out = append(out, p)
		}
	}
	return out
}

func TestReviewRefusesHostEscalationBelowEnvAdminAtItsLineAndNamesTheKinds(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	operator, admin := rig.principal(t, "op", "operator"), rig.principal(t, "adm", "admin")

	rv := rig.reviewAs(t, operator, "web", privilegedCompose)
	got := needsAdmin(rv)
	if len(got) != 1 || got[0].Severity != stacks.SeverityError || got[0].Message != "This needs Admin: privileged: true." || got[0].File != "compose.yaml" || got[0].Line != 4 {
		t.Fatalf("problems = %+v, want one blocking \"This needs Admin: privileged: true.\" at compose.yaml:4", rv.Problems)
	}
	if !slices.Equal(rv.NeedsAdmin, []string{"extra privileges"}) {
		t.Errorf("needsAdmin = %v, want [extra privileges]", rv.NeedsAdmin)
	}

	rv = rig.reviewAs(t, admin, "web", privilegedCompose)
	if len(needsAdmin(rv)) != 0 || len(rv.NeedsAdmin) != 0 {
		t.Errorf("an env Admin's review = %+v, %v, want no policy problem", rv.Problems, rv.NeedsAdmin)
	}
}

func TestReviewJudgesABindWrittenAsAVariableByItsValue(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	operator := rig.principal(t, "op", "operator")

	rv := rig.reviewAs(t, operator, "web", dataBindCompose, stacks.Var{Name: "DATA", Value: "/var/run/docker.sock"})
	got := needsAdmin(rv)
	if len(got) != 1 || got[0].Message != "This needs Admin: a bind mount of /var/run/docker.sock." || got[0].Line != 5 {
		t.Fatalf("problems = %+v, want the socket named at line 5", rv.Problems)
	}
	if !slices.Equal(rv.NeedsAdmin, []string{"bind mounts"}) {
		t.Errorf("needsAdmin = %v", rv.NeedsAdmin)
	}
	rv = rig.reviewAs(t, operator, "web", dataBindCompose, stacks.Var{Name: "DATA", Value: "/srv/data"})
	if len(needsAdmin(rv)) != 0 {
		t.Errorf("a data root through a variable was refused: %+v", rv.Problems)
	}
}

func TestReviewRefusesWhatTheConvertedSpecAsksForThatTheComposeRulesDoNotSee(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	operator, admin := rig.principal(t, "op", "operator"), rig.principal(t, "adm", "admin")
	rv := rig.reviewAs(t, operator, "web", hostNetworkCompose)
	got := needsAdmin(rv)
	if len(got) != 1 || got[0].Line != 2 || !strings.HasPrefix(got[0].Message, "This needs Admin: ") {
		t.Fatalf("problems = %+v, want the host network refused at the service (line 2)", rv.Problems)
	}
	if !slices.Equal(rv.NeedsAdmin, []string{"host networking"}) {
		t.Errorf("needsAdmin = %v", rv.NeedsAdmin)
	}
	if rv := rig.reviewAs(t, admin, "web", hostNetworkCompose); len(needsAdmin(rv)) != 0 {
		t.Errorf("an env Admin was refused: %+v", rv.Problems)
	}
}

func TestPolicyAnswersTheEntriesTheRefusalListsAndAllowsACleanStack(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	operator := rig.principal(t, "op", "operator")
	policy := func(compose string, vars ...stacks.Var) stackops.PolicyResult {
		t.Helper()
		copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, Vars: vars}
		res := stacks.Check(testsupport.Context(t), stacks.CheckInput{Name: "web", Target: stacks.TargetSwarm, Copy: copy})
		plan, err := convert.Convert(res.Loaded.Project, convert.Options{StackName: "web", StackID: "stack-web", Objects: stackops.PlannedObjects("web", nil)})
		if err != nil {
			t.Fatal(err)
		}
		out, err := rig.deps.Policy(testsupport.Context(t), stackops.Caller{Principal: operator, Actor: "op"}, rig.envID, copy, plan)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if res := policy(stacks.StarterCompose); res.Refused || len(res.Problems) != 0 || len(res.Kinds) != 0 {
		t.Errorf("a clean stack: %+v", res)
	}
	res := policy(privilegedCompose)
	if !res.Refused || len(res.Entries) != 1 || res.Entries[0] != (stackops.PolicyEntry{Text: "privileged: true", File: "compose.yaml", Line: 4}) {
		t.Errorf("privileged: %+v", res)
	}
}

func TestPolicyWithNoCheckerOrNoCallerIsNeverReadAsAllowed(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	copy := stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(privilegedCompose)}}}
	if _, err := rig.deps.Policy(testsupport.Context(t), stackops.Caller{}, rig.envID, copy, convert.Plan{}); !errors.Is(err, stackops.ErrNoPolicy) {
		t.Errorf("no caller: %v, want ErrNoPolicy", err)
	}
	rig.deps.Checker = nil
	if _, err := rig.deps.Policy(testsupport.Context(t), stackops.Caller{Principal: rig.principal(t, "root", "")}, rig.envID, copy, convert.Plan{}); !errors.Is(err, stackops.ErrNoPolicy) {
		t.Errorf("no checker: %v, want ErrNoPolicy", err)
	}
}

func TestReviewBlocksOnAStackThatAnotherOwnerHoldsAndOnATargetThatChanged(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil, dockertest.FakeService{ID: "s1", Name: "web_old", Mode: "replicated", Image: "x", Labels: swarmNS})
	rv := rig.review(t, "web", stacks.StarterCompose, nil)
	var clash bool
	for _, p := range rv.Problems {
		if p.Code == stackops.CodeNameClash && p.Severity == stacks.SeverityError && p.Message == "A stack named web already exists on this environment, not managed by Coaming." {
			clash = true
		}
	}
	if !clash {
		t.Errorf("problems = %+v, want the name clash", rv.Problems)
	}
	if len(rv.Services) != 1 || rv.Services[0].Change != "added" {
		t.Errorf("services = %+v: a service Coaming does not own is not Coaming's to remove", rv.Services)
	}

	ri := rig.reviewInput(t, "web", stacks.StarterCompose, nil)
	ri.Stack.Target = stacks.TargetStandalone
	if _, err := rig.deps.Review(testsupport.Context(t), ri); !errors.Is(err, stackops.ErrTargetChanged) {
		t.Errorf("a standalone stack on a swarm: %v, want ErrTargetChanged", err)
	}
}

func TestReviewWithNoCheckerRefusesRatherThanSkipTheCheck(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	ri := rig.reviewInput(t, "web", stacks.StarterCompose, nil)
	rig.deps.Checker = nil
	if _, err := rig.deps.Review(testsupport.Context(t), ri); !errors.Is(err, stackops.ErrNoPolicy) {
		t.Errorf("review with no checker: %v, want ErrNoPolicy", err)
	}
}

// The Rendered YAML tab (plan 07-16, D-11, A11): the draft's rendering against the one stored with
// the last good revision.

func TestReviewOfAFirstDeployDiffsTheRenderingAgainstAnEmptyDocument(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	rv := rig.review(t, "tracer", stacks.StarterCompose, nil)
	if len(rv.YAML.Hunks) != 1 || rv.YAML.Hunks[0].OldLines != 0 || rv.YAML.Hunks[0].NewLines == 0 {
		t.Fatalf("yaml = %+v, want one hunk that adds the whole rendering", rv.YAML)
	}
	if rv.YAML.ChangedLines != rv.YAML.Hunks[0].NewLines || len(rv.YAML.Lines) != rv.YAML.Hunks[0].NewLines {
		t.Errorf("changed lines %d, lines %d, hunk %+v", rv.YAML.ChangedLines, len(rv.YAML.Lines), rv.YAML.Hunks[0])
	}
	if !strings.Contains(strings.Join(rv.YAML.Lines, "\n"), "image: nginx:1.27-alpine@"+dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine")) {
		t.Errorf("the rendering shows the image the review chose: %q", rv.YAML.Lines)
	}
}

func TestReviewOfADraftEqualToTheLastDeployHasNoYAMLChange(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	first := rig.review(t, "tracer", stacks.StarterCompose, nil)
	in := rig.reviewInput(t, "tracer", stacks.StarterCompose, nil)
	in.Rendering = []byte(strings.Join(first.YAML.Lines, "\n") + "\n")
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rv.YAML.Hunks) != 0 || rv.YAML.ChangedLines != 0 || len(rv.YAML.Lines) != 0 {
		t.Errorf("yaml = %+v, want no change against its own rendering", rv.YAML)
	}
}

func TestReviewOfOneChangedReplicaLineIsTwoChangedLinesInOneHunk(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	first := rig.review(t, "tracer", stacks.StarterCompose, nil)
	stored := strings.Join(first.YAML.Lines, "\n") + "\n"

	grown := strings.Replace(stacks.StarterCompose, "    image:", "    deploy:\n      replicas: 3\n    image:", 1)
	in := rig.reviewInput(t, "tracer", grown, nil)
	in.Rendering = []byte(stored)
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	if len(rv.YAML.Hunks) != 1 || rv.YAML.ChangedLines < 1 {
		t.Fatalf("yaml = %+v, want one hunk", rv.YAML)
	}
	var added []string
	for _, l := range rv.YAML.Hunks[0].Lines {
		if l.Kind == stacks.DiffAdded {
			added = append(added, strings.TrimSpace(l.Text))
		}
	}
	if !slices.Contains(added, "replicas: 3") {
		t.Errorf("added lines = %q, want replicas: 3", added)
	}
}

func TestReviewMasksASecretInTheRenderedYAML(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      API_KEY: ${API_KEY}\n"
	rv := rig.review(t, "shop", compose, nil, stacks.Var{Name: "API_KEY", Value: "supersecret-value", Secret: true})
	all := strings.Join(rv.YAML.Lines, "\n")
	if strings.Contains(all, "supersecret-value") || !strings.Contains(all, "<hidden>") {
		t.Errorf("rendering = %q, want the secret hidden", all)
	}
}

// problemMessages is the message of each problem: the words the review builds from the loader's or the
// converter's text. Keys are the file's own key paths and keep the tests' own assertions.
func problemMessages(ps []stacks.Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Message)
	}
	return out
}

// A problem the review adds after the check (a conversion refusal quotes the value it refuses) is masked
// too, whatever step found it (review WR-04, T-07-160).
func TestReviewNeverQuotesASecretInAProblem(t *testing.T) {
	t.Parallel()
	const secret = "hunter2secretvalue"
	rig := newSwarmRig(t, nil)
	compose := "services:\n  web:\n    image: nginx:1.27-alpine\n    deploy:\n      endpoint_mode: ${PW}\n"
	rv := rig.review(t, "shop", compose, nil, stacks.Var{Name: "PW", Value: secret, Secret: true})
	var found bool
	for _, p := range rv.Problems {
		if strings.Contains(p.Message, secret) || strings.Contains(p.Key, secret) {
			t.Errorf("problem = %+v, want the value hidden", p)
		}
		if p.Code == stackops.CodeConvertProblem {
			found = true
			if want := stacks.SecretValueMessage("services.web.deploy.endpoint_mode"); p.Message != want {
				t.Errorf("message = %q, want the fixed sentence %q", p.Message, want)
			}
		}
	}
	if !found {
		t.Fatalf("problems = %+v, want a conversion problem (the premise of the test)", rv.Problems)
	}
	stackstest.NoSecretRun(t, secret, problemMessages(rv.Problems)...)
}

// A review of a field the converter refuses, where compose-go split a secret before the converter saw
// it, never quotes part of the secret (07-42, D-02, T-07-180); a plain value keeps the converter's words.
func TestReviewNeverQuotesPartOfASecretTheConverterSplits(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	cases := map[string]struct {
		compose string
		secret  string
		// want is the Message exactly; contains is a phrase it holds when want is empty.
		want, contains string
		absent         string
	}{
		"restart":                   {compose: head + "    restart: ${PW}\n", secret: "sometimes:3", want: stacks.SecretValueMessage("services.web.restart"), absent: "sometimes"},
		"ports":                     {compose: head + "    ports:\n      - \"${PW}\"\n", secret: "127.0.0.1:8000-8001:80", want: stacks.SecretValueMessage("services.web.ports"), absent: "8000-8001"},
		"a plain value keeps words": {compose: head + "    dns:\n      - 8.8.8.x\n    environment:\n      TOKEN: ${PW}\n", secret: "Password2024!", contains: "is not an IP address"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, nil)
			rv := rig.review(t, "shop", tc.compose, nil, stacks.Var{Name: "PW", Value: tc.secret, Secret: true})
			var msg string
			var found bool
			for _, p := range rv.Problems {
				if p.Code == stackops.CodeConvertProblem {
					msg, found = p.Message, true
				}
				if strings.Contains(p.Message, tc.secret) {
					t.Errorf("problem = %+v, want the secret absent", p)
				}
			}
			if !found {
				t.Fatalf("problems = %+v, want a convert_failed problem", rv.Problems)
			}
			stackstest.NoSecretRun(t, tc.secret, problemMessages(rv.Problems)...)
			if tc.want != "" && msg != tc.want {
				t.Errorf("message = %q, want %q", msg, tc.want)
			}
			if tc.absent != "" && strings.Contains(msg, tc.absent) {
				t.Errorf("message = %q, want no %q", msg, tc.absent)
			}
			if tc.contains != "" && !strings.Contains(msg, tc.contains) {
				t.Errorf("message = %q, want it to contain %q", msg, tc.contains)
			}
		})
	}
}

// A deployment made after the draft was started (plan 07-16, STK-05, D-09): what it changed and
// what the draft changed, from the version the draft began with, file by file and masked.

const concurrentBase = "services:\n  web:\n    image: nginx:1.27-alpine\n    environment:\n      MODE: prod\n"

func copyOf(compose string, files []stacks.File, vars ...stacks.Var) stacks.WorkingCopy {
	return stacks.WorkingCopy{Files: append([]stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, files...), Vars: vars}
}

// concurrentInput is a review of draft whose base was revision 1 and whose newest good revision is
// deployed, deployed by bob.
func (r *swarmRig) concurrentInput(t *testing.T, base, deployed, draft stacks.WorkingCopy, deployedSeq int64) stackops.ReviewInput {
	t.Helper()
	in := r.reviewInput(t, "shop", string(draft.Compose()), draft.Files[1:], draft.Vars...)
	in.BaseSeq, in.Base = 1, &base
	in.Previous = &deployed
	in.Deployed = &stackops.DeployedRevision{Seq: deployedSeq, By: "bob", At: time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)}
	return in
}

func hunkLines(f stackops.FileHunks, kind byte) []string {
	var out []string
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == kind {
				out = append(out, strings.TrimSpace(l.Text))
			}
		}
	}
	return out
}

func TestReviewOfADraftStartedBeforeANewerDeployShowsWhatEachSideChanged(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	base := copyOf(concurrentBase, nil)
	deployed := copyOf(strings.Replace(concurrentBase, "MODE: prod", "MODE: staging", 1), nil)
	draft := copyOf(strings.Replace(concurrentBase, "image: nginx:1.27-alpine", "image: nginx:1.28-alpine", 1), nil)

	rv, err := rig.deps.Review(testsupport.Context(t), rig.concurrentInput(t, base, deployed, draft, 2))
	if err != nil {
		t.Fatal(err)
	}
	c := rv.Concurrent
	if c == nil {
		t.Fatal("concurrent = nil, want the deployment made after the draft started")
	}
	if c.Revision != 2 || c.DeployedBy != "bob" || !c.DeployedAt.Equal(time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("concurrent = revision %d by %q at %v", c.Revision, c.DeployedBy, c.DeployedAt)
	}
	if len(c.BaseToDeployed) != 1 || c.BaseToDeployed[0].Path != stacks.ComposeFile {
		t.Fatalf("base to deployed = %+v, want compose.yaml only", c.BaseToDeployed)
	}
	if got := hunkLines(c.BaseToDeployed[0], stacks.DiffAdded); len(got) != 1 || got[0] != "MODE: staging" {
		t.Errorf("deployed since added %q, want MODE: staging", got)
	}
	if got := hunkLines(c.BaseToDeployed[0], stacks.DiffRemoved); len(got) != 1 || got[0] != "MODE: prod" {
		t.Errorf("deployed since removed %q, want MODE: prod", got)
	}
	if len(c.BaseToDraft) != 1 {
		t.Fatalf("base to draft = %+v, want compose.yaml only", c.BaseToDraft)
	}
	if got := hunkLines(c.BaseToDraft[0], stacks.DiffAdded); len(got) != 1 || got[0] != "image: nginx:1.28-alpine" {
		t.Errorf("the draft added %q, want the new image", got)
	}
	if c.BaseToDeployed[0].ChangedLines != 2 || len(c.BaseToDeployed[0].Lines) == 0 {
		t.Errorf("changed lines %d and the newer lines %d, want 2 and the text for the unchanged lines", c.BaseToDeployed[0].ChangedLines, len(c.BaseToDeployed[0].Lines))
	}
}

func TestReviewOfADraftStartedFromTheNewestRevisionIsNotConcurrent(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	same := copyOf(concurrentBase, nil)
	in := rig.concurrentInput(t, same, same, same, 1) // revision 1 is both the base and the newest
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Concurrent != nil {
		t.Errorf("concurrent = %+v, want none: the draft started from the newest revision", rv.Concurrent)
	}

	in = rig.reviewInput(t, "shop", concurrentBase, nil)
	if rv, err = rig.deps.Review(testsupport.Context(t), in); err != nil || rv.Concurrent != nil {
		t.Errorf("a stack that was never deployed: concurrent = %+v, %v; want none", rv.Concurrent, err)
	}
}

func TestReviewNeverShowsASecretFileOrVariableInTheConcurrentDiffs(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := concurrentBase + "    secrets:\n      - tok\nsecrets:\n  tok:\n    file: tok.txt\n"
	secretFile := func(content string) []stacks.File {
		return []stacks.File{{Path: "tok.txt", Content: []byte(content), Secret: true}}
	}
	base := copyOf(compose, secretFile("token-one-value"), stacks.Var{Name: "API_KEY", Value: "key-one-value", Secret: true}, stacks.Var{Name: "REGION", Value: "eu"})
	deployed := copyOf(compose, secretFile("token-two-value"), stacks.Var{Name: "API_KEY", Value: "key-two-value", Secret: true}, stacks.Var{Name: "REGION", Value: "us"})
	draft := copyOf(compose, secretFile("token-three-value"), stacks.Var{Name: "API_KEY", Value: "key-three-value", Secret: true}, stacks.Var{Name: "REGION", Value: "eu"})

	rv, err := rig.deps.Review(testsupport.Context(t), rig.concurrentInput(t, base, deployed, draft, 2))
	if err != nil {
		t.Fatal(err)
	}
	if rv.Concurrent == nil {
		t.Fatal("concurrent = nil")
	}
	everything := fmt.Sprintf("%+v", *rv.Concurrent)
	for _, secret := range []string{"token-one-value", "token-two-value", "token-three-value", "key-one-value", "key-two-value", "key-three-value"} {
		if strings.Contains(everything, secret) {
			t.Errorf("the secret %q reached the concurrent diffs: %s", secret, everything)
		}
	}
	find := func(files []stackops.FileHunks, path string) (stackops.FileHunks, bool) {
		for _, f := range files {
			if f.Path == path {
				return f, true
			}
		}
		return stackops.FileHunks{}, false
	}
	tok, ok := find(rv.Concurrent.BaseToDeployed, "tok.txt")
	if !ok || !tok.Hidden || len(tok.Hunks) != 0 || tok.Variables {
		t.Errorf("tok.txt = %+v, %v; want a hidden change with nothing to show", tok, ok)
	}
	vars, ok := find(rv.Concurrent.BaseToDeployed, "variables")
	if !ok || !vars.Variables || !vars.Hidden {
		t.Fatalf("variables = %+v, %v; want a change, partly hidden", vars, ok)
	}
	if got := hunkLines(vars, stacks.DiffAdded); len(got) != 1 || got[0] != "REGION=us" {
		t.Errorf("variables added %q, want the plain variable's new value", got)
	}
	// The draft changed the secrets and left REGION alone: hidden, and no plain line.
	mine, ok := find(rv.Concurrent.BaseToDraft, "variables")
	if !ok || !mine.Hidden || len(mine.Hunks) != 0 {
		t.Errorf("draft variables = %+v, %v; want a hidden change and no lines", mine, ok)
	}
}

func TestReviewListsTheComposeFileFirstAndAFileTheOtherSideAddedOrRemoved(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	base := copyOf(concurrentBase, []stacks.File{{Path: "b.env", Content: []byte("A=1\n")}})
	deployed := copyOf(concurrentBase, []stacks.File{{Path: "a.env", Content: []byte("A=2\n")}})
	draft := copyOf(strings.Replace(concurrentBase, "prod", "dev", 1), []stacks.File{{Path: "b.env", Content: []byte("A=1\n")}})
	rv, err := rig.deps.Review(testsupport.Context(t), rig.concurrentInput(t, base, deployed, draft, 2))
	if err != nil || rv.Concurrent == nil {
		t.Fatalf("review = %+v, %v", rv.Concurrent, err)
	}
	var paths []string
	for _, f := range rv.Concurrent.BaseToDeployed {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"a.env", "b.env"}) {
		t.Errorf("base to deployed files = %v, want the one the deployment added, then the one it removed", paths)
	}
	var mine []string
	for _, f := range rv.Concurrent.BaseToDraft {
		mine = append(mine, f.Path)
	}
	if !slices.Equal(mine, []string{stacks.ComposeFile}) {
		t.Errorf("base to draft files = %v, want compose.yaml only", mine)
	}
}

func TestReviewInUpdateModeRefusesServicesTheStackDoesNotHaveAndLooksNothingUp(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, in := stackInput(t, "site", twoServiceCompose, nil)
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	lookups := len(rig.fake.DistributionRefs())

	for name, services := range map[string][]string{
		"an unknown service":          {"web", "ghost"},
		"no service":                  nil,
		"a name the swarm would give": {"site_web", "nope"},
	} {
		ri := rig.reviewInput(t, "site", twoServiceCompose, nil)
		ri.Mode, ri.Services = stackops.ReviewModeUpdate, services
		_, err := rig.deps.Review(testsupport.Context(t), ri)
		var invalid *stackops.ServicesInvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("%s: err = %v, want ServicesInvalidError", name, err)
		}
		if invalid.Empty != (len(services) == 0) || (len(services) > 0 && !slices.Contains(invalid.Unknown, services[len(services)-1])) {
			t.Errorf("%s: %+v names the wrong thing", name, invalid)
		}
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("a refused update made %d registry checks", n-lookups)
	}

	// The swarm's own name for a service is accepted on a swarm.
	ri := rig.reviewInput(t, "site", twoServiceCompose, nil)
	ri.Mode, ri.Services = stackops.ReviewModeUpdate, []string{"site_web"}
	if _, err := rig.deps.Review(testsupport.Context(t), ri); err != nil {
		t.Errorf("review by swarm name: %v", err)
	}
}

func TestUpdateServicesOfAStandaloneStackAreNamedAsInTheFileOnly(t *testing.T) {
	t.Parallel()
	project := &types.Project{Services: types.Services{"web": {Name: "web"}, "db": {Name: "db"}}}
	if err := stackops.ValidateUpdateServices(project, stacks.TargetStandalone, "demo", []string{"web", "db"}); err != nil {
		t.Errorf("services of the file: %v", err)
	}
	var invalid *stackops.ServicesInvalidError
	if err := stackops.ValidateUpdateServices(project, stacks.TargetStandalone, "demo", []string{"demo_web"}); !errors.As(err, &invalid) || !slices.Equal(invalid.Unknown, []string{"demo_web"}) {
		t.Errorf("a swarm name on a standalone host: %v, want it refused", err)
	}
	if err := stackops.ValidateUpdateServices(project, stacks.TargetSwarm, "demo", []string{"demo_web"}); err != nil {
		t.Errorf("a swarm name on a swarm: %v", err)
	}
}

func TestReviewInUpdateModeMarksOnlyTheRowWhoseRegistryIsDownAndBlocksOnlyForIt(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	compose := "services:\n  web:\n    image: nginx:1.27\n  app:\n    image: " + hostApp + "\n  cache:\n    image: redis:7\n"
	_, in := stackInput(t, "site", compose, nil)
	if res, _ := rig.deploy(t, in); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	moved := digestOf("e")
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", moved)
	rig.fake.SetDistributionFailure(hostApp, 500, registryDown)
	lookups := len(rig.fake.DistributionRefs())

	ri := rig.reviewInput(t, "site", compose, nil)
	ri.Mode, ri.Services = stackops.ReviewModeUpdate, []string{"web", "app"}
	rv, err := rig.deps.Review(testsupport.Context(t), ri)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]stackops.ImageRow{}
	for _, r := range rv.Images {
		rows[r.Service] = r
	}
	if r := rows["web"]; r.State != stackops.ImageNew || r.Digest != moved || r.Current == "" || r.Current == moved {
		t.Errorf("web = %+v, want its new digest looked up", r)
	}
	if r := rows["app"]; r.State != stackops.ImageUnreachable || r.Digest != "" || r.Note != "Can't reach the registry" || r.Current == "" {
		t.Errorf("app = %+v, want the registry marked unreachable on that row, with what it runs now", r)
	}
	if r := rows["cache"]; r.State != stackops.ImagePinned || r.Digest != r.Current {
		t.Errorf("cache = %+v, want it kept as it runs", r)
	}
	var blocking []string
	for _, p := range rv.Problems {
		if p.Severity == stacks.SeverityError {
			blocking = append(blocking, p.Code+" "+p.Key)
		}
	}
	if !slices.Equal(blocking, []string{stackops.CodeRegistryUnreachable + " services.app.image"}) {
		t.Errorf("blocking problems = %v, want app alone", blocking)
	}
	if _, ok := rv.Pins["site_app"]; ok || rv.Pins["site_web"] == "" || rv.Pins["site_cache"] == "" {
		t.Errorf("pins = %v, want web and cache and no pin for app", rv.Pins)
	}
	// Only the chosen services were looked up.
	for _, ref := range rig.fake.DistributionRefs()[lookups:] {
		if strings.Contains(ref, "redis") {
			t.Errorf("the review asked the registry about %s, which was not chosen", ref)
		}
	}
}

// TestReviewAdoptedSwarmStackShowsOwnershipOnly: the first review of an adopted stack that docker stack deploy made
// is not refused as a name clash, shows every service as differing only in Coaming's labels, counts them apart from
// the changed ones and leaves the stack's networks and secrets alone (D-16, D-20, ROADMAP SC3).
func TestReviewAdoptedSwarmStackShowsOwnershipOnly(t *testing.T) {
	t.Parallel()
	a := adoptCorpus(t, nil)

	rv := a.review(t)

	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v, want an adopted stack not refused as a name clash", rv.Problems)
	}
	var names []string
	for _, c := range rv.Services {
		if !c.OwnershipOnly {
			t.Errorf("service %s is %s with its own change, want ownership labels only", c.Short, c.Change)
		}
		names = append(names, c.Short)
	}
	if want := []string{"api", "bare", "legacy"}; !slices.Equal(names, want) {
		t.Errorf("services = %v, want %v", names, want)
	}
	if rv.Summary.OwnershipOnly != 3 || rv.Summary.Services != (stackops.Counts{}) {
		t.Errorf("summary = %+v, want 3 ownership-only and none changed", rv.Summary)
	}
	if rv.Adoption == nil || !rv.Adoption.Pending || rv.Adoption.OwnershipOnly != 3 {
		t.Errorf("adoption = %+v, want pending with 3 services", rv.Adoption)
	}
	for _, o := range rv.Objects {
		if o.Change != stackops.ChangeExternal {
			t.Errorf("object %s %s %s: want the existing networks and secrets neither added nor changed", o.Change, o.Kind, o.Name)
		}
	}
	for _, name := range []string{"db_password.h1", "tls_key.h1"} {
		if !slices.Contains(objectsOf(rv), "external secret "+name) {
			t.Errorf("objects = %v, want %s listed as external", objectsOf(rv), name)
		}
	}
	if w := a.rig.fake.Writes(); len(w) != 0 {
		t.Errorf("a review wrote %v", w)
	}
}

// TestReviewOfAnAdoptedStackKeepsARealChangeInTheList: a service with a real change as well as the labels is a
// normal changed row, counted as changed, and the others stay ownership-only.
func TestReviewOfAnAdoptedStackKeepsARealChangeInTheList(t *testing.T) {
	t.Parallel()
	a := adoptCorpus(t, nil)
	compose := strings.Replace(string(a.draft.Copy.Compose()), "replicas: 4", "replicas: 5", 1)
	if compose == string(a.draft.Copy.Compose()) {
		t.Fatal("the rebuilt file has no replicas: 4 to change")
	}
	a.draft.Copy = stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(compose)}}, Vars: a.draft.Copy.Vars}

	rv := a.review(t)

	if rv.Summary.Services.Changed != 1 || rv.Summary.OwnershipOnly != 2 {
		t.Errorf("summary = %+v, want one changed service and two ownership-only", rv.Summary)
	}
}

// TestReviewShowsThePlatformsARedeployPutsBack (G-08-4, P7 D-11, P7 D-18): a service that keeps its digest is sent the
// platforms the stack's last good revision recorded for that digest, and the review lists that change before the deploy,
// as the deploy then makes it. A record for another digest is not shown, and a stack whose services run the recorded
// platforms lists no change.
func TestReviewShowsThePlatformsARedeployPutsBack(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	kept := "nginx:1.27@" + digestOf("a")
	const withPlatforms = `"Platforms":[{"Architecture":"amd64","OS":"linux"},{"Architecture":"arm64","OS":"linux"}]`
	reviewWith := func(t *testing.T, rig *swarmRig, good map[string]stacks.LivePin) stackops.Review {
		t.Helper()
		in := rig.reviewInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins = &memPins{good: good}
		rv, err := rig.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return rv
	}
	siteChange := func(t *testing.T, rv stackops.Review) stacks.ServiceChange {
		t.Helper()
		for _, c := range rv.Services {
			if c.Name == "site_web" {
				return c
			}
		}
		t.Fatalf("review lists %+v, want a change for site_web", rv.Services)
		return stacks.ServiceChange{}
	}

	t.Run("the recorded platforms are listed as added", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil, siteWeb(kept))
		rv := reviewWith(t, rig, map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: kept, Platforms: []swarm.Platform{amd, arm}}})
		c := siteChange(t, rv)
		if c.Change != "changed" || !strings.Contains(string(c.Next), withPlatforms) || strings.Contains(string(c.Current), `"Platforms":[{`) {
			t.Errorf("site_web = %s, current %s, next %s; want a change that adds the recorded platforms", c.Change, c.Current, c.Next)
		}
		if n := len(rig.fake.DistributionRefs()); n != 0 {
			t.Errorf("distribution lookups = %d, want none for a kept digest", n)
		}
	})

	t.Run("a record for another digest is not shown", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil, siteWeb(kept))
		rv := reviewWith(t, rig, map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: "nginx:1.27@" + digestOf("b"), Platforms: []swarm.Platform{amd, arm}}})
		if c := siteChange(t, rv); strings.Contains(string(c.Next), `"Platforms":[{`) {
			t.Errorf("next = %s, want no platforms from a record of another digest (T-08-111)", c.Next)
		}
	})

	t.Run("services that run the recorded platforms list no change", func(t *testing.T) {
		t.Parallel()
		rig := newSwarmRig(t, nil)
		rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
		_, in := stackInput(t, "site", pinCompose("nginx:1.27"), nil)
		in.Pins, in.RevisionID = &memPins{}, "rev-1"
		if res, job := rig.deploy(t, in); !res.OK() || job.State != jobs.JobDone {
			t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
		}
		rv := reviewWith(t, rig, map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.27", Image: kept, Platforms: []swarm.Platform{amd, arm}}})
		if len(rv.Services) != 0 {
			t.Errorf("review = %+v, want no change: the service runs the recorded platforms", rv.Services)
		}
	})
}

// TestAReviewedDeploySendsThePlacementTheReviewShowed (WR-02): the review and the deploy that follows it apply the
// platform sources in one order, so the platforms the review lists for a service are the platforms the deploy sends. A
// stack whose last good revision recorded fewer platforms than the registry names for the same digest is sent the
// record by both.
func TestAReviewedDeploySendsThePlacementTheReviewShowed(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	pinned := "nginx:1.27@" + digestOf("a")
	compose := pinCompose("nginx:1.27")
	rig := newSwarmRig(t, nil)
	rig.deps.Platforms = stackops.NewPlatformMemo()
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27", digestOf("a"))
	// The record was made for the text nginx:1.26, so the review looks nginx:1.27 up; the registry names the same digest.
	good := map[string]stacks.LivePin{"site_web": {AsWritten: "nginx:1.26", Image: pinned, Platforms: []swarm.Platform{amd}}}

	in := rig.reviewInput(t, "site", compose, nil)
	in.Pins = &memPins{good: good}
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	var shown []swarm.Platform
	for _, c := range rv.Services {
		if c.Name != "site_web" {
			continue
		}
		var spec swarm.ServiceSpec
		if err := json.Unmarshal(c.Next, &spec); err != nil {
			t.Fatalf("review's next spec: %v", err)
		}
		if spec.TaskTemplate.Placement != nil {
			shown = spec.TaskTemplate.Placement.Platforms
		}
	}

	deploy := reviewedDeploy(t, rig, "site", compose, rv)
	deploy.Pins = &memPins{good: good}
	res, job := rig.deploy(t, deploy)
	if !res.OK() || job.State != jobs.JobDone {
		t.Fatalf("deploy = %+v job %s %v", res, job.State, itemStates(job))
	}
	creates := rig.fake.ServiceCreates()
	if len(creates) != 1 || creates[0].Spec.TaskTemplate.Placement == nil {
		t.Fatalf("service creates = %+v, want one with a placement", creates)
	}
	sent := creates[0].Spec.TaskTemplate.Placement.Platforms
	if !slices.Equal(shown, sent) {
		t.Errorf("the review showed platforms %v, the deploy sent %v, want the same", shown, sent)
	}
	if !slices.Equal(sent, []swarm.Platform{amd}) {
		t.Errorf("the deploy sent %v, want the platforms the last good revision recorded %v", sent, []swarm.Platform{amd})
	}
}
