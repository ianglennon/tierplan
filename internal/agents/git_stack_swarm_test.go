// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The git stack suite on a real swarm (plan 09-33, GIT-04, GIT-06, GIT-07): a stack of two compose files from a folder of a
// git repository, with a secret file the repository does not hold supplied as a sealed overlay, deployed through a real
// agent. The repository is the in-process git fixture served over TLS on loopback.

const (
	gitSwarmBase = "services:\n  app:\n    image: %IMAGE%\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" +
		"    secrets: [tok]\n    deploy:\n      replicas: 1\nsecrets:\n  tok:\n    file: ./tok.txt\n"
	// gitSwarmProd is the second file: its keys win over the first's, so the replicas are the number it names.
	gitSwarmProd = "services:\n  app:\n    environment:\n      MODE: %MODE%\n    deploy:\n      replicas: %REPLICAS%\n"
	gitSwarmTok  = "overlay-secret-0123456789"
)

// gitSwarmFile fills the marks of one of the folder's files.
func gitSwarmFile(text string, with map[string]string) string {
	for k, v := range with {
		text = strings.ReplaceAll(text, "%"+k+"%", v)
	}
	return text
}

// TestGitStackOnSwarm deploys a git stack of two compose files and an overlay secret to a swarm, updates it from a new
// commit, turns auto-deploy on, rolls back to the first revision and shows that the roll back pauses auto-deploy for good:
// a later commit to the folder is found but not deployed.
func TestGitStackOnSwarm(t *testing.T) {
	f := startSwarmFix(t, 1, 1)
	f.install(t)
	f.waitSwarmOnline(t, 2)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	image := addr + "/gitstack/app:a"
	digest := dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, image, "a", nil)
	h := newGitHarness(t, f.srv.Do, f.env.ID)

	const stack, svc, folder = "gsw", "gsw_app", "stacks/app"
	files := func(mode, replicas string) map[string]string {
		return map[string]string{
			folder + "/compose.yaml":      gitSwarmFile(gitSwarmBase, map[string]string{"IMAGE": image}),
			folder + "/compose.prod.yaml": gitSwarmFile(gitSwarmProd, map[string]string{"MODE": mode, "REPLICAS": replicas}),
		}
	}
	replicasOf := func() string {
		return serviceField(t, f, svc, "{{.Spec.Mode.Replicated.Replicas}}")
	}
	modeOf := func() string {
		return serviceField(t, f, svc, "{{range .Spec.TaskTemplate.ContainerSpec.Env}}{{println .}}{{end}}")
	}
	runs := func(replicas, mode string) {
		t.Helper()
		if got := replicasOf(); got != replicas {
			t.Errorf("the service has %s replicas, want %s", got, replicas)
		}
		if got := modeOf(); !strings.Contains(got, "MODE="+mode) {
			t.Errorf("the service's environment is %q, want MODE=%s", got, mode)
		}
	}

	first := h.repo.Commit(t, files("one", "2"), "app: two replicas")
	made := h.createStack(stack, folder, []string{"compose.yaml", "compose.prod.yaml"}, []map[string]string{{"path": "tok.txt", "content": gitSwarmTok}})
	if made.Commit != first {
		t.Fatalf("the stack was made from %s, want %s", made.Commit, first)
	}
	// Cleanups run last in, first out: TierPlan removes the stack; the docker CLI only sweeps up what a failure left.
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	t.Cleanup(func() { removeStackTierPlan(t, f, made.StackID, stack, engineObjects{}) })

	// 1. Revision #1: the second file's replicas win over the first's, and the overlay secret is a file in a task.
	h.deploy(made.StackID, h.review(made.StackID))
	f.waitTasksOn(t, svc, digest, 2, taskWait)
	runs("2", "one")
	if got := secretInContainer(t, f, svc, "tok"); got != gitSwarmTok {
		t.Errorf("/run/secrets/tok = %q, want the overlay's content", got)
	}
	if revs := h.revisions(made.StackID); len(revs) != 1 || revs[0].Commit != first || revs[0].Seq != 1 {
		t.Fatalf("revisions after the first deploy = %+v, want #1 from %s", revs, first)
	}

	// 2. A commit to the folder is found, pulled and deployed as revision #2.
	second := h.repo.Commit(t, files("two", "3"), "app: three replicas")
	if st := h.checkNow(made.StackID, "the check to find the folder's commit", func(s gitState) bool { return s.HeadSha == second }); st.Status != "behind" || st.Behind != 1 {
		t.Fatalf("git state after a commit to the folder = %+v, want behind 1", st)
	}
	if pulled := h.pullReviewDeploy(made.StackID); pulled != second {
		t.Fatalf("the pull took %s, want %s", pulled, second)
	}
	f.waitTasksOn(t, svc, digest, 3, taskWait)
	runs("3", "two")
	if got := secretInContainer(t, f, svc, "tok"); got != gitSwarmTok {
		t.Errorf("/run/secrets/tok after the pull = %q, want the overlay's content to survive it", got)
	}
	if st := h.state(made.StackID); st.Status != "in_sync" || st.DeployedSha != second {
		t.Fatalf("git state after revision #2 = %+v, want in sync at %s", st, second)
	}

	// 3. Auto-deploy on, then a roll back to #1: the services are #1's, and auto-deploy is off for the roll back.
	h.setAutoDeploy(made.StackID, true)
	if st := h.state(made.StackID); !st.AutoDeploy || st.Paused {
		t.Fatalf("git state with auto-deploy on = %+v", st)
	}
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+made.StackID+"/rollback", map[string]any{"revision": 1, "expectedDeployed": 2})
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the roll back to #1 = %d %s", code, body)
	}
	if started.Revision != 3 {
		t.Errorf("the roll back took revision %d, want 3", started.Revision)
	}
	h.awaitJob(made.StackID, started.JobID, "roll back")
	f.waitTasksOn(t, svc, digest, 2, taskWait)
	runs("2", "one")
	if got := secretInContainer(t, f, svc, "tok"); got != gitSwarmTok {
		t.Errorf("/run/secrets/tok after the roll back = %q, want the overlay's content", got)
	}
	rolled := h.state(made.StackID)
	if rolled.AutoDeploy || rolled.PausedReason != "rollback" {
		t.Fatalf("git state after the roll back = %+v, want auto-deploy off, paused for the roll back", rolled)
	}
	if rows := f.srv.AuditRows(audit.ActionGitAutoDeployPaused); len(rows) != 1 || rows[0].Details["reason"] != "rollback" {
		t.Errorf("git.autodeploy_paused audit rows = %+v, want one for the roll back", rows)
	}
	revs := h.revisions(made.StackID)
	if len(revs) != 3 || revs[0].Seq != 3 || revs[0].Origin != "rollback" || revs[0].RestoredFrom != 1 || revs[0].Outcome != "good" || !revs[0].Deployed {
		t.Fatalf("revisions after the roll back = %+v, want #3 a good roll back of #1 that is the deployed one", revs)
	}

	// 4. A further commit to the folder is found and left alone. Each check is a later event than the commit it is
	// about, and the second shows that the first left nothing to do.
	third := h.repo.Commit(t, files("three", "4"), "app: four replicas")
	for i := range 2 {
		st := h.checkNow(made.StackID, "the check to find the commit after the roll back", func(s gitState) bool { return s.HeadSha == third })
		if st.Status != "behind" || st.AutoDeploy {
			t.Fatalf("git state after check %d following the roll back = %+v, want behind with auto-deploy off", i+1, st)
		}
	}
	if after := h.revisions(made.StackID); len(after) != 3 {
		t.Errorf("revisions after a commit that auto-deploy must not deploy = %+v, want the same three", after)
	}
	runs("2", "one")
	if rows := f.srv.AuditRows(audit.ActionGitAutoDeployed); len(rows) != 0 {
		t.Errorf("git.autodeployed audit rows = %+v, want none: the stack was rolled back", rows)
	}
}
