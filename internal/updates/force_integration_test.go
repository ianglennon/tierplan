//go:build integration

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates_test

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/updates"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// TestForcedUpdatePlainContainerRealEngine retries a version that failed, on purpose, for a plain container on a real engine
// (plan 12-35 for the gap 12-45 left: no test forced a container, IMG-07, D-14). Two failures are retried. First a registry that
// is down when the update pulls: the image could not be pulled, nothing was changed, and the badge reads rolled back; without
// force the version is refused with that reason, and once the registry is back force updates the container to it and the failure
// is forgotten. Then a build whose command exits at once: it is rolled back, refused without force, and force really tries it
// again, which ends rolled back again with the old container running as it was.
func TestForcedUpdatePlainContainerRealEngine(t *testing.T) {
	node := dockertest.StartHost(t, "")
	registry := dockertest.StartRegistry(t, node, dockertest.RegistryOptions{})
	image := registry + "/tp/force:1"
	digestA := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "A", `["sleep","3600"]`, nil)

	ctx := testsupport.Context(t)
	docker := func(args ...string) string {
		t.Helper()
		out, err := node.Exec(ctx, append([]string{"docker"}, args...)...)
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(out)
	}
	docker("pull", image+"@"+digestA)
	docker("run", "-d", "--name", "app", "--stop-timeout", "1", image)
	containerID := func() string { return docker("inspect", "-f", "{{.Id}}", "app") }
	runningImage := func() string { return docker("inspect", "-f", "{{.Config.Image}}", "app") }
	idA := containerID()

	r := newApplyRigOn(t, "unix://"+dockertest.UnixProxy(t, node.Host()), nil)
	r.applier.Clock = stackops.RealClock()
	r.applier.ConvergeTimeout = time.Minute
	r.applier.Service.ImageTexts = &updates.ImageTextCache{Inspect: updates.InspectImageTexts(r.applier.Aggregator)}
	var ticks atomic.Int64
	r.applier.Service.Checker.Now = func() time.Time { return t0.Add(time.Duration(ticks.Add(1)) * time.Minute) }
	// start offers the digest as the registry's now and starts the update. force is the person's choice to try again.
	start := func(digest string, force bool) (updates.Run, jobs.Job, error) {
		t.Helper()
		r.heads.set(image, answer{digest: digest})
		if _, _, err := r.applier.Aggregator.FreshSnapshot(ctx, r.envID); err != nil {
			t.Fatalf("read the node: %v", err)
		}
		req := r.containerRequest("app", digest)
		req.Force = force
		id, err := r.applier.Start(ctx, req)
		if err != nil {
			return updates.Run{}, jobs.Job{}, err
		}
		job, err := r.hub.Wait(ctx, id)
		if err != nil {
			t.Fatalf("waiting for the job: %v", err)
		}
		return r.runRow(t, job), job, nil
	}
	refused := func(err error, reason string) {
		t.Helper()
		var prev *updates.PreviouslyFailedError
		if !errors.Is(err, updates.ErrPreviouslyFailed) || !errors.As(err, &prev) || prev.Failure == nil || prev.Failure.Reason != reason {
			t.Fatalf("an update without force to the version that failed = %v, want ErrPreviouslyFailed with the reason %s", err, reason)
		}
	}
	verdict := func() updates.Verdict {
		t.Helper()
		if _, _, err := r.applier.Aggregator.FreshSnapshot(ctx, r.envID); err != nil {
			t.Fatalf("read the node: %v", err)
		}
		sv, found, err := r.applier.Service.Find(ctx, r.envID, "container", "app")
		if err != nil || !found {
			t.Fatalf("Find(app) = %v found %v", err, found)
		}
		return sv.Verdict
	}

	// 1. The registry is down when the update pulls.
	digestB := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "B", `["sleep","3600"]`, nil)
	docker("stop", "tp-registry")
	run, job, err := start(digestB, false)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.State == jobs.JobDone || run.Reason != updates.ReasonPullFailed {
		t.Fatalf("the update with the registry down: job %s run %s %q, want a pull_failed that did not finish", job.State, run.State, run.Reason)
	}
	if containerID() != idA || runningImage() != image {
		t.Errorf("the container is %s on %q after a pull that failed, want %s on %q: nothing was changed", containerID(), runningImage(), idA, image)
	}
	if f, ok := r.containerFailureOf(t, "app"); !ok || f.Reason != updates.ReasonPullFailed || f.Digest != digestB {
		t.Errorf("failure = %+v %v, want the pull of %s remembered", f, ok, digestB)
	}
	if v := verdict(); v.State != updates.StateRolledBack {
		t.Errorf("verdict = %s, want rolled_back for a version that could not be pulled", v.State)
	}
	_, _, err = start(digestB, false)
	refused(err, updates.ReasonPullFailed)

	// The registry is back: force tries the version again, and it works.
	docker("start", "tp-registry")
	dockertest.Wait(t, 90*time.Second, "the registry to answer /v2/ again", func() (bool, string) {
		out, err := node.Sh(ctx, "wget -S -O /dev/null -T 3 http://127.0.0.1:5000/v2/ 2>&1 | grep -q '^ *HTTP/'")
		return err == nil, out
	})
	run, job, err = start(digestB, true)
	if err != nil {
		t.Fatalf("Start with force: %v", err)
	}
	if job.State != jobs.JobDone || run.State != updates.RunDone {
		t.Fatalf("the forced update: job %s run %s %q, want done", job.State, run.State, run.Reason)
	}
	if containerID() == idA || runningImage() != image+"@"+digestB {
		t.Errorf("after the forced update the container is %s on %q, want a new container on %s@%s", containerID(), runningImage(), image, digestB)
	}
	if f, ok := r.containerFailureOf(t, "app"); ok {
		t.Errorf("failure = %+v, want it forgotten after the forced update worked", f)
	}
	if v := verdict(); v.State != updates.StateUpToDate {
		t.Errorf("verdict after the forced update = %s, want up_to_date", v.State)
	}
	idB := containerID()

	// 2. A build whose command exits at once is rolled back; force tries it again and it is rolled back again.
	digestC := dockertest.PushVariantCmd(t, node, dockertest.ServiceTestImage, image, "C", "", nil)
	run, _, err = start(digestC, false)
	if err != nil || run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart {
		t.Fatalf("the bad build: err %v run %s %q, want rolled_back didnt_start", err, run.State, run.Reason)
	}
	_, _, err = start(digestC, false)
	refused(err, updates.ReasonDidntStart)
	run, job, err = start(digestC, true)
	if err != nil {
		t.Fatalf("Start with force: %v", err)
	}
	if run.State != updates.RunRolledBack || run.Reason != updates.ReasonDidntStart || job.State == jobs.JobDone {
		t.Fatalf("the forced bad build: job %s run %s %q, want it tried again and rolled back again", job.State, run.State, run.Reason)
	}
	if containerID() != idB || runningImage() != image+"@"+digestB {
		t.Errorf("after the forced bad build the container is %s on %q, want %s on %s@%s as before", containerID(), runningImage(), idB, image, digestB)
	}
	if f, ok := r.containerFailureOf(t, "app"); !ok || f.Reason != updates.ReasonDidntStart || f.Digest != digestC {
		t.Errorf("failure = %+v %v, want the bad build remembered again", f, ok)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Errorf("the lock is held after the forced update")
	}
}
