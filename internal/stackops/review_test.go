// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/stacks/convert"
	"github.com/ianglennon/bayplan/proto/testsupport"
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
		if p.Code == stackops.CodeNameClash && p.Severity == stacks.SeverityError && p.Message == "A stack named web already exists on this environment, not managed by Bayplan." {
			clash = true
		}
	}
	if !clash {
		t.Errorf("problems = %+v, want the name clash", rv.Problems)
	}
	if len(rv.Services) != 1 || rv.Services[0].Change != "added" {
		t.Errorf("services = %+v: a service Bayplan does not own is not Bayplan's to remove", rv.Services)
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
