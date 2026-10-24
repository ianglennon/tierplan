// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/stackops"
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

func TestRollbackReviewOfAStandaloneStackIsRefusedUntilItsEngineExists(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, standalone)
	in := rig.rollbackReviewInput(t, "plain", poolsCompose, map[string]string{"plain_web": "nginx:1.27-alpine@sha256:" + strings.Repeat("a", 64)})
	in.Stack.Target = stacks.TargetStandalone
	if _, err := rig.deps.Review(testsupport.Context(t), in); err == nil || !strings.Contains(err.Error(), "cannot deploy") {
		t.Fatalf("err = %v, want ErrTargetEngineMissing", err)
	}
}
