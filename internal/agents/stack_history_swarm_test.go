// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// draftState is a stack's draft as the route answers it: whether it is a saved draft, its version and its files.
type draftState struct {
	IsDraft bool            `json:"isDraft"`
	Version int64           `json:"version"`
	Files   json.RawMessage `json:"files"`
	Vars    json.RawMessage `json:"vars"`
}

// readDraft reads the stack's draft through the API.
func readDraft(t *testing.T, f *swarmFix, stackID string) draftState {
	t.Helper()
	code, body := f.srv.Do(http.MethodGet, f.stackAPI()+"/"+stackID+"/draft", nil)
	var d draftState
	if code != http.StatusOK || json.Unmarshal(body, &d) != nil {
		t.Fatalf("GET the draft of stack %s = %d %s", stackID, code, body)
	}
	return d
}

// sharedStackHistoryRollback proves a roll back to an older revision on a real swarm with a real registry (STK-08,
// D-02, D-03, D-04): a stack is deployed at three image digests (three good revisions), the draft is edited
// without being deployed, and the roll back to revision #1 is reviewed and run. Every task then runs revision
// #1's digest, History lists revision #4 as a roll back of #1 and the deployed one, and the draft is exactly as
// it was: a roll back never touches it.
func sharedStackHistoryRollback(t *testing.T, f *swarmFix) {
	f.stackEnvReady(t)
	mgr := f.sw.Managers[0]
	addr := dockertest.StartSwarmRegistry(t, f.sw, dockertest.RegistryOptions{})
	const stack, svc, replicas = "hist1", "hist1_app", 2

	// Three tags of one image, each with a digest of its own.
	tags := []string{"a", "b", "c"}
	refs := make([]string, len(tags))
	digests := make([]string, len(tags))
	for i, tag := range tags {
		refs[i] = addr + "/hist/app:" + tag
		digests[i] = dockertest.PushVariant(t, mgr, dockertest.ServiceTestImage, refs[i], tag, nil)
		for j := range i {
			if digests[j] == digests[i] {
				t.Fatalf("tags %s and %s have the same digest %s", tags[j], tags[i], digests[i])
			}
		}
	}
	composeOf := func(image, extra string) string {
		return "services:\n  app:\n    image: " + image + "\n    command: [\"sleep\", \"3600\"]\n    stop_grace_period: 1s\n" + extra + "    deploy:\n      replicas: 2\n"
	}
	runsImage := func(ref, digest string) {
		t.Helper()
		if got := serviceField(t, f, svc, "{{.Spec.TaskTemplate.ContainerSpec.Image}}"); got != ref+"@"+digest {
			t.Errorf("the service runs the image %q, want %s@%s", got, ref, digest)
		}
		f.waitTasksOn(t, svc, digest, replicas, taskWait)
	}

	id := f.createStackTierPlan(t, stack, composeOf(refs[0], ""))
	// Cleanups run last in, first out: TierPlan removes the stack; the docker CLI only sweeps up what a failure left.
	t.Cleanup(func() { removeStackCLI(t, f, stack) })
	t.Cleanup(func() { removeStackTierPlan(t, f, id, stack, engineObjects{}) })

	// Three deploys, three digests: revisions #1, #2 and #3, each good.
	for i := range refs {
		if i > 0 {
			f.saveStackTierPlan(t, id, paritySource{Compose: composeOf(refs[i], "")})
		}
		f.deployStackTierPlan(t, id)
		runsImage(refs[i], digests[i])
	}

	// An edit nobody deployed: the draft has changes the roll back must leave alone.
	f.saveStackTierPlan(t, id, paritySource{Compose: composeOf(refs[2], "    environment:\n      NOTE: edited after the third deploy\n")})
	draftBefore := readDraft(t, f, id)
	if !draftBefore.IsDraft {
		t.Fatalf("the draft after an edit = %+v, want a saved draft", draftBefore)
	}

	// The review of the roll back to #1: every image is pinned to the digest revision #1 stored.
	code, body := f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/review", map[string]any{"mode": "rollback", "revision": 1})
	var rv struct {
		Revision    int64 `json:"revision"`
		DeployedSeq int64 `json:"deployedSeq"`
		Images      []struct {
			Service, Digest, Current, State string
		} `json:"images"`
		Problems []struct {
			Severity, Code, Message string
		} `json:"problems"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &rv) != nil {
		t.Fatalf("POST the review of the roll back to #1 = %d %s", code, body)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			t.Errorf("the roll back review found an error: %s (%s)", p.Message, p.Code)
		}
	}
	if rv.Revision != 1 || rv.DeployedSeq != 3 {
		t.Errorf("the roll back review is of revision %d against deployed %d, want 1 against 3", rv.Revision, rv.DeployedSeq)
	}
	if len(rv.Images) != 1 {
		t.Fatalf("the roll back review lists %d images, want the one service's: %+v", len(rv.Images), rv.Images)
	}
	if r := rv.Images[0]; r.Service != "app" || r.State != "pinned" || r.Digest != digests[0] || r.Current != digests[2] {
		t.Errorf("the roll back review's image row = %+v, want app pinned to %s (from %s)", r, digests[0], digests[2])
	}

	// The roll back, as the review sends it.
	code, body = f.srv.Do(http.MethodPost, f.stackAPI()+"/"+id+"/rollback", map[string]any{"revision": 1, "expectedDeployed": rv.DeployedSeq})
	var started struct {
		JobID    string `json:"jobId"`
		Revision int64  `json:"revision"`
	}
	if code != http.StatusAccepted || json.Unmarshal(body, &started) != nil || started.JobID == "" {
		t.Fatalf("POST the roll back to #1 = %d %s", code, body)
	}
	if started.Revision != 4 {
		t.Errorf("the roll back took revision %d, want 4", started.Revision)
	}
	waitJob(t, f, id, started.JobID, "roll back")

	// Every task runs revision #1's digest, not what the tag a points to now or any later one.
	runsImage(refs[0], digests[0])

	// History: #4 is a roll back of #1, good, and the one the stack stands at.
	code, body = f.srv.Do(http.MethodGet, f.stackAPI()+"/"+id+"/revisions", nil)
	var hist struct {
		Items []struct {
			Seq          int64  `json:"seq"`
			Origin       string `json:"origin"`
			RestoredFrom int64  `json:"restoredFrom"`
			Outcome      string `json:"outcome"`
			Deployed     bool   `json:"deployed"`
		} `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &hist) != nil {
		t.Fatalf("GET the revisions = %d %s", code, body)
	}
	if len(hist.Items) != 4 || hist.Items[0].Seq != 4 {
		t.Fatalf("History lists %+v, want revisions 4, 3, 2 and 1 newest first", hist.Items)
	}
	if r := hist.Items[0]; r.Origin != "rollback" || r.RestoredFrom != 1 || r.Outcome != "good" || !r.Deployed {
		t.Errorf("revision #4 = %+v, want a good roll back of #1, deployed", r)
	}
	for _, r := range hist.Items[1:] {
		if r.Deployed || r.Outcome != "good" {
			t.Errorf("revision #%d = %+v, want good and no longer the deployed one", r.Seq, r)
		}
	}

	// The draft is as it was: same version, same files, same variables.
	draftAfter := readDraft(t, f, id)
	if draftAfter.IsDraft != draftBefore.IsDraft || draftAfter.Version != draftBefore.Version ||
		string(draftAfter.Files) != string(draftBefore.Files) || string(draftAfter.Vars) != string(draftBefore.Vars) {
		t.Errorf("the roll back changed the draft:\n before: %+v\n after:  %+v", draftBefore, draftAfter)
	}
}
