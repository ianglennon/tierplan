// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"sync"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// steppedClock is a clock a test moves by hand. After never fires: the tests take their passes with Round.
type steppedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *steppedClock) After(time.Duration) <-chan time.Time { return nil }

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// enabledStandaloneRig is a standalone rig an enable has switched on: the proxy runs, one token (generation 1) was issued
// and the deployer was called once. No node row exists, so the proxy has not enrolled.
func enabledStandaloneRig(t *testing.T, o standaloneRigOptions) *standaloneRig {
	t.Helper()
	r := newStandaloneRig(t, o)
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
	if got := r.sys.deployCount(); got != 1 {
		t.Fatalf("deploys after the enable = %d, want 1", got)
	}
	return r
}

// generation is the current join token's generation, 0 for none.
func (r *standaloneRig) generation() int {
	r.t.Helper()
	st, err := r.tokens.TokenStatus(testsupport.Context(r.t), r.envID)
	if err != nil {
		r.t.Fatalf("TokenStatus: %v", err)
	}
	return st.Generation
}

// held is what the fake host holds now, to be changed and given back with setHost.
func (r *standaloneRig) held() hostFixture {
	r.sys.mu.Lock()
	defer r.sys.mu.Unlock()
	h := r.sys.host
	if h.Proxy != nil {
		p := *h.Proxy
		h.Proxy = &p
	}
	return h
}

// ensureJobs are the ensure jobs the hub holds.
func (r *standaloneRig) ensureJobs() []jobs.Job {
	return r.hub.List(func(j jobs.Job) bool { return j.Kind == ingressdeploy.JobKindEnsure })
}

// TestStandaloneEnsureRepairsAndIsQuiet is the tracer: a pass over a standalone host whose ingress is on and whose proxy
// container is gone issues one token and redeploys the system project in an ingress_ensure job owned by system:ingress,
// and a second pass over the converged host issues no token and makes no call.
func TestStandaloneEnsureRepairsAndIsQuiet(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	h := r.held()
	h.Proxy, h.NetworkSubnet = nil, ""
	r.sys.setHost(h)

	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Fatalf("deploys after the repairing pass = %d, want 2", got)
	}
	if got := r.generation(); got != 2 {
		t.Errorf("token generation = %d, want 2: one token for the repair", got)
	}
	in := r.sys.deploys[1]
	if !in.NoWait || in.EnvID != r.envID || in.NodeID != testAgentID {
		t.Errorf("deploy input = env %q node %q nowait %v", in.EnvID, in.NodeID, in.NoWait)
	}
	if got := r.sys.tokens[1]; len(got) < len(ingresswire.TokenPrefix) || got[:len(ingresswire.TokenPrefix)] != ingresswire.TokenPrefix {
		t.Error("the repair was not given a join token")
	}
	if at, class := r.ensureState(); at.IsZero() || class != ingressdeploy.EnsureOK {
		t.Errorf("ensure state = %v %q, want a pass recorded as ok", at, class)
	}
	js := r.ensureJobs()
	if len(js) != 1 || js[0].Owner != "system:ingress" || js[0].State != jobs.JobDone || js[0].EnvID != r.envID {
		t.Fatalf("ensure jobs = %+v, want one done job owned by system:ingress", js)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the pass")
	}

	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Errorf("a pass over the converged host called the deployer (%d deploys in all)", got)
	}
	if got := r.generation(); got != 2 {
		t.Errorf("a pass over the converged host issued a token (generation %d)", got)
	}
	if len(r.ensureJobs()) != 1 {
		t.Error("a pass over the converged host started a job")
	}
}

// TestStandaloneEnsureTriggers: an old image label, a different access-log variable, a missing container and a missing
// network each lead to one redeploy, after which the host is quiet; a container that is restarting leads to none.
func TestStandaloneEnsureTriggers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hostFixture)
		deploy bool
	}{
		{"old image", func(h *hostFixture) { h.Proxy.Image = "ghcr.io/example/proxy:0.0.1" }, true},
		{"access log", func(h *hostFixture) { h.Proxy.AccessLog = true }, true},
		{"missing container", func(h *hostFixture) { h.Proxy = nil }, true},
		{"missing network", func(h *hostFixture) { h.NetworkSubnet = "" }, true},
		{"restarting", func(h *hostFixture) { h.Proxy.State = "restarting" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := enabledStandaloneRig(t, standaloneRigOptions{})
			h := r.held()
			tc.change(&h)
			r.sys.setHost(h)
			want := 1
			if tc.deploy {
				want = 2
			}
			r.round()
			if got := r.sys.deployCount(); got != want {
				t.Fatalf("deploys after one pass = %d, want %d", got, want)
			}
			r.round()
			if got := r.sys.deployCount(); got != want {
				t.Errorf("deploys after a second pass = %d, want %d: the host should be quiet", got, want)
			}
		})
	}
}

// TestStandaloneEnsureEnrolledNeedsNoToken: once the proxy has enrolled (an active node row and the cache volume), a
// redeploy for a new image carries no token and no secret, and issues none.
func TestStandaloneEnsureEnrolledNeedsNoToken(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	r.addNodeRow("active")
	h := r.held()
	h.Proxy.Image = "ghcr.io/example/proxy:0.0.1"
	r.sys.setHost(h)

	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Fatalf("deploys = %d, want 2", got)
	}
	if got := r.sys.tokens[1]; got != "" {
		t.Error("an enrolled proxy's redeploy was given a join token")
	}
	if _, ok := r.sys.deploys[1].Project.Environment["TIERPLAN_SECRET_token"]; ok {
		t.Error("the project carries a token secret")
	}
	if got := r.generation(); got != 1 {
		t.Errorf("token generation = %d, want 1: none is issued for an enrolled proxy", got)
	}
	r.round()
	if got := r.sys.deployCount(); got != 2 {
		t.Errorf("deploys after a second pass = %d, want 2", got)
	}
	if got := r.generation(); got != 1 {
		t.Errorf("token generation after a second pass = %d, want 1", got)
	}
}

// TestStandaloneEnsureClasses: each way a pass can end short of a repair records its own class, and writes nothing.
func TestStandaloneEnsureClasses(t *testing.T) {
	missing := func(r *standaloneRig) {
		h := r.held()
		h.Proxy = nil
		r.sys.setHost(h)
	}
	for _, tc := range []struct {
		name  string
		setup func(*standaloneRig) func()
		class string
		// deploys is how many deploys the rig holds after the pass: the enable's one, and a failed repair's.
		deploys int
	}{
		{name: "unreachable", class: ingressdeploy.EnsureUnreachable, deploys: 1, setup: func(r *standaloneRig) func() {
			missing(r)
			r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
			return nil
		}},
		{name: "locked", class: ingressdeploy.EnsureLocked, deploys: 1, setup: func(r *standaloneRig) func() {
			missing(r)
			release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
			if !ok {
				r.t.Fatal("could not take the lock for the stack deploy")
			}
			return release
		}},
		{name: "url unset", class: ingressdeploy.EnsureURLUnset, deploys: 1, setup: func(r *standaloneRig) func() {
			missing(r)
			return nil
		}},
		{name: "write failed", class: ingressdeploy.EnsureWriteFailed, deploys: 2, setup: func(r *standaloneRig) func() {
			missing(r)
			r.sys.mu.Lock()
			r.sys.result = stackops.DeployResult{Code: stackops.CodeNameClash}
			r.sys.mu.Unlock()
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The enable needs a usable address, so the rig starts with one and the test blanks it.
			u := &switchableURL{url: "wss://tierplan.example:7480/ingress/connect"}
			r := enabledStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) { d.URL = u.get }})
			release := tc.setup(r)
			if release != nil {
				defer release()
			}
			if tc.class == ingressdeploy.EnsureURLUnset {
				u.set("")
			}
			gen := r.generation()
			r.round()
			if _, class := r.ensureState(); class != tc.class {
				t.Errorf("class = %q, want %q", class, tc.class)
			}
			if got := r.sys.deployCount(); got != tc.deploys {
				t.Errorf("deploys = %d, want %d", got, tc.deploys)
			}
			if tc.class != ingressdeploy.EnsureWriteFailed && r.generation() != gen {
				t.Error("a pass that could not write issued a token")
			}
		})
	}
}

// switchableURL is the ingress URL a test can blank after the enable.
type switchableURL struct {
	mu  sync.Mutex
	url string
}

func (u *switchableURL) set(s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.url = s
}

func (u *switchableURL) get() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.url
}

// TestStandaloneEnsurePausedWritesNothing: an environment a restore paused is only observed: the pass issues no token,
// never calls the deployer, and records paused, although the proxy is gone.
func TestStandaloneEnsurePausedWritesNothing(t *testing.T) {
	r := enabledStandaloneRig(t, standaloneRigOptions{})
	h := r.held()
	h.Proxy, h.NetworkSubnet = nil, ""
	r.sys.setHost(h)
	if _, err := r.istore.SetPaused(testsupport.Context(r.t), r.envID, true, "system:restore", nil); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	r.round()
	r.round()
	if got := r.sys.deployCount(); got != 1 {
		t.Errorf("deploys = %d, want 1: a paused environment is never written to", got)
	}
	if got := r.generation(); got != 1 {
		t.Errorf("token generation = %d, want 1: none is issued while paused", got)
	}
	if _, class := r.ensureState(); class != ingressdeploy.EnsurePaused {
		t.Errorf("class = %q, want paused", class)
	}
	if got := r.auditCount("ingress.deploy_paused"); got != 1 {
		t.Errorf("ingress.deploy_paused audit rows = %d, want 1", got)
	}
	if len(r.ensureJobs()) != 0 {
		t.Error("a paused environment started an ensure job")
	}
}

// TestStandaloneEnsureNotEnrolledAfterBound: a running proxy with no active node row is ok until it has run for
// NotEnrolledAfter and not_enrolled after; a node row ends the report; neither writes.
func TestStandaloneEnsureNotEnrolledAfterBound(t *testing.T) {
	// The fake host's proxy started at 2026-11-21T12:00:00Z.
	clk := &steppedClock{at: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	r := enabledStandaloneRig(t, standaloneRigOptions{deps: func(d *ingressdeploy.Deps) { d.Clock = clk }})
	writes := len(r.fake.Writes())

	clk.advance(4 * time.Minute)
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class at 4 minutes = %q, want ok", class)
	}

	clk.advance(2 * time.Minute)
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureNotEnrolled {
		t.Errorf("class at 6 minutes = %q, want not_enrolled", class)
	}
	if got := r.sys.deployCount(); got != 1 {
		t.Errorf("deploys = %d, want 1: an unenrolled proxy is reported, not recreated", got)
	}
	if got := r.generation(); got != 1 {
		t.Errorf("token generation = %d, want 1", got)
	}
	if got := len(r.fake.Writes()); got != writes {
		t.Errorf("the passes wrote %v", r.fake.Writes()[writes:])
	}

	r.addNodeRow("active")
	r.round()
	if _, class := r.ensureState(); class != ingressdeploy.EnsureOK {
		t.Errorf("class once the node enrolled = %q, want ok", class)
	}
}
