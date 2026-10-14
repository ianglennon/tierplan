// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
)

// How far a rolling update has got (plan 06-09, RES-02, D-03, RESEARCH E9 and E10).

type rolloutBody struct {
	State           string `json:"state"`
	Message         string `json:"message"`
	StartedAt       string `json:"startedAt"`
	CompletedAt     string `json:"completedAt"`
	Updated         int    `json:"updated"`
	Total           int    `json:"total"`
	LatestTaskError string `json:"latestTaskError"`
}

const (
	rolloutOld = "localhost:5000/private/web:1"
	rolloutNew = "localhost:5000/private/web:2"
)

// rolloutService is a replicated service whose template names rolloutNew.
func rolloutService(state, message string) dockertest.FakeService {
	s := webService()
	s.Image = rolloutNew
	s.UpdateState = state
	s.UpdateMessage = message
	return s
}

// rolloutTask is a task of svc-web on image, running or whatever state says.
func rolloutTask(id string, slot int, image, state, desired string, age time.Duration) swarm.Task {
	t := tkTask(id, "svc-web", "node-1", slot, state, desired, age)
	t.Spec = swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: image}, Runtime: swarm.RuntimeContainer}
	return t
}

func rolloutGet(t *testing.T, f *envFix, e envBody) rolloutBody {
	t.Helper()
	rec := f.get(t, e, "services/svc-web/rollout")
	if rec.code != http.StatusOK {
		t.Fatalf("GET rollout: %d %s", rec.code, rec.body)
	}
	return decode[rolloutBody](t, rec)
}

func TestRolloutCountsTasksOnTheNewSpec(t *testing.T) {
	f, e := tkEnv(t, 5, []dockertest.FakeService{rolloutService("updating", "update in progress")},
		rolloutTask("a-new", 1, rolloutNew, "running", "running", time.Minute),
		rolloutTask("b-old", 2, rolloutOld, "running", "running", time.Hour),
		rolloutTask("c-starting", 3, rolloutNew, "starting", "running", time.Second),
		rolloutTask("a-gone", 1, rolloutOld, "shutdown", "shutdown", time.Hour),
	)
	got := rolloutGet(t, f, e)
	if got.State != "updating" || got.Message != "update in progress" {
		t.Errorf("state = %q (%q), want updating", got.State, got.Message)
	}
	// Three tasks are desired running; only a-new is running on the new spec.
	if got.Updated != 1 || got.Total != 3 {
		t.Errorf("updated %d of %d, want 1 of 3", got.Updated, got.Total)
	}
}

func TestRolloutAfterAScaleIsNone(t *testing.T) {
	f, e := tkEnv(t, 5, []dockertest.FakeService{rolloutService("", "")},
		rolloutTask("a", 1, rolloutNew, "running", "running", time.Minute),
		rolloutTask("b", 2, rolloutNew, "running", "running", time.Minute),
	)
	got := rolloutGet(t, f, e)
	if got.State != "none" {
		t.Errorf("state = %q, want none (a scale leaves no UpdateStatus)", got.State)
	}
	if got.Updated != 2 || got.Total != 2 {
		t.Errorf("updated %d of %d, want 2 of 2", got.Updated, got.Total)
	}
}

func TestRolloutLatestTaskError(t *testing.T) {
	failed := rolloutTask("f-new", 1, rolloutNew, "failed", "shutdown", time.Minute)
	failed.Status.Err = "task: non-zero exit (1)\x00\nsecond line"
	rejected := rolloutTask("r-old", 2, rolloutNew, "rejected", "shutdown", time.Hour)
	rejected.Status.Err = "no suitable node"
	f, e := tkEnv(t, 5, []dockertest.FakeService{rolloutService("paused", "update paused due to failure")},
		rolloutTask("ok", 3, rolloutNew, "running", "running", time.Minute),
		failed, rejected,
	)
	got := rolloutGet(t, f, e)
	if got.State != "paused" {
		t.Errorf("state = %q, want paused", got.State)
	}
	if got.LatestTaskError != "task: non-zero exit (1) second line" {
		t.Errorf("latestTaskError = %q, want the newest failure, cleaned", got.LatestTaskError)
	}
}

func TestRolloutOfAnUnknownServiceIsNotFound(t *testing.T) {
	f, e := tkEnv(t, 5, []dockertest.FakeService{rolloutService("", "")})
	rec := f.get(t, e, "services/nope/rollout")
	if rec.code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.code, rec.body)
	}
}
