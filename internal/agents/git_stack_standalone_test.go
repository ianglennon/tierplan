// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration

package agents_test

import (
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// The git stack tracer of plan 09-33 (GIT-04, GIT-06, GIT-07): a stack made from a folder of a git repository lives its
// whole life on a real standalone engine behind a real agent. The repository is the in-process git fixture served over
// TLS on loopback, so nothing here reaches the network for git. The server and the engine are the real ones; the
// poller's own timer is not what this test waits on (the 09-18 tests drive it with a clock), so each check is the
// request the Check now button sends, which takes the same path to the same deploy.

// gitAppCompose is the folder's compose file: one service whose environment is the setting a commit changes.
func gitAppCompose(image, mode string) string {
	return "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" +
		"    environment:\n      MODE: " + mode + "\n"
}

// modeOf is the MODE setting of the stack's one container, read from the engine.
func (f *saFix) modeOf(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(sh(t, f.host, "docker exec "+name+"-app-1 printenv MODE"))
}

// TestGitStackStandaloneThroughAnAgent creates a git stack from a folder, deploys it, finds the next commit behind,
// pulls and redeploys it, and then lets auto-deploy deploy the commit after that by itself (GIT-04, GIT-06, GIT-07).
func TestGitStackStandaloneThroughAnAgent(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	image := f.registry + "/bp/app:1"
	f.pushBusybox(t, image, nil)
	h := newGitHarness(t, f.srv.Do, f.env.ID)

	const name, folder = "gsa", "stacks/app"
	first := h.repo.Commit(t, map[string]string{folder + "/compose.yaml": gitAppCompose(image, "one")}, "app: mode one")

	// 1. Create the stack from the folder, review it and deploy it. The container runs what the first commit says,
	// and the stack reads in sync with that commit deployed.
	made := h.createStack(name, folder, []string{"compose.yaml"}, nil)
	if made.Commit != first {
		t.Fatalf("the stack was made from %s, want the first commit %s", made.Commit, first)
	}
	h.deploy(made.StackID, h.review(made.StackID))
	if got := f.containers(t, name); len(got["app"]) != 1 || !strings.HasSuffix(got["app"][0], " running") {
		t.Fatalf("containers after the first deploy = %v, want app running", got)
	}
	if got := f.modeOf(t, name); got != "one" {
		t.Errorf("MODE after the first deploy = %q, want one", got)
	}
	if st := h.state(made.StackID); st.Status != "in_sync" || st.DeployedSha != first || st.HeadSha != first {
		t.Fatalf("git state after the first deploy = %+v, want in sync at %s", st, first)
	}
	revs := h.revisions(made.StackID)
	if len(revs) != 1 || revs[0].Commit != first || revs[0].SyncTrigger != "" || revs[0].Outcome != "good" || !revs[0].Deployed {
		t.Fatalf("revisions after the first deploy = %+v, want one good revision of %s that a person started", revs, first)
	}

	// 2. A commit elsewhere in the repository moves the head and leaves this stack in sync; a commit to the folder
	// puts it behind by one.
	h.repo.Commit(t, map[string]string{"stacks/other/compose.yaml": "services: {}\n"}, "elsewhere")
	if st := h.checkNow(made.StackID, "the check to see the other folder's commit", func(s gitState) bool { return s.HeadSha != first }); st.Status != "in_sync" || st.Behind != 0 {
		t.Fatalf("git state after a commit elsewhere = %+v, want in sync, behind 0", st)
	}
	second := h.repo.Commit(t, map[string]string{folder + "/compose.yaml": gitAppCompose(image, "two")}, "app: mode two")
	st := h.checkNow(made.StackID, "the check to find the folder's commit", func(s gitState) bool { return s.HeadSha == second })
	if st.Status != "behind" || st.Behind != 1 || st.DeployedSha != first {
		t.Fatalf("git state after a commit to the folder = %+v, want behind 1 with %s deployed", st, first)
	}
	if got := f.modeOf(t, name); got != "one" {
		t.Errorf("MODE after the check = %q, want one: a check deploys nothing", got)
	}

	// 3. Pull & redeploy runs the new commit.
	if pulled := h.pullReviewDeploy(made.StackID); pulled != second {
		t.Fatalf("the pull took %s, want %s", pulled, second)
	}
	if got := f.modeOf(t, name); got != "two" {
		t.Errorf("MODE after Pull & redeploy = %q, want two", got)
	}
	if st := h.state(made.StackID); st.Status != "in_sync" || st.DeployedSha != second || st.Behind != 0 {
		t.Fatalf("git state after Pull & redeploy = %+v, want in sync at %s", st, second)
	}
	if got := f.containers(t, name); len(got["app"]) != 1 {
		t.Errorf("containers after the redeploy = %v, want the one app container and no leftover", got)
	}

	// 4. With auto-deploy on, the next commit to the folder deploys by itself, as the system on behalf of the person
	// who turned it on, from a poll. Turning it on checks the repository, which finds nothing to deploy.
	h.setAutoDeploy(made.StackID, true)
	on := h.state(made.StackID)
	if !on.AutoDeploy || on.Paused || on.Owner == nil || on.Owner.ID == "" {
		t.Fatalf("git state after turning auto-deploy on = %+v, want it on with an owner", on)
	}
	third := h.repo.Commit(t, map[string]string{folder + "/compose.yaml": gitAppCompose(image, "three")}, "app: mode three")
	h.requestCheck(made.StackID)
	auto := h.awaitAutoRevision(made.StackID, third)
	if auto.SyncTrigger != "poll" || auto.Origin != "deploy" || !auto.Deployed {
		t.Errorf("the automatic revision = %+v, want a deploy from a poll that is the deployed one", auto)
	}
	if got := f.modeOf(t, name); got != "three" {
		t.Errorf("MODE after the automatic deploy = %q, want three", got)
	}
	rows := f.srv.AuditRows(audit.ActionGitAutoDeployed)
	if len(rows) != 1 {
		t.Fatalf("git.autodeployed audit rows = %+v, want one", rows)
	}
	if r := rows[0]; r.ActorID != "system:gitsync" || r.Details["owner"] != on.Owner.ID || r.Details["commit"] != third || r.Details["trigger"] != "poll" {
		t.Errorf("the automatic deploy was audited as %+v, want system:gitsync on behalf of %s for %s from a poll", r, on.Owner.ID, third)
	}
	if st := h.state(made.StackID); st.Status != "in_sync" || st.DeployedSha != third || !st.AutoDeploy || st.Paused {
		t.Errorf("git state after the automatic deploy = %+v, want in sync at %s with auto-deploy still on", st, third)
	}
	f.noDenials(t)
}
