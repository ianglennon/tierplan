// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build integration || swarm

package agents_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/gitx/gittest"
	"github.com/ianglennon/bayplan/proto/testsupport"
)

// The part of the git stack suites (plan 09-33) that the standalone and the swarm test share: a real git server
// on loopback, which the server's default egress policy allows as it allows the loopback registry, the repository
// saved through the API with the server's certificate as its CA, and the routes of a git stack that both tests walk.
// Nothing here reaches the network outside the test process.

// gitDeployWait is how long a deploy job, a pull or a check gets to end on a real engine.
const gitDeployWait = 5 * time.Minute

// gitHarness is a git repository served over TLS and saved in one environment of a running Bayplan.
type gitHarness struct {
	t     *testing.T
	do    func(method, path string, body any) (int, []byte)
	envID string

	repo   *gittest.Repo
	server *gittest.HTTPServer
	// repoID is the saved repository's ID in Bayplan.
	repoID string
}

// newGitHarness makes the repository and its server and saves the repository in the environment through the API.
func newGitHarness(t *testing.T, do func(method, path string, body any) (int, []byte), envID string) *gitHarness {
	t.Helper()
	h := &gitHarness{t: t, do: do, envID: envID, repo: gittest.NewRepo(t)}
	// A first commit gives the repository a main branch for the connection test to find.
	h.repo.Commit(t, map[string]string{"README.md": "# stacks\n"}, "readme")
	h.server = gittest.NewHTTPServer(t, h.repo, gittest.HTTPOptions{TLS: true})
	code, body := do(http.MethodPost, "/api/env/"+envID+"/git/repos", map[string]any{
		"name": "acme", "url": h.server.URL, "caPem": h.server.CAPEM,
	})
	var made struct {
		ID string `json:"id"`
	}
	if code != http.StatusCreated || json.Unmarshal(body, &made) != nil || made.ID == "" {
		t.Fatalf("POST the repository = %d %s", code, body)
	}
	h.repoID = made.ID
	return h
}

func (h *gitHarness) stacksAPI() string { return "/api/env/" + h.envID + "/stacks" }

// gitCreated is the answer of the create-from-repository route.
type gitCreated struct {
	StackID      string `json:"stackId"`
	DraftVersion int64  `json:"draftVersion"`
	Commit       string `json:"commit"`
	Problems     []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	} `json:"problems"`
}

// createStack makes a git stack from a folder of the repository's main branch.
func (h *gitHarness) createStack(name, folder string, composeFiles []string, overlays []map[string]string) gitCreated {
	h.t.Helper()
	body := map[string]any{
		"name": name, "repoId": h.repoID, "refKind": "branch", "ref": "main", "folder": folder, "composeFiles": composeFiles,
	}
	if overlays != nil {
		body["overlays"] = overlays
	}
	code, raw := h.do(http.MethodPost, "/api/env/"+h.envID+"/git-stacks", body)
	var made gitCreated
	if code != http.StatusCreated || json.Unmarshal(raw, &made) != nil || made.StackID == "" {
		h.t.Fatalf("POST git-stacks (%s) = %d %s", name, code, raw)
	}
	for _, p := range made.Problems {
		if p.Severity == "error" {
			h.t.Fatalf("the new git stack %s has a blocking problem: %s", name, raw)
		}
	}
	return made
}

// gitState is the part of the git route's answer the tests read.
type gitState struct {
	DeployedSha  string `json:"deployedSha"`
	HeadSha      string `json:"headSha"`
	Behind       int    `json:"behind"`
	Status       string `json:"status"`
	ReasonCode   string `json:"reasonCode"`
	Checking     bool   `json:"checking"`
	AutoDeploy   bool   `json:"autoDeploy"`
	Paused       bool   `json:"autoDeployPaused"`
	PausedReason string `json:"autoDeployPausedReason"`
	Owner        *struct {
		ID string `json:"id"`
	} `json:"autoDeployOwner"`
}

func (h *gitHarness) state(stackID string) gitState {
	h.t.Helper()
	code, raw := h.do(http.MethodGet, h.stacksAPI()+"/"+stackID+"/git", nil)
	var s gitState
	if code != http.StatusOK || json.Unmarshal(raw, &s) != nil {
		h.t.Fatalf("GET the git state of stack %s = %d %s", stackID, code, raw)
	}
	return s
}

// requestCheck asks for a check, the same request the Check now button sends, and does not wait for it.
func (h *gitHarness) requestCheck(stackID string) {
	h.t.Helper()
	code, raw := h.do(http.MethodPost, h.stacksAPI()+"/"+stackID+"/git/check", nil)
	if code != http.StatusAccepted {
		h.t.Fatalf("POST the check of stack %s = %d %s, want 202", stackID, code, raw)
	}
}

// checkNow asks for a check, the same request the Check now button sends, and waits until the stack's state shows
// the result: ok says whether the state is the one the test waits for. The check runs in the server, so the wait
// reads the stack's state through the API, with a bound.
func (h *gitHarness) checkNow(stackID, what string, ok func(gitState) bool) gitState {
	h.t.Helper()
	h.requestCheck(stackID)
	var last gitState
	testsupport.Poll(h.t, gitDeployWait, what, func() (bool, string) { // testsupport: external, the check runs in the server and its answer is stored for the route to read
		code, raw := h.do(http.MethodGet, h.stacksAPI()+"/"+stackID+"/git", nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET git = %d %s", code, raw)
		}
		last = gitState{}
		if err := json.Unmarshal(raw, &last); err != nil {
			return false, string(raw)
		}
		return !last.Checking && ok(last), string(raw)
	})
	return last
}

// draftVersion is the version of the stack's draft now.
func (h *gitHarness) draftVersion(stackID string) int64 {
	h.t.Helper()
	code, raw := h.do(http.MethodGet, h.stacksAPI()+"/"+stackID+"/draft", nil)
	var d struct {
		Version int64 `json:"version"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &d) != nil {
		h.t.Fatalf("GET the draft of stack %s = %d %s", stackID, code, raw)
	}
	return d.Version
}

// pull refreshes the draft from the head of the stack's ref, deploying nothing, and returns the commit it took.
func (h *gitHarness) pull(stackID string) string {
	h.t.Helper()
	code, raw := h.do(http.MethodPost, h.stacksAPI()+"/"+stackID+"/git/pull", map[string]any{"draftVersion": h.draftVersion(stackID)})
	var pulled struct {
		Commit string `json:"commit"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &pulled) != nil || pulled.Commit == "" {
		h.t.Fatalf("POST the pull of stack %s = %d %s", stackID, code, raw)
	}
	return pulled.Commit
}

// review reviews the draft and returns the images it pinned, which the deploy then uses without a lookup.
func (h *gitHarness) review(stackID string) map[string]string {
	h.t.Helper()
	code, raw := h.do(http.MethodPost, h.stacksAPI()+"/"+stackID+"/review", map[string]any{})
	var rv struct {
		Pins     map[string]string `json:"pins"`
		Problems []struct {
			Severity, Code, Message string
		} `json:"problems"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &rv) != nil {
		h.t.Fatalf("POST the review of stack %s = %d %s", stackID, code, raw)
	}
	for _, p := range rv.Problems {
		if p.Severity == "error" {
			h.t.Fatalf("the review of stack %s has a blocking problem: %s", stackID, raw)
		}
	}
	return rv.Pins
}

// gitJob is a deploy job as the route answers it.
type gitJob struct {
	State string `json:"state"`
}

// awaitJob waits until the job ends and fails the test unless it is done.
func (h *gitHarness) awaitJob(stackID, jobID, what string) {
	h.t.Helper()
	var job gitJob
	var raw []byte
	testsupport.Poll(h.t, gitDeployWait, "the "+what+" job "+jobID+" to end", func() (bool, string) { // testsupport: external, the job runs on a real engine
		code, body := h.do(http.MethodGet, h.stacksAPI()+"/"+stackID+"/deploys/"+jobID, nil)
		if code != http.StatusOK {
			return false, fmt.Sprintf("GET the job = %d %s", code, body)
		}
		raw, job = body, gitJob{}
		if err := json.Unmarshal(body, &job); err != nil {
			return false, string(body)
		}
		return job.State != "running", string(body)
	})
	if job.State != "done" {
		h.t.Fatalf("the %s of stack %s ended %s, want done: %s", what, stackID, job.State, raw)
	}
}

// deploy deploys the stack's draft at its current version with the pins a review chose, and waits for the job.
func (h *gitHarness) deploy(stackID string, pins map[string]string) {
	h.t.Helper()
	body := map[string]any{"draftVersion": h.draftVersion(stackID)}
	if pins != nil {
		body["pins"] = pins
	}
	code, raw := h.do(http.MethodPost, h.stacksAPI()+"/"+stackID+"/deploys", body)
	var started struct {
		JobID string `json:"jobId"`
	}
	if code != http.StatusAccepted || json.Unmarshal(raw, &started) != nil || started.JobID == "" {
		h.t.Fatalf("POST the deploy of stack %s = %d %s", stackID, code, raw)
	}
	h.awaitJob(stackID, started.JobID, "deploy")
}

// pullReviewDeploy is what a person does on the stack's page once Pull & redeploy is pressed: pull, review, deploy.
// It returns the commit that was pulled.
func (h *gitHarness) pullReviewDeploy(stackID string) string {
	h.t.Helper()
	commit := h.pull(stackID)
	h.deploy(stackID, h.review(stackID))
	return commit
}

// gitRevision is one revision of the History tab, as far as these tests read it.
type gitRevision struct {
	Seq          int64  `json:"seq"`
	Origin       string `json:"origin"`
	RestoredFrom int64  `json:"restoredFrom"`
	Outcome      string `json:"outcome"`
	Deployed     bool   `json:"deployed"`
	Commit       string `json:"commit"`
	SyncTrigger  string `json:"syncTrigger"`
}

// revisions lists the stack's revisions, newest first.
func (h *gitHarness) revisions(stackID string) []gitRevision {
	h.t.Helper()
	code, raw := h.do(http.MethodGet, h.stacksAPI()+"/"+stackID+"/revisions", nil)
	var list struct {
		Items []gitRevision `json:"items"`
	}
	if code != http.StatusOK || json.Unmarshal(raw, &list) != nil {
		h.t.Fatalf("GET the revisions of stack %s = %d %s", stackID, code, raw)
	}
	return list.Items
}

// setAutoDeploy turns the stack's auto-deploy on or off as the signed-in user.
func (h *gitHarness) setAutoDeploy(stackID string, on bool) {
	h.t.Helper()
	code, raw := h.do(http.MethodPut, h.stacksAPI()+"/"+stackID+"/git/auto-deploy", map[string]any{"enabled": on})
	if code != http.StatusOK {
		h.t.Fatalf("PUT auto-deploy (%v) of stack %s = %d %s", on, stackID, code, raw)
	}
}

// awaitAutoRevision waits until the stack has a good revision made from commit by an automatic deploy and returns it.
func (h *gitHarness) awaitAutoRevision(stackID, commit string) gitRevision {
	h.t.Helper()
	var found gitRevision
	testsupport.Poll(h.t, gitDeployWait, "an automatic deploy of "+commit+" to finish", func() (bool, string) { // testsupport: external, the server checks the repository and deploys on its own
		revs := h.revisions(stackID)
		for _, r := range revs {
			if r.Commit == commit && r.SyncTrigger != "" {
				found = r
				return r.Outcome != "pending", fmt.Sprintf("%+v", r)
			}
		}
		return false, fmt.Sprintf("no revision of %s yet: %+v", commit, revs)
	})
	if found.Outcome != "good" {
		h.t.Fatalf("the automatic deploy of %s ended %+v, want a good revision", commit, found)
	}
	return found
}
