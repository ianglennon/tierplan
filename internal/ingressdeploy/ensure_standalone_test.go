// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

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
