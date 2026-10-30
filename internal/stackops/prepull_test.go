// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package stackops_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
	"github.com/ianglennon/coaming/internal/jobs"
	"github.com/ianglennon/coaming/internal/registry"
	"github.com/ianglennon/coaming/internal/stackops"
	"github.com/ianglennon/coaming/internal/store/storetest"
	"github.com/ianglennon/coaming/internal/vault"
	"github.com/ianglennon/coaming/proto/testsupport"
)

// The pre-pull of a standalone deploy (plan 07-21, D-27, IMG-02, STK-09): every pinned image the host does not
// have is pulled by Coaming, with the credential it stores, before the Compose SDK runs, so the SDK finds the
// images locally and never needs a credential. Each pull is a step of the job with its progress.

const (
	prePullSecret   = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	appImage        = "ghcr.io/acme/app:1"
	prePullServices = "services:\n  app:\n    image: " + appImage + "\n  worker:\n    image: " + appImage + "\n  web:\n    image: nginx:1.27-alpine\n"
)

// pulls are the pulls the fake engine served, in order.
func pulls(rig *swarmRig) []string {
	var out []string
	for _, w := range rig.fake.Writes() {
		if strings.HasPrefix(w, "POST /images/create") {
			out = append(out, w)
		}
	}
	return out
}

// withCredential stores a GHCR credential for acme/ in the rig's resolver and returns the store.
func withCredential(t *testing.T, rig *swarmRig) *registry.Store {
	t.Helper()
	v, err := vault.New(make([]byte, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	creds := registry.NewStore(storetest.New(t), v, time.Now)
	if _, err := creds.Create(context.Background(), registry.CreateInput{
		Name: "ghcr-acme", Type: registry.TypeGHCR, Prefix: "acme", Username: "bot", Secret: prePullSecret,
	}, "admin-1", nil); err != nil {
		t.Fatal(err)
	}
	rig.deps.Resolver = registry.NewResolver(creds)
	return creds
}

func TestPrePullPullsEachImageTheHostLacksOnceWithTheStoredCredentialAndShowsItsProgress(t *testing.T) {
	t.Parallel()
	rig := newStandaloneRig(t)
	withCredential(t, rig)
	rig.fake.SetDistributionDigest("ghcr.io/acme/app:1", digestOf("a"))
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", digestOf("b"))
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, job := rig.deployStandalone(t, standaloneInput("demo", prePullServices))

	if !res.OK() || stand.calls != 1 {
		t.Fatalf("result %+v after %d SDK calls, want a deploy that reached the SDK once", res, stand.calls)
	}
	// One pull for the image two services share, one for the other: two in all.
	if got := pulls(rig); len(got) != 2 {
		t.Fatalf("pulls = %v, want one for each image", got)
	}
	var withAuth, anonymous int
	for _, header := range rig.fake.PullAuth() {
		if header == "" {
			anonymous++
			continue
		}
		raw, err := base64.URLEncoding.DecodeString(header)
		var got struct{ Username, Password string }
		if err != nil || json.Unmarshal(raw, &got) != nil || got.Username != "bot" || got.Password != prePullSecret {
			t.Errorf("a pull was sent %q, want the ghcr credential", header)
		}
		withAuth++
	}
	if withAuth != 1 || anonymous != 1 {
		t.Errorf("pulls with a credential %d, anonymous %d, want one of each", withAuth, anonymous)
	}

	app := itemByID(t, job, "pull:ghcr.io/acme/app:1@"+digestOf("a"))
	if app.Label != "Pull ghcr.io/acme/app:1" || app.Parent != stackops.ItemLookup || app.Kind != stackops.KindStep || app.State != jobs.ItemDone {
		t.Errorf("the pull step = {label %q parent %q kind %q state %s}, want a done step 'Pull ghcr.io/acme/app:1' under the lookup", app.Label, app.Parent, app.Kind, app.State)
	}
	if app.Result["image"] != "ghcr.io/acme/app:1" || app.Result["ref"] != "ghcr.io/acme/app:1" {
		t.Errorf("the pull's result = %v, want the image and the normalised reference without a digest", app.Result)
	}
	if got := itemByID(t, job, "pull:nginx:1.27-alpine@"+digestOf("b")).Result["ref"]; got != "docker.io/library/nginx:1.27-alpine" {
		t.Errorf("the nginx pull's reference = %q, want docker.io/library/nginx:1.27-alpine", got)
	}
	if app.Unit != "bytes" || app.Total != 2048 || app.Current != 2048 || app.Count != 2 || app.Done != 2 {
		t.Errorf("the pull's progress = %d of %d %s, %d of %d layers, want 2048 of 2048 bytes in 2 layers", app.Current, app.Total, app.Unit, app.Done, app.Count)
	}
	if got := itemByID(t, job, "pull:nginx:1.27-alpine@"+digestOf("b")); got.Label != "Pull nginx:1.27-alpine" || got.State != jobs.ItemDone {
		t.Errorf("the other pull step = %+v", got)
	}
	// The credential is in no item and no line of the job.
	raw, _ := json.Marshal(job)
	if out, _ := rig.hub.Output(job.ID); strings.Contains(string(raw), prePullSecret) || strings.Contains(outputText(out), prePullSecret) {
		t.Error("the job holds the secret of the credential")
	}
	for _, header := range rig.fake.PullAuth() {
		if header != "" && strings.Contains(string(raw), header) {
			t.Error("the job holds the registry header")
		}
	}
}

func outputText(lines []jobs.Output) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text + "\n")
	}
	return b.String()
}

func TestPrePullSkipsAnImageTheHostAlreadyHasAtThePinnedDigest(t *testing.T) {
	t.Parallel()
	pin := "nginx:1.27-alpine@" + digestOf("b")
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.Images = []dockertest.FakeImage{{ID: digestOf("c"), RepoTags: []string{pin}}}
	})
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", digestOf("b"))
	rig.deps.Up = (&upStandIn{}).up

	res, job := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if !res.OK() {
		t.Fatalf("result = %+v", res)
	}
	if got := pulls(rig); len(got) != 0 {
		t.Errorf("pulls = %v, want none: the host has the image", got)
	}
	for _, it := range job.Items {
		if strings.HasPrefix(it.ID, "pull:") {
			t.Errorf("the job has the step %s, want none for an image the host has", it.ID)
		}
	}
}

func TestPrePullIsNotDoneForAServiceWhoseImageTextDidNotChange(t *testing.T) {
	t.Parallel()
	// STK-09: a redeploy of an unchanged service neither pulls nor moves to a newer image, even though the
	// tag has moved and the host no longer has the image.
	pin := "nginx:1.27-alpine@" + digestOf("a")
	hash := preparedHash(t, "demo", webStandalone, map[string]string{"web": pin})["web"]
	rig := newStandaloneRig(t, deployedContainer("demo", "web", hash, "nginx:1.27-alpine", pin))
	rig.fake.SetDistributionDigest("docker.io/library/nginx:1.27-alpine", digestOf("b"))
	stand := &upStandIn{}
	rig.deps.Up = stand.up

	res, _ := rig.deployStandalone(t, standaloneInput("demo", webStandalone))

	if !res.OK() || stand.calls != 1 {
		t.Fatalf("result %+v after %d SDK calls", res, stand.calls)
	}
	if got := pulls(rig); len(got) != 0 {
		t.Errorf("pulls = %v, want none for a service whose text did not change", got)
	}
	if got := stand.project.Services["web"].Image; got != pin {
		t.Errorf("the SDK was given %q, want the running pin %q: the moved tag must not be followed", got, pin)
	}
}

func TestPrePullStopsTheDeployAtTheFailedPullWithTheRegistryWordsAndCreatesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg  dockertest.FakePullStatus
		code string
	}{
		"denied":       {dockertest.FakePullStatus{Code: 403, Message: "denied: secret-registry-body-A"}, "pull_denied"},
		"not found":    {dockertest.FakePullStatus{Code: 404, Message: "manifest unknown secret-registry-body-B"}, "image_not_found"},
		"rate limited": {dockertest.FakePullStatus{Code: 429, Message: "toomanyrequests: secret-registry-body-C"}, "rate_limited"},
		"certificate":  {dockertest.FakePullStatus{Code: 500, Message: "x509: certificate signed by unknown authority secret-registry-body-D"}, "registry_tls"},
		"agent":        {dockertest.FakePullStatus{Code: 403, Message: "agent_policy_denied: POST /v1.52/images/create"}, "agent_policy_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
				standalone(c)
				c.TasksFollowWrites = false
				c.PullStatus = tc.cfg
			})
			stand := &upStandIn{}
			rig.deps.Up = stand.up

			// The review chose the pin, so the deploy makes no lookup (the fake answers a lookup with the
			// refusal too).
			in := standaloneInput("demo", withNetworkAndVolume)
			in.Reviewed = map[string]string{"web": "nginx:1.27-alpine@" + digestOf("b")}
			res, job := rig.deployStandalone(t, in)

			if res.OK() || res.Code != tc.code || res.FailedService != "web" {
				t.Fatalf("result = %+v, want %s at web", res, tc.code)
			}
			if stand.calls != 0 {
				t.Errorf("the SDK was called %d times, want never: nothing may be created after a failed pull", stand.calls)
			}
			pull := job.Items[len(job.Items)-1]
			for _, it := range job.Items {
				if strings.HasPrefix(it.ID, "pull:") {
					pull = it
				}
			}
			if !strings.HasPrefix(pull.ID, "pull:") || pull.State != jobs.ItemFailed || pull.Code != tc.code || pull.Message != stackops.CodeMessage(tc.code) || pull.Message == "" {
				t.Errorf("pull step = %+v, want failed with %s and its sentence", pull, tc.code)
			}
			if raw, _ := json.Marshal(job); strings.Contains(string(raw), "secret-registry-body") {
				t.Errorf("the job carries the registry's answer: %s", raw)
			}
			// The lookup was done; every step after the pull did not run.
			if got := itemByID(t, job, "lookup"); got.State != jobs.ItemDone {
				t.Errorf("lookup = %s, want done", got.State)
			}
			for _, id := range []string{"networks", "volumes", "services", "cleanup", "network:demo_default", "volume:demo_data", "service:web"} {
				if it := itemByID(t, job, id); it.State != jobs.ItemSkipped || it.Code != stackops.CodeNotRun {
					t.Errorf("%s = %s %s, want skipped, not_run", id, it.State, it.Code)
				}
			}
		})
	}
}

func TestPrePullRecordsAnUnauthorizedPullOnTheCredentialAndClearsItAfterOneThatWorked(t *testing.T) {
	t.Parallel()
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.PullStatus = dockertest.FakePullStatus{Code: 401, Message: "unauthorized: bad token"}
	})
	creds := withCredential(t, rig)
	rig.deps.Up = (&upStandIn{}).up
	const compose = "services:\n  app:\n    image: " + appImage + "\n"

	in := standaloneInput("demo", compose)
	in.Reviewed = map[string]string{"app": appImage + "@" + digestOf("a")}
	res, job := rig.deployStandalone(t, in)

	if res.OK() || res.Code != dockerx.PullCodeUnauthorized {
		t.Fatalf("result = %+v, want pull_unauthorized", res)
	}
	list, err := creds.List(context.Background())
	if err != nil || len(list) != 1 || list[0].LastFailure == nil || list[0].LastFailure.Code != "pull_unauthorized" {
		t.Fatalf("credentials = %+v (%v), want the failure recorded", list, err)
	}
	// The step names the credential that was used, never its secret.
	pull := itemByID(t, job, "pull:"+appImage+"@"+firstDigest(job))
	if pull.Result["credential"] != "ghcr-acme" || pull.Result["credentialId"] == "" {
		t.Errorf("pull result = %v, want the credential's name and ID", pull.Result)
	}
}

// firstDigest is the digest of the first pull step of the job.
func firstDigest(job jobs.Job) string {
	for _, it := range job.Items {
		if _, ref, ok := strings.Cut(it.ID, "@"); ok && strings.HasPrefix(it.ID, "pull:") {
			return ref
		}
	}
	return ""
}

func TestPrePullCancelledInTheMiddleOfAPullEndsThePullAndTheStepsAfterItCancelled(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	rig := newSwarmRig(t, func(c *dockertest.FakeConfig) {
		standalone(c)
		c.TasksFollowWrites = false
		c.PullHold = gate
	})
	t.Cleanup(func() { close(gate) })
	stand := &upStandIn{}
	rig.deps.Up = stand.up
	in := standaloneInput("demo", webStandalone)
	in.Reviewed = map[string]string{"web": "nginx:1.27-alpine@" + digestOf("b")}
	id, done := rig.startStandalone(t, in)

	// The pull is on the engine, held; cancel the job there.
	if err := rig.fake.Await(testsupport.Context(t), func() bool { return len(pulls(rig)) == 1 }); err != nil {
		t.Fatalf("waiting for the pull to reach the engine: %v", err)
	}
	if err := rig.hub.Cancel(id); err != nil {
		t.Fatal(err)
	}
	res := testsupport.Await(t, "the deploy to end", done)

	if res.Code != stackops.CodeCancelled || stand.calls != 0 {
		t.Fatalf("result %+v after %d SDK calls, want cancelled before the SDK", res, stand.calls)
	}
	job, err := rig.hub.Wait(testsupport.Context(t), id)
	if err != nil {
		t.Fatal(err)
	}
	if got := itemByID(t, job, "pull:nginx:1.27-alpine@"+digestOf("b")); got.State != jobs.ItemCancelled || got.Code != stackops.CodeCancelled {
		t.Errorf("pull step = %s %s, want cancelled", got.State, got.Code)
	}
	for _, step := range []string{"networks", "services", "cleanup"} {
		if got := itemByID(t, job, step); got.State != jobs.ItemCancelled {
			t.Errorf("%s = %s, want cancelled", step, got.State)
		}
	}
}
