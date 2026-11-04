//go:build integration

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package agents_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestStandaloneStackForcedUpdate retries a version that failed, on purpose, for a service of a managed stack on a standalone
// host (plan 12-35 for the gap 12-45 left: no test forced a standalone stack's service, IMG-07, D-14). The stack runs one
// service from the host's registry, which the server asks itself. Two failures are retried. A build whose command exits at once is
// rolled back, refused without force, and tried again by force, which is rolled back again. Then the node cannot reach its
// registry while the server can (the node's connections to port 5000 are refused): the image cannot be pulled, nothing is
// changed, the badge reads rolled back with that reason, and once the node reaches the registry again force updates the service
// to the same digest and the failure is forgotten.
func TestStandaloneStackForcedUpdate(t *testing.T) {
	f := startSAFix(t, dockertest.RegistryOptions{})
	dockertest.ForwardRegistry(t, f.host)
	addFixtureCredential(t, f.srv, f.registry, "tp")
	image := f.registry + "/tp/force:1"
	const stack, container = "forcestk", "forcestk-app-1"
	// push puts a build under the tag and removes the host's own copy, so the deploy has to pull it from the registry.
	push := func(marker, cmd string) string {
		t.Helper()
		digest := dockertest.PushVariantCmd(t, f.host, dockertest.ServiceTestImage, image, marker, cmd, nil)
		sh(t, f.host, "docker image rm -f "+image)
		return digest
	}
	digestA := push("A", `["sleep","3600"]`)
	compose := "services:\n  app:\n    image: " + image + "\n    stop_grace_period: 1s\n"
	id := f.create(t, stack, compose, nil)
	f.deployOK(t, id)
	idOf := func() string { return strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.Id}}' "+container)) }
	imageOf := func() string {
		return strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.Config.Image}}' "+container))
	}
	if got := imageOf(); !strings.HasSuffix(got, "@"+digestA) {
		t.Fatalf("the service runs %q, want %s pinned to %s", got, image, digestA)
	}
	idA := idOf()
	base := "/api/env/" + f.env.ID

	check := func() {
		t.Helper()
		var code int
		var body []byte
		// The server lets one person check one environment once in ten seconds (429 too_many_requests): a check made soon
		// after the last waits for its turn.
		f.srv.WaitFor("the server to accept a check", time.Minute, func() (bool, string) {
			code, body = f.srv.Do(http.MethodPost, base+"/updates/check", nil)
			return code != http.StatusTooManyRequests, string(body)
		})
		if code != http.StatusOK {
			t.Fatalf("POST updates/check = %d %s", code, body)
		}
	}
	state := func() updateView {
		u, _ := containerUpdate(t, f.srv, f.env.ID, container)
		return u
	}
	waitState := func(want string) updateView {
		t.Helper()
		var u updateView
		f.srv.WaitFor("the update state of "+container+" to read "+want, 2*time.Minute, func() (bool, string) {
			u = state()
			b, _ := json.Marshal(u)
			return u.State == want, string(b)
		})
		return u
	}
	// run starts the update of the service as the confirm sends it and returns the status and body.
	run := func(digest string, force bool) (int, []byte) {
		body := map[string]any{"kind": "container", "name": container, "expect": map[string]string{"app": digest}}
		if force {
			body["force"] = true
		}
		return f.srv.Do(http.MethodPost, base+"/updates/run", body)
	}
	// finish waits for the deploy job an accepted update started.
	finish := func(code int, body []byte) saJob {
		t.Helper()
		var started struct {
			JobID   string `json:"jobId"`
			StackID string `json:"stackId"`
		}
		if code != http.StatusOK || json.Unmarshal(body, &started) != nil || started.JobID == "" || started.StackID != id {
			t.Fatalf("POST updates/run = %d %s, want 200 with a job of stack %s", code, body, id)
		}
		var job saJob
		testsupport.Poll(t, standaloneDeployWait*2, "the update job "+started.JobID+" to end", func() (bool, string) {
			code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id+"/deploys/"+started.JobID, nil)
			if code != http.StatusOK {
				return false, string(body)
			}
			job = saJob{}
			if err := json.Unmarshal(body, &job); err != nil {
				return false, string(body)
			}
			return job.State != "running", string(body)
		})
		return job
	}
	// current is the container the service has now. A rollback of a failed update puts the service back by recreating its container
	// from the previous image, so the ID moves with it; a refusal and a pull that failed change nothing and keep it.
	current := idA
	refusedWithoutForce := func(digest, reason string) {
		t.Helper()
		code, body := run(digest, false)
		if code != http.StatusConflict || !strings.Contains(string(body), "previously_failed") || !strings.Contains(string(body), reason) {
			t.Fatalf("an update without force to the version that failed = %d %s, want 409 previously_failed with the reason %s", code, body, reason)
		}
		if idOf() != current {
			t.Fatalf("a refused update replaced the container")
		}
	}

	// 1. A build whose command exits at once is rolled back; force tries it again.
	digestC := push("C", "")
	check()
	waitState("update_available")
	job := finish(run(digestC, false))
	raw, _ := json.Marshal(job)
	t.Logf("the update to a build that exits at once ended %s: %s", job.State, raw)
	if job.State == "done" {
		t.Fatalf("an update to a build that exits at once ended done: %s", raw)
	}
	u := waitState("rolled_back")
	if u.Failure == nil || u.Failure.Digest != digestC {
		t.Fatalf("the badge after the rollback = %+v, want a failure of %s", u, digestC)
	}
	reason := u.Failure.Reason
	t.Logf("the failure of the bad build reads %q", reason)
	if got := imageOf(); !strings.HasSuffix(got, "@"+digestA) || strings.TrimSpace(sh(t, f.host, "docker inspect -f '{{.State.Running}}' "+container)) != "true" {
		t.Errorf("after the rollback the service runs %q, want %s pinned to %s, running", got, image, digestA)
	}
	current = idOf()
	refusedWithoutForce(digestC, reason)
	failedBefore := len(f.srv.AuditRows("image.update_failed"))
	job = finish(run(digestC, true))
	if job.State == "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the forced update to a build that exits at once ended done: %s", raw)
	}
	waitState("rolled_back")
	current = idOf()
	if n := len(f.srv.AuditRows("image.update_failed")); n != failedBefore+1 {
		t.Errorf("image.update_failed rows = %d after the forced update, want %d: force tried the version again", n, failedBefore+1)
	}
	if got := imageOf(); !strings.HasSuffix(got, "@"+digestA) {
		t.Errorf("after the forced bad build the service runs %q, want %s pinned to %s", got, image, digestA)
	}
	if code, body := f.srv.Do(http.MethodGet, f.api()+"/"+id+"/revisions", nil); code != http.StatusOK {
		t.Fatalf("GET revisions = %d %s", code, body)
	}

	// 2. The tag moves to a good build, but the node cannot reach its registry: the pull fails and nothing is changed. The server
	// reaches the registry through the host's published address, so only the node's own connections (the engine pulling from
	// localhost:5000, which leave the host through its bridge) are refused.
	digestD := push("D", `["sleep","3600"]`)
	check()
	waitState("update_available")
	sh(t, f.host, "iptables -I OUTPUT -o docker0 -p tcp --dport 5000 -j REJECT --reject-with tcp-reset")
	unblocked := false
	unblock := func() {
		if !unblocked {
			unblocked = true
			sh(t, f.host, "iptables -D OUTPUT -o docker0 -p tcp --dport 5000 -j REJECT --reject-with tcp-reset")
		}
	}
	t.Cleanup(unblock)
	job = finish(run(digestD, false))
	raw, _ = json.Marshal(job)
	t.Logf("the update with the node cut off from its registry ended %s: %s", job.State, raw)
	if job.State == "done" {
		t.Fatalf("an update whose image could not be pulled ended done: %s", raw)
	}
	u = waitState("rolled_back")
	if u.Failure == nil || u.Failure.Digest != digestD || u.Failure.Reason != "pull_failed" {
		b, _ := json.Marshal(u)
		t.Fatalf("the badge after the pull failed = %s, want rolled_back with a pull_failed failure of %s", b, digestD)
	}
	if idOf() != current {
		t.Errorf("a pull that failed replaced the container")
	}
	refusedWithoutForce(digestD, "pull_failed")

	// The node reaches the registry again: force tries the same digest again, and it works.
	unblock()
	job = finish(run(digestD, true))
	if job.State != "done" {
		raw, _ := json.Marshal(job)
		t.Fatalf("the forced update ended %s, want done: %s", job.State, raw)
	}
	if got := imageOf(); !strings.HasSuffix(got, "@"+digestD) || idOf() == current {
		t.Errorf("after the forced update the service runs %q (container %s), want %s pinned to %s in a new container", got, idOf(), image, digestD)
	}
	if u := waitState("up_to_date"); u.Failure != nil {
		t.Errorf("the failure after the forced update worked = %+v, want it forgotten", u.Failure)
	}
	f.noDenials(t)
}
