// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stackops/stackopstest"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The review of a roll back (plan 08-03, D-02, D-05): a revision's own converted stack, with the digests it
// stored, against what is running.

// rollbackReviewInput is a review of rolling back to a revision whose copy is composeText and which ran pins.
func (r *swarmRig) rollbackReviewInput(t *testing.T, name, composeText string, pins map[string]string) stackops.ReviewInput {
	t.Helper()
	target := stackops.Snapshot{
		Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(composeText)}}},
	}
	in := r.reviewInput(t, name, composeText, nil)
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRollback, &target, pins, target.Copy
	return in
}

func TestRollbackReviewUsesTheStoredDigests(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, first := stackInput(t, "web", poolsCompose, nil)
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	// The tag has moved on the registry since the revision ran: a roll back must not follow it.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", "sha256:"+strings.Repeat("b", 64))
	stored := "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)
	lookups, writes := len(rig.fake.DistributionRefs()), len(rig.fake.Writes())

	rv, err := rig.deps.Review(testsupport.Context(t), rig.rollbackReviewInput(t, "web", poolsCompose, map[string]string{"web_web": stored}))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back review asks no registry", lookups, n)
	}
	if n := len(rig.fake.Writes()); n != writes {
		t.Errorf("a review wrote %v", rig.fake.Writes()[writes:])
	}
	if len(rv.Images) != 1 {
		t.Fatalf("images = %+v, want one row", rv.Images)
	}
	row := rv.Images[0]
	if row.Service != "web" || row.State != stackops.ImagePinned || row.Note != "revision" ||
		row.Digest != "sha256:"+strings.Repeat("a", 64) || row.Current == "" || row.Current == row.Digest {
		t.Errorf("image row = %+v, want web pinned to the stored digest, which differs from what runs", row)
	}
	if rv.Summary.ImagesNew != 0 || rv.Summary.ImagesPinned != 1 {
		t.Errorf("summary = %+v, want the one image pinned and none new", rv.Summary)
	}
	if rv.Pins["web_web"] != stored {
		t.Errorf("pins = %v, want the stored reference", rv.Pins)
	}
	if len(rv.Services) != 1 || rv.Services[0].Change != "changed" || rv.Services[0].Short != "web" {
		t.Errorf("services = %+v, want web changed (the image moves to the stored digest)", rv.Services)
	}
	if rv.Concurrent != nil {
		t.Errorf("concurrent = %+v, a roll back review has none", rv.Concurrent)
	}
}

func TestRollbackReviewOfWhatRunsFindsNothingToDo(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	_, first := stackInput(t, "web", poolsCompose, nil)
	if res, _ := rig.deploy(t, first); !res.OK() {
		t.Fatalf("deploy = %+v", res)
	}
	running := "nginx:1.27-alpine@" + dockertest.FakeDigestOf("docker.io/library/nginx:1.27-alpine")
	rv, err := rig.deps.Review(testsupport.Context(t), rig.rollbackReviewInput(t, "web", poolsCompose, map[string]string{"web_web": running}))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 || len(rv.Problems) != 0 {
		t.Errorf("review = services %+v objects %v problems %+v, want nothing to roll back", rv.Services, objectsOf(rv), rv.Problems)
	}
}

func TestRollbackReviewRefusesAnUndeployableRevision(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, nil)
	// The revision stored no image for the service its file declares.
	rv, err := rig.deps.Review(testsupport.Context(t), rig.rollbackReviewInput(t, "web", poolsCompose, map[string]string{}))
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Problems) != 1 || rv.Problems[0].Code != stackops.CodeRevisionNotDeployable || rv.Problems[0].Severity != stacks.SeverityError {
		t.Fatalf("problems = %+v, want one blocking revision_not_deployable", rv.Problems)
	}
	if !strings.Contains(rv.Problems[0].Message, "revision #1") || !strings.Contains(rv.Problems[0].Message, "web_web") {
		t.Errorf("message = %q, want it to name the revision and the service", rv.Problems[0].Message)
	}
	if len(rv.Images) != 0 || len(rv.Services) != 0 {
		t.Errorf("review = images %+v services %+v, want nothing beside the problem", rv.Images, rv.Services)
	}
}

// standaloneRollbackInput is a review of rolling a standalone stack back to a revision whose copy is composeText and
// which ran pins (by service name in the compose file).
func (r *swarmRig) standaloneRollbackInput(t *testing.T, name, composeText string, pins map[string]string) stackops.ReviewInput {
	t.Helper()
	target := stackops.Snapshot{
		Seq: 1, Kind: stackops.KindRevision, Outcome: stackops.OutcomeGood,
		Copy: stacks.WorkingCopy{Files: []stacks.File{{Path: stacks.ComposeFile, Content: []byte(composeText)}}},
	}
	in := r.standaloneReviewInput(t, name, composeText, nil)
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRollback, &target, pins, target.Copy
	return in
}

func TestStandaloneRollbackReview(t *testing.T) {
	t.Parallel()
	// The host runs the default pins of the hosted stack (digest a). The revision to restore ran web at digest b.
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))
	older := "nginx:1.27-alpine@" + stackopstest.Digest("b")
	runningDB := "postgres:16@" + stackopstest.Digest("a")
	// The tag has moved on the registry since the revision ran: a roll back must not follow it.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", stackopstest.Digest("c"))
	lookups, writes := len(rig.fake.DistributionRefs()), len(rig.fake.Writes())

	rv, err := rig.deps.Review(testsupport.Context(t), rig.standaloneRollbackInput(t, "demo", hostedCompose, map[string]string{"web": older, "db": runningDB}))

	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a roll back review asks no registry", lookups, n)
	}
	if n := len(rig.fake.Writes()); n != writes {
		t.Errorf("a review wrote %v", rig.fake.Writes()[writes:])
	}
	if got := changesOf(rv); !slices.Equal(got, []string{"changed web"}) {
		t.Fatalf("services = %v, want web changed (the image moves to the stored digest) and db left alone", got)
	}
	if rv.Pins["web"] != older || rv.Pins["db"] != runningDB {
		t.Errorf("pins = %v, want the stored references", rv.Pins)
	}
	if len(rv.Images) != 2 {
		t.Fatalf("images = %+v, want one row for each service", rv.Images)
	}
	for _, row := range rv.Images {
		if row.State != stackops.ImagePinned || row.Note != "revision" {
			t.Errorf("image row = %+v, want pinned with the note revision", row)
		}
	}
	web := rv.Images[1]
	if web.Service != "web" || web.Digest != stackopstest.Digest("b") || web.Current != stackopstest.Digest("a") {
		t.Errorf("web image row = %+v, want the stored digest against the running one", web)
	}
	if rv.Summary.ImagesNew != 0 || rv.Summary.ImagesPinned != 2 || rv.Summary.Services.Changed != 1 {
		t.Errorf("summary = %+v, want two images pinned, none new and one service changed", rv.Summary)
	}
	if rv.Concurrent != nil {
		t.Errorf("concurrent = %+v, a roll back review has none", rv.Concurrent)
	}
}

func TestStandaloneRollbackReviewOfWhatRunsFindsNothingToDo(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))
	running := map[string]string{"web": "nginx:1.27-alpine@" + stackopstest.Digest("a"), "db": "postgres:16@" + stackopstest.Digest("a")}

	rv, err := rig.deps.Review(testsupport.Context(t), rig.standaloneRollbackInput(t, "demo", hostedCompose, running))

	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 || len(rv.Problems) != 0 {
		t.Errorf("review = services %v objects %v problems %+v, want nothing to roll back", changesOf(rv), objectsOf(rv), rv.Problems)
	}
}

func TestStandaloneRollbackReviewListsOnlyWhatTheRevisionDeclaresToCreate(t *testing.T) {
	t.Parallel()
	// The host holds the stack as the file declares it, with the volume. The revision to restore declares the same
	// services and a network of its own, which the host lacks: it is created. The volume a later revision added
	// and this one does not declare is not listed: a deploy leaves it on the host (D-52).
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))
	earlier := "services:\n  web:\n    image: nginx:1.27-alpine\n    networks: [front]\n    ports:\n      - \"8080:80\"\n    restart: unless-stopped\n    environment:\n      MODE: prod\n    depends_on: [db]\n  db:\n    image: postgres:16\nnetworks:\n  front: {}\n"
	pins := map[string]string{"web": "nginx:1.27-alpine@" + stackopstest.Digest("a"), "db": "postgres:16@" + stackopstest.Digest("a")}

	rv, err := rig.deps.Review(testsupport.Context(t), rig.standaloneRollbackInput(t, "demo", earlier, pins))

	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if got := objectsOf(rv); !slices.Equal(got, []string{"added network demo_front"}) {
		t.Errorf("objects = %v, want the revision's network alone, and no removed volume", got)
	}
}

func TestStandaloneRollbackReviewRefusesAnUndeployableRevision(t *testing.T) {
	t.Parallel()
	rig := hostRig(t, hostStack(t, "demo", hostedCompose, nil))

	rv, err := rig.deps.Review(testsupport.Context(t), rig.standaloneRollbackInput(t, "demo", hostedCompose, map[string]string{"web": "nginx:1.27-alpine@" + stackopstest.Digest("a")}))

	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Problems) != 1 || rv.Problems[0].Code != stackops.CodeRevisionNotDeployable || !strings.Contains(rv.Problems[0].Message, "db") {
		t.Fatalf("problems = %+v, want one blocking revision_not_deployable naming db", rv.Problems)
	}
	if len(rv.Images) != 0 || len(rv.Services) != 0 {
		t.Errorf("review = images %+v services %v, want nothing beside the problem", rv.Images, changesOf(rv))
	}
}

// The review of a restore (plan 08-23, STK-14, D-23): the deployed revision with its stored digests against what runs, so
// that what it lists is the drift.

func TestRestoreReviewIsTheDrift(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	// Somebody scaled web on the swarm; the revision deployed 2 replicas.
	rig.editLive(t, "web", replicasOf(3))
	// The tag has moved on the registry since: a restore must not follow it.
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", "sha256:"+strings.Repeat("b", 64))
	lookups, writes := len(rig.fake.DistributionRefs()), len(rig.fake.Writes())

	deployed := rig.store.rev
	in := rig.reviewInput(t, rig.st.Name, driftCompose, nil)
	in.Stack = rig.st
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRestore, &deployed, rig.store.pins, deployed.Copy
	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	if len(rv.Problems) != 0 {
		t.Fatalf("problems = %+v", rv.Problems)
	}
	if len(rv.Services) != 1 || rv.Services[0].Short != "web" || rv.Services[0].Change != "changed" {
		t.Fatalf("services = %+v, want web alone, changed: that is the drift", rv.Services)
	}
	if cur, next := string(rv.Services[0].Current), string(rv.Services[0].Next); !strings.Contains(cur, `"Replicas":3`) || !strings.Contains(next, `"Replicas":2`) {
		t.Errorf("web current = %s next = %s, want its replicas going from 3 back to 2", cur, next)
	}
	if rv.Summary.Services.Changed != 1 || rv.Summary.Services.Added != 0 || rv.Summary.Services.Removed != 0 {
		t.Errorf("summary = %+v, want the one change", rv.Summary.Services)
	}
	if n := len(rig.fake.DistributionRefs()); n != lookups {
		t.Errorf("distribution checks went from %d to %d, a restore review asks no registry", lookups, n)
	}
	if n := len(rig.fake.Writes()); n != writes {
		t.Errorf("a review wrote %v", rig.fake.Writes()[writes:])
	}
	for _, row := range rv.Images {
		if row.State != stackops.ImagePinned || row.Note != "revision" {
			t.Errorf("image row = %+v, want it pinned to the digest the revision stored", row)
		}
	}
	if rv.Concurrent != nil {
		t.Errorf("concurrent = %+v, a restore review has none", rv.Concurrent)
	}
}

func TestRestoreReviewOfWhatIsDeployedFindsNothingToDo(t *testing.T) {
	t.Parallel()
	rig := newDriftRig(t)
	deployed := rig.store.rev
	in := rig.reviewInput(t, rig.st.Name, driftCompose, nil)
	in.Stack = rig.st
	in.Mode, in.Target, in.TargetPins, in.Copy = stackops.ReviewModeRestore, &deployed, rig.store.pins, deployed.Copy

	rv, err := rig.deps.Review(testsupport.Context(t), in)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(rv.Services) != 0 || len(rv.Objects) != 0 || len(rv.Problems) != 0 {
		t.Errorf("review = services %+v objects %v problems %+v, want no drift to restore", rv.Services, objectsOf(rv), rv.Problems)
	}
}

// platformsRows are the Placement.Platforms paths a review lists, by service.
func platformsRows(t *testing.T, rv stackops.Review) []string {
	t.Helper()
	var out []string
	for _, p := range gapPaths(t, rv.Services) {
		if strings.Contains(p, "Placement.Platforms") {
			out = append(out, p)
		}
	}
	return out
}

// TestRollbackAndRestoreReviewsShowThePlatformsTheRevisionStored (G-08-4, D-05, D-23, T-08-112): the review reads the
// platforms the revision stored, so it lists the platforms change a roll back or a restore would make, and nothing when
// what runs already matches. The platforms it stored differ from the ones that run here, so the running ones are not what
// the review reads.
func TestRollbackAndRestoreReviewsShowThePlatformsTheRevisionStored(t *testing.T) {
	t.Parallel()
	amd := swarm.Platform{OS: "linux", Architecture: "amd64"}
	arm := swarm.Platform{OS: "linux", Architecture: "arm64"}

	review := func(t *testing.T, mode string, runs, stored []swarm.Platform) (stackops.Review, int) {
		t.Helper()
		rig := newDriftRig(t)
		for _, svc := range []string{"web", "api", "gone"} {
			rig.editLive(t, svc, setLivePlatforms(runs...))
		}
		target := rig.store.rev
		target.Platforms = map[string][]swarm.Platform{}
		for _, svc := range []string{"web", "api", "gone"} {
			target.Platforms[rig.st.Name+"_"+svc] = slices.Clone(stored)
		}
		in := rig.reviewInput(t, rig.st.Name, driftCompose, nil)
		in.Stack = rig.st
		in.Mode, in.Target, in.TargetPins, in.Copy = mode, &target, rig.store.pins, target.Copy
		lookups := len(rig.fake.DistributionRefs())
		rv, err := rig.deps.Review(testsupport.Context(t), in)
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		if len(rv.Problems) != 0 {
			t.Fatalf("problems = %+v", rv.Problems)
		}
		return rv, len(rig.fake.DistributionRefs()) - lookups
	}

	for _, mode := range []string{stackops.ReviewModeRollback, stackops.ReviewModeRestore} {
		t.Run(mode+" lists the platforms change it would make", func(t *testing.T) {
			t.Parallel()
			rv, lookups := review(t, mode, []swarm.Platform{amd, arm}, []swarm.Platform{arm})
			if got := platformsRows(t, rv); len(got) != 3 {
				t.Errorf("platforms rows = %v, want one for each of the three services: the revision stored arm64 alone", got)
			}
			if lookups != 0 {
				t.Errorf("the review asked a registry %d times", lookups)
			}
		})
		t.Run(mode+" reads empty when what runs already matches", func(t *testing.T) {
			t.Parallel()
			rv, _ := review(t, mode, []swarm.Platform{arm}, []swarm.Platform{arm})
			if len(rv.Services) != 0 {
				t.Errorf("services = %v, want nothing to change: what runs has the platforms the revision stored", changesOf(rv))
			}
		})
	}
}
