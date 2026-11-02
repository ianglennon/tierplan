// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stackops_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stackops/stackopstest"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Reverts drift (plan 08-25, STK-14, D-25): a deploy review lists, apart from the draft's own changes, every outside
// change the deploy would put back: the three-way set of the deployed revision, the draft and what runs.

// failingPins is a PinStore that cannot read the pins of the last good revision.
type failingPins struct{}

func (failingPins) GoodPins(context.Context, string) (map[string]stacks.LivePin, error) {
	return nil, errors.New("the pins are unreadable")
}

func (failingPins) SetPins(context.Context, string, map[string]string) error { return nil }

func (failingPins) SetPlatforms(context.Context, string, map[string][]swarm.Platform) error {
	return nil
}

// revertsInput is a review of draft as the stack drift's deploy, with its deployed revision, pins and audit log as the
// route supplies them.
func (r *driftRig) revertsInput(t *testing.T, draft string) stackops.ReviewInput {
	t.Helper()
	rev := r.store.rev
	in := r.reviewInput(t, r.st.Name, draft, nil)
	in.Stack, in.Previous, in.Pins, in.Audit = r.st, &rev.Copy, &memPins{good: stackops.LivePins(r.store.pins)}, r.swarmRig.store
	in.Deployed = &stackops.DeployedRevision{Seq: rev.Seq, By: "bob", At: causeEpoch}
	return in
}

func (r *driftRig) reverts(t *testing.T, draft string) stackops.Review {
	t.Helper()
	rv, err := r.deps.Review(testsupport.Context(t), r.revertsInput(t, draft))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rv.Problems.Errors() != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	return rv
}

// revertRows says what a review reverts, one line each: the service, what happens, the setting, what runs and what the
// deploy sets.
func revertRows(rv stackops.Review) []string {
	var out []string
	for _, r := range rv.RevertsDrift {
		out = append(out, fmt.Sprintf("%s %s %s %q -> %q", r.Service, r.Change, r.Path, r.Live, r.Desired))
	}
	return out
}

func replicasIn(compose string, n int) string {
	return strings.Replace(compose, "replicas: 2", fmt.Sprintf("replicas: %d", n), 1)
}

func TestRevertsDriftThreeWay(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3)) // a hot-fix: the revision deployed 2

	rv := rig.reverts(t, driftCompose)
	if got, want := revertRows(rv), []string{`web changed Mode.Replicated.Replicas "3" -> "2"`}; !slices.Equal(got, want) {
		t.Fatalf("a draft that keeps the deployed value: reverts = %q, want %q", got, want)
	}
	if rv.Summary.RevertsDrift != 1 || rv.RevertsDriftUnavailable {
		t.Errorf("summary = %+v unavailable = %v, want one revert and the section available", rv.Summary, rv.RevertsDriftUnavailable)
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Summary.Services.Changed != 1 {
		t.Errorf("services = %v, want the change the draft makes to web still listed as it was (the section adds to the review)", changesOf(rv))
	}

	// The draft sets the value to something else: that is its own change, not a revert.
	if rows := revertRows(rig.reverts(t, replicasIn(driftCompose, 5))); len(rows) != 0 {
		t.Errorf("a draft that scales to 5: reverts = %q, want none: the draft's own change", rows)
	}
	// The draft takes the hot-fix in: it reverts nothing.
	if rows := revertRows(rig.reverts(t, replicasIn(driftCompose, 3))); len(rows) != 0 {
		t.Errorf("a draft that accepts the 3 replicas: reverts = %q, want none", rows)
	}
	// An unrelated change of the draft's own is not in the section either, and the hot-fix still is.
	own := strings.Replace(driftCompose, "MODE: prod", "MODE: staging", 1)
	if got, want := revertRows(rig.reverts(t, own)), []string{`web changed Mode.Replicated.Replicas "3" -> "2"`}; !slices.Equal(got, want) {
		t.Errorf("a draft that edits MODE: reverts = %q, want only the replicas", got)
	}
}

func TestRevertsDriftAddedByHand(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "HOTFIX=1")
	})

	rv := rig.reverts(t, driftCompose)

	// Neither the revision nor the draft has the variable, so the deploy removes it.
	if got, want := revertRows(rv), []string{`web changed TaskTemplate.ContainerSpec.Env[HOTFIX] "HOTFIX=1" -> ""`}; !slices.Equal(got, want) {
		t.Fatalf("reverts = %q, want %q", got, want)
	}
	withIt := strings.Replace(driftCompose, "MODE: prod", "MODE: prod\n      HOTFIX: \"1\"", 1)
	if rows := revertRows(rig.reverts(t, withIt)); len(rows) != 0 {
		t.Errorf("a draft that has the variable: reverts = %q, want none", rows)
	}
}

func TestRevertsDriftMissingAndExtra(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	gone := rig.ownedNow(t, rig.st)["drift_gone"]
	extra := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "drift_extra", Labels: map[string]string{
			stacks.LabelSwarmNamespace: "drift", stacks.LabelStackID: rig.st.ID,
		}},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "busybox:1"}},
	}
	err := rig.deps.Aggregator.WriteOnManager(testsupport.Context(t), rig.envID, func(c *dockerx.Client) error {
		if err := c.ServiceRemove(context.Background(), gone.ID); err != nil {
			return err
		}
		_, _, err := c.ServiceCreate(context.Background(), mustJSON(t, extra), "")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// The draft keeps `gone` and has no `extra`: the deploy creates the one again and removes the other.
	rv := rig.reverts(t, driftCompose)
	if got, want := revertRows(rv), []string{`extra removed  "" -> ""`, `gone created  "" -> ""`}; !slices.Equal(got, want) {
		t.Fatalf("reverts = %q, want %q", got, want)
	}

	// A draft that drops `gone` and declares `extra` is doing both itself.
	own := strings.Replace(driftCompose, "  gone:\n    image: nginx:1.29-alpine\n", "  extra:\n    image: busybox:1\n", 1)
	if rows := revertRows(rig.reverts(t, own)); len(rows) != 0 {
		t.Errorf("a draft that drops gone and declares extra: reverts = %q, want none: they are the draft's own changes", rows)
	}
}

func TestRevertsDriftCause(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	rig.editLive(t, "api", func(s *swarm.ServiceSpec) {
		s.TaskTemplate.ContainerSpec.Env = append(s.TaskTemplate.ContainerSpec.Env, "OUTSIDE=1")
	})
	at := causeEpoch.Add(time.Hour)
	rig.record(t, scaleRow(at, "alice", rig.serviceID(t, "web"), rig.envID, "2", "3"))

	rv := rig.reverts(t, driftCompose)

	if len(rv.RevertsDrift) != 2 {
		t.Fatalf("reverts = %q, want the scaled web and the api changed by hand", revertRows(rv))
	}
	api, web := rv.RevertsDrift[0], rv.RevertsDrift[1]
	if api.Service != "api" || web.Service != "web" {
		t.Fatalf("rows = %q, want api then web", revertRows(rv))
	}
	want := stackops.DriftCause{Action: stackops.CauseScaled, Service: "web", By: "alice", At: at, From: "2", To: "3"}
	if web.Cause == nil || *web.Cause != want {
		t.Errorf("web cause = %+v, want %+v", web.Cause, want)
	}
	if api.Cause != nil {
		t.Errorf("api cause = %+v, want none: nothing in the audit log explains it, which reads as changed outside TierPlan", api.Cause)
	}
}

func TestRevertsDriftNotInRestore(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	rev := rig.store.rev

	in := rig.revertsInput(t, driftCompose)
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRestore, &rev, rig.store.pins, rev.Copy
	restore, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("restore review: %v", err)
	}
	if len(restore.RevertsDrift) != 0 || restore.RevertsDriftUnavailable || restore.Summary.RevertsDrift != 0 {
		t.Errorf("restore: reverts = %q unavailable = %v, want no section: its whole Changes tab is the drift", revertRows(restore), restore.RevertsDriftUnavailable)
	}
	if len(restore.Services) != 1 || restore.Services[0].Short != "web" {
		t.Errorf("restore services = %v, want the drift listed as the changes", changesOf(restore))
	}

	// A roll back to a revision that has the deployed value for the setting does put it back.
	in = rig.revertsInput(t, driftCompose)
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRollback, &rev, rig.store.pins, rev.Copy
	rollback, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("rollback review: %v", err)
	}
	if got, want := revertRows(rollback), []string{`web changed Mode.Replicated.Replicas "3" -> "2"`}; !slices.Equal(got, want) {
		t.Errorf("rollback: reverts = %q, want %q", got, want)
	}
}

func TestRevertsDriftUnavailable(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	rig.editLive(t, "web", replicasOf(3))
	baseline := rig.reverts(t, replicasIn(driftCompose, 4))

	// The pins of the deployed revision cannot be read: the section says so and the rest of the review is as it was.
	in := rig.revertsInput(t, replicasIn(driftCompose, 4))
	in.Pins = failingPins{}
	broken, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !broken.RevertsDriftUnavailable || len(broken.RevertsDrift) != 0 || broken.Summary.RevertsDrift != 0 {
		t.Errorf("unreadable pins: reverts = %q unavailable = %v, want the unavailable line and no rows", revertRows(broken), broken.RevertsDriftUnavailable)
	}
	if !slices.Equal(changesOf(broken), changesOf(baseline)) || broken.DraftVersion != baseline.DraftVersion || len(broken.Problems) != len(baseline.Problems) {
		t.Errorf("review = %v, want the rest of it unchanged (%v)", changesOf(broken), changesOf(baseline))
	}

	// A revision that stored no image for a service cannot be converted any more, so there is nothing to compare.
	in = rig.revertsInput(t, driftCompose)
	in.Pins = &memPins{good: map[string]stacks.LivePin{}}
	unknown, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if !unknown.RevertsDriftUnavailable || len(unknown.RevertsDrift) != 0 {
		t.Errorf("a revision that cannot be redeployed: reverts = %q unavailable = %v, want the unavailable line", revertRows(unknown), unknown.RevertsDriftUnavailable)
	}

	// A stack that was never deployed has no revision to revert to: no rows and no line.
	in = rig.revertsInput(t, driftCompose)
	in.Deployed, in.Previous = nil, nil
	first, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if first.RevertsDriftUnavailable || len(first.RevertsDrift) != 0 {
		t.Errorf("no deployed revision: reverts = %q unavailable = %v, want neither", revertRows(first), first.RevertsDriftUnavailable)
	}
}

func TestRevertsDriftStandalone(t *testing.T) {
	t.Parallel()
	// docker update --restart always, and an environment entry added: two outside changes on the host.
	rig := hostedDrift(t, driftHostedCompose, func(h *stackopstest.Hosted) {
		h.Docs["web"].Config.Env = append(h.Docs["web"].Config.Env, "EXTRA=1")
		h.Docs["web"].HostConfig.RestartPolicy.Name = "always"
	})
	review := func(draft string) stackops.Review {
		t.Helper()
		in := rig.standaloneReviewInput(t, "demo", draft, &memPins{good: stackops.LivePins(rig.h.Pins)})
		in.Previous = &rig.input().Deployed.Copy
		in.Deployed = &stackops.DeployedRevision{Seq: 1, By: "bob", At: causeEpoch}
		rv, err := rig.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		return rv
	}

	rv := review(driftHostedCompose)
	var paths []string
	for _, r := range rv.RevertsDrift {
		if r.Service != "web" || r.Change != stackops.RevertChanged {
			t.Errorf("row = %+v, want a changed setting of web", r)
		}
		paths = append(paths, r.Path+"="+r.Live+">"+r.Desired)
	}
	if want := []string{"environment.EXTRA=1>", "restart=always>unless-stopped"}; !slices.Equal(paths, want) {
		t.Fatalf("reverts = %q, want %q", paths, want)
	}
	if rv.Summary.RevertsDrift != 2 || rv.RevertsDriftUnavailable {
		t.Errorf("summary = %+v unavailable = %v", rv.Summary, rv.RevertsDriftUnavailable)
	}

	// The draft sets the restart policy itself: that setting is no longer a revert.
	own := strings.Replace(driftHostedCompose, "restart: unless-stopped", "restart: on-failure", 1)
	rv = review(own)
	if len(rv.RevertsDrift) != 1 || rv.RevertsDrift[0].Path != "environment.EXTRA" {
		t.Errorf("a draft that changes the restart policy: reverts = %+v, want only the environment entry", rv.RevertsDrift)
	}
}

// The rules of the three-way set on their own, over the JSON of the services.
func TestThreeWayRules(t *testing.T) {
	t.Parallel()
	j := func(s string) json.RawMessage { return json.RawMessage(s) }
	deployed := map[string]json.RawMessage{
		"a": j(`{"replicas":1,"image":"x:1","env":["K=1","L=2"]}`),
		"b": j(`{"replicas":1}`),
	}
	live := map[string]json.RawMessage{
		"a": j(`{"replicas":3,"image":"x:2","env":["K=1","L=2","H=1"]}`),
		"c": j(`{}`),
	}
	desired := map[string]json.RawMessage{
		"a": j(`{"replicas":1,"image":"x:3","env":["K=1","L=2"]}`),
		"b": j(`{"replicas":1}`),
	}

	var got []string
	for _, r := range stackops.RevertsDrift(deployed, desired, live) {
		got = append(got, fmt.Sprintf("%s %s %s %q -> %q", r.Service, r.Change, r.Path, r.Live, r.Desired))
	}
	want := []string{
		`a changed env[2] "H=1" -> ""`,
		`a changed replicas "3" -> "1"`,
		`b created  "" -> ""`,
		`c removed  "" -> ""`,
	}
	slices.Sort(got)
	slices.Sort(want)
	// The image is the draft's own change (x:3 is neither x:1 nor x:2), so it is not listed.
	if !slices.Equal(got, want) {
		t.Errorf("reverts = %q, want %q", got, want)
	}
}

// TestRevertsDriftListsAPlatformsChangeMadeOutside (G-08-4, D-25, T-08-112): the platforms the deployed revision recorded
// come from the pins of its last good revision, so a deploy that would put back platforms changed outside TierPlan lists
// them as it lists any other outside change.
func TestRevertsDriftListsAPlatformsChangeMadeOutside(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}
	rig := newDriftRig(t)
	rig.editLive(t, "web", setLivePlatforms(amd, arm))
	in := rig.revertsInput(t, driftCompose)
	good := stackops.LivePins(rig.store.pins)
	web := good[rig.st.Name+"_web"]
	web.Platforms = []swarm.Platform{amd}
	good[rig.st.Name+"_web"] = web
	in.Pins = &memPins{good: good}

	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	var found bool
	for _, r := range rv.RevertsDrift {
		found = found || (r.Service == "web" && strings.Contains(r.Path, "Placement.Platforms"))
	}
	if !found || rv.RevertsDriftUnavailable {
		t.Fatalf("reverts = %q unavailable = %v, want a Placement.Platforms row for web: the deploy puts back what the revision recorded", revertRows(rv), rv.RevertsDriftUnavailable)
	}
}

func TestRevertLinesReadAsOneLineEach(t *testing.T) {
	t.Parallel()
	got := stackops.RevertLines([]stackops.DriftRevert{
		{Service: "web", Change: stackops.RevertChanged, Path: "Mode.Replicated.Replicas", Live: "5", Desired: "3"},
		{Service: "web", Change: stackops.RevertChanged, Path: "TaskTemplate.ContainerSpec.Image", Live: "nginx:1.28", Desired: "nginx:1.27"},
		{Service: "web", Change: stackops.RevertChanged, Path: "TaskTemplate.ContainerSpec.Env[DEBUG]", Live: "1"},
		{Service: "web", Change: stackops.RevertChanged, Path: "Labels[com.example.tier]", Live: "x", Desired: "y"},
		{Service: "web", Change: stackops.RevertChanged, Path: "TaskTemplate.Resources.Limits.NanoCPUs", Live: "1", Desired: "2"},
		{Service: "db", Change: stackops.RevertCreated},
		{Service: "old", Change: stackops.RevertRemoved},
	})
	want := []string{
		"web replicas 5 → 3",
		"web image nginx:1.28 → nginx:1.27",
		"web Env DEBUG 1 → unset",
		"web Labels com.example.tier x → y",
		"web NanoCPUs 1 → 2",
		"db created again",
		"old removed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("RevertLines =\n%q\nwant\n%q", got, want)
	}
	if got := stackops.RevertLines(nil); got == nil || len(got) != 0 {
		t.Errorf("stackops.RevertLines(nil) = %#v, want an empty list that is not nil", got)
	}
}
