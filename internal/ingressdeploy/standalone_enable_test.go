// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingressbundle"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// wantStandaloneDone fails the test unless every item of the standalone enable job ended done.
func wantStandaloneDone(t *testing.T, job jobs.Job) {
	t.Helper()
	if job.State != jobs.JobDone {
		t.Fatalf("job state = %q, want done; items = %+v", job.State, job.Items)
	}
	ids := []string{ingressdeploy.ItemToken, ingressdeploy.ItemSwitch}
	for _, sp := range stackops.SystemItemSpecs() {
		ids = append(ids, sp.ID)
	}
	for _, id := range ids {
		if it := itemOf(job, id); it.State != jobs.ItemDone {
			t.Errorf("item %s = %q (code %q), want done", id, it.State, it.Code)
		}
	}
}

// TestStandaloneEnableDeploysTheProxy is the tracer: an enable on a standalone host whose node is connected issues one
// join token, deploys the proxy's project once through the standalone pipeline without waiting for the proxy to enrol,
// and switches ingress on with one audit row that records the subnet and the image.
func TestStandaloneEnableDeploysTheProxy(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	job := r.enable(ingressdeploy.EnableRequest{})
	wantStandaloneDone(t, job)
	if job.Kind != ingressdeploy.JobKindEnable || job.EnvID != r.envID || job.Owner != testOwner {
		t.Errorf("job = kind %q env %q owner %q", job.Kind, job.EnvID, job.Owner)
	}
	order := make([]string, 0, len(job.Items))
	for _, it := range job.Items {
		order = append(order, it.ID)
	}
	want := []string{ingressdeploy.ItemToken}
	for _, sp := range stackops.SystemItemSpecs() {
		want = append(want, sp.ID)
	}
	want = append(want, ingressdeploy.ItemSwitch)
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("items = %v, want the token, the system items, the switch: %v", order, want)
	}

	ctx := testsupport.Context(t)
	tok, err := r.tokens.TokenStatus(ctx, r.envID)
	if err != nil || !tok.Exists || tok.Generation != 1 {
		t.Fatalf("TokenStatus = %+v, %v; want one token, generation 1", tok, err)
	}
	if it := itemOf(job, ingressdeploy.ItemToken); it.Result["issued"] != "true" {
		t.Errorf("token item result = %v, want issued", it.Result)
	}

	if r.sys.deployCount() != 1 {
		t.Fatalf("deploys = %d, want 1", r.sys.deployCount())
	}
	in := r.sys.deploys[0]
	if in.EnvID != r.envID || in.NodeID != testAgentID || !in.NoWait || in.Prefix != 24 {
		t.Errorf("deploy input = env %q node %q nowait %v prefix %d", in.EnvID, in.NodeID, in.NoWait, in.Prefix)
	}
	image := ingressdeploy.IngressImage(testServerVersion)
	svc := in.Project.Services[ingress.ProxyComposeService]
	if in.Project.Name != ingress.ProjectName || svc.Image != image {
		t.Errorf("project = %q image %q, want %q %q", in.Project.Name, svc.Image, ingress.ProjectName, image)
	}
	envOf := func(k string) string {
		if v := svc.Environment[k]; v != nil {
			return *v
		}
		return ""
	}
	if got := envOf(ingressbundle.EnvURL); got != "wss://tierplan.example:7480/ingress/connect" {
		t.Errorf("URL variable = %q", got)
	}
	if got := envOf(ingressbundle.EnvPin); got != "sha256/AAAA,sha256/BBBB" {
		t.Errorf("pin variable = %q, want the two pins joined by a comma", got)
	}
	if _, on := svc.Environment[ingressbundle.EnvAccessLog]; on {
		t.Error("the access-log variable is set although the stored switch is off")
	}
	if svc.CustomLabels[ingressdeploy.TokenLabel] != tok.ID {
		t.Errorf("token label = %q, want the issued token's ID %q", svc.CustomLabels[ingressdeploy.TokenLabel], tok.ID)
	}
	if got := r.sys.tokens[0]; !strings.HasPrefix(got, ingresswire.TokenPrefix) {
		t.Error("the project was not given a join token")
	}

	state, err := r.istore.DeployState(ctx, r.envID)
	if err != nil || !state.Enabled {
		t.Fatalf("DeployState = %+v, %v; want enabled", state, err)
	}
	if got := r.auditCount("ingress.enabled"); got != 1 {
		t.Fatalf("ingress.enabled audit rows = %d, want 1", got)
	}
	var details string
	if err := r.st.Read.QueryRow(`SELECT details FROM audit_log WHERE action = 'ingress.enabled'`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"10.0.8.0/24", image} {
		if !strings.Contains(details, w) {
			t.Errorf("audit details %q do not name %q", details, w)
		}
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the operation lock is still held after the job")
	}
	if len(r.sys.downs) != 0 {
		t.Errorf("downs = %v, want none", r.sys.downs)
	}
}

// TestStandaloneEnableNeverShowsTheToken: the clear text of the join token is in no item, detail or output line of the
// job and in no audit detail, whether the proxy came up or the deploy failed.
func TestStandaloneEnableNeverShowsTheToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result stackops.DeployResult
	}{
		{"deployed", stackops.DeployResult{}},
		{"failed", stackops.DeployResult{Code: stackops.CodeNameClash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStandaloneRig(t, standaloneRigOptions{result: tc.result})
			job := r.enable(ingressdeploy.EnableRequest{})
			if r.sys.deployCount() != 1 {
				t.Fatalf("deploys = %d, want 1", r.sys.deployCount())
			}
			clear := r.sys.tokens[0]
			if !strings.HasPrefix(clear, ingresswire.TokenPrefix) {
				t.Fatalf("the deploy was given %q, want a join token", clear)
			}
			raw, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			var all strings.Builder
			all.Write(raw)
			lines, _ := r.hub.Output(job.ID)
			for _, l := range lines {
				all.WriteString(l.Text)
			}
			rows, err := r.st.Read.Query(`SELECT details FROM audit_log`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
				var d string
				if err := rows.Scan(&d); err != nil {
					t.Fatal(err)
				}
				all.WriteString(d)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(all.String(), clear) || strings.Contains(all.String(), ingresswire.TokenPrefix) {
				t.Error("the job, its output or an audit row carries the join token")
			}
		})
	}
}

// TestStandaloneEnableFailureLeavesIngressOff: a deploy that fails ends the job as an enable failure: the switch is
// not run, ingress stays off and the lock is free.
func TestStandaloneEnableFailureLeavesIngressOff(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{result: stackops.DeployResult{Code: stackops.CodeNameClash}})
	job := r.enable(ingressdeploy.EnableRequest{})
	if it := itemOf(job, ingressdeploy.ItemSwitch); it.State != jobs.ItemSkipped || it.Code != ingressdeploy.CodeNotRun {
		t.Errorf("switch item = %q (code %q), want skipped not_run", it.State, it.Code)
	}
	if it := itemOf(job, stackops.ItemCheck); it.State != jobs.ItemFailed || it.Code != stackops.CodeNameClash {
		t.Errorf("check item = %q (code %q), want failed with the deploy's code", it.State, it.Code)
	}
	state, err := r.istore.DeployState(testsupport.Context(t), r.envID)
	if err != nil || state.Enabled || state.EnsureError != ingressdeploy.EnsureEnableFailed {
		t.Errorf("DeployState = %+v, %v; want off with a failed-enable record", state, err)
	}
	if got := r.auditCount("ingress.enabled"); got != 0 {
		t.Errorf("ingress.enabled audit rows = %d, want 0", got)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is held after the job")
	}
}

// TestStandaloneEnableTakesTheOperationLock: an enable on a standalone host is refused while a stack operation holds
// the environment, holds the lock for the whole job under HolderKindIngress, and refuses a stack operation meanwhile.
func TestStandaloneEnableTakesTheOperationLock(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	ctx := testsupport.Context(t)

	release, _, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackID: "s1", StackName: "web", By: "user:u2"})
	if !ok {
		t.Fatal("could not take the lock for the stack deploy")
	}
	_, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	var locked *ingressdeploy.LockedError
	if !errors.As(err, &locked) || locked.Holder.StackName != "web" || locked.Holder.Kind != "deploy" {
		t.Fatalf("Enable under a stack deploy = %v, want a *LockedError naming the holder", err)
	}
	if r.sys.deployCount() != 0 {
		t.Error("a refused enable deployed")
	}
	if st, _ := r.tokens.TokenStatus(ctx, r.envID); st.Exists {
		t.Error("a refused enable issued a join token")
	}
	release()

	g := newGate()
	r.sys.gate = g
	id, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("the job never reached the deploy")
	}
	held, isHeld := r.locks.Held(r.envID)
	if !isHeld || held.Kind != stackops.HolderKindIngress || held.JobID != id || held.By != testOwner || held.Workload != "ingress" {
		t.Fatalf("holder while the job runs = %+v (held %v), want the ingress job %s", held, isHeld, id)
	}
	if _, who, ok := r.locks.TryAcquire(r.envID, stackops.Holder{Kind: "deploy", StackName: "web"}); ok || who.Kind != stackops.HolderKindIngress {
		t.Fatalf("a stack deploy while ingress runs: ok=%v holder=%+v, want refused naming the ingress job", ok, who)
	}
	close(g.proceed)
	wantStandaloneDone(t, r.wait(id))
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the lock is held after the job ended")
	}
}

// convergedHost is a host that holds everything an enable wants, the proxy created with the token of the given ID.
func convergedHost(tokenID string) hostFixture {
	return hostFixture{
		Proxy:         &proxyFixture{Image: ingressdeploy.IngressImage(testServerVersion), TokenLabel: tokenID},
		NetworkSubnet: "10.0.8.0/24", Volume: true,
	}
}

// TestStandaloneEnableOnAConvergedHostWritesNothing: a host that holds the running proxy as wanted, its network and
// volume, and an active node row is left alone: no token is issued, nothing is deployed, the system items end as
// unchanged and ingress is switched on.
func TestStandaloneEnableOnAConvergedHostWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nodeRow  bool
		labelled bool
	}{
		{name: "enrolled", nodeRow: true},
		{name: "not yet enrolled, current token label", labelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStandaloneRig(t, standaloneRigOptions{})
			ctx := testsupport.Context(t)
			tok, err := r.tokens.IssueToken(ctx, testActor, r.envID, "", "test")
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			r.sys.setHost(convergedHost(tok.ID))
			if tc.nodeRow {
				r.addNodeRow("active")
			}
			before := len(r.fake.Writes())
			job := r.enable(ingressdeploy.EnableRequest{})
			wantStandaloneDone(t, job)
			if r.sys.deployCount() != 0 {
				t.Errorf("deploys = %d, want 0", r.sys.deployCount())
			}
			if got := len(r.fake.Writes()); got != before {
				t.Errorf("the host was written to: %v", r.fake.Writes())
			}
			st, err := r.tokens.TokenStatus(ctx, r.envID)
			if err != nil || st.Generation != 1 || st.ID != tok.ID {
				t.Errorf("TokenStatus = %+v, %v; want the token unchanged at generation 1", st, err)
			}
			if it := itemOf(job, ingressdeploy.ItemToken); it.Result["issued"] != "false" {
				t.Errorf("token item result = %v, want not issued", it.Result)
			}
			for _, sp := range stackops.SystemItemSpecs() {
				if it := itemOf(job, sp.ID); it.Result["unchanged"] != "true" {
					t.Errorf("item %s result = %v, want unchanged", sp.ID, it.Result)
				}
			}
			if state, err := r.istore.DeployState(ctx, r.envID); err != nil || !state.Enabled {
				t.Errorf("DeployState = %+v, %v; want enabled", state, err)
			}
		})
	}
}

// TestStandaloneEnableAfterDisableIssuesAToken: the token was disabled by a switch-off and the container is gone: the
// enable issues the next generation and deploys with it.
func TestStandaloneEnableAfterDisableIssuesAToken(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{})
	ctx := testsupport.Context(t)
	first, err := r.tokens.IssueToken(ctx, testActor, r.envID, "", "test")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := r.tokens.DisableToken(ctx, testActor, r.envID, "", "test"); err != nil {
		t.Fatalf("DisableToken: %v", err)
	}
	r.sys.setHost(hostFixture{Volume: true})
	wantStandaloneDone(t, r.enable(ingressdeploy.EnableRequest{}))
	st, err := r.tokens.TokenStatus(ctx, r.envID)
	if err != nil || st.Generation != 2 || st.Disabled || st.ID == first.ID {
		t.Fatalf("TokenStatus = %+v, %v; want a new enabled token, generation 2", st, err)
	}
	if r.sys.deployCount() != 1 {
		t.Fatalf("deploys = %d, want 1", r.sys.deployCount())
	}
	svc := r.sys.deploys[0].Project.Services[ingress.ProxyComposeService]
	if svc.CustomLabels[ingressdeploy.TokenLabel] != st.ID || !strings.HasPrefix(r.sys.tokens[0], ingresswire.TokenPrefix) {
		t.Errorf("the deploy carried token label %q and value of %d bytes, want the new token", svc.CustomLabels[ingressdeploy.TokenLabel], len(r.sys.tokens[0]))
	}
}

// TestStandalonePreflightBasics: a host that is not connected, a paused environment and an unset address each refuse
// the preview as a finding and the enable as a preflight error, with nothing written and no job started.
func TestStandalonePreflightBasics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  string
		setup func(*testing.T, *standaloneRig)
		deps  func(*ingressdeploy.Deps)
	}{
		{name: "host not connected", code: ingressdeploy.CodeHostUnreachable, setup: func(_ *testing.T, r *standaloneRig) {
			r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes[0].Connected = false })
		}},
		{name: "no node at all", code: ingressdeploy.CodeHostUnreachable, setup: func(_ *testing.T, r *standaloneRig) {
			r.envs.set(func(i *ingressdeploy.EnvInfo) { i.Nodes = nil })
		}},
		{name: "paused", code: ingressdeploy.CodePaused, setup: func(t *testing.T, r *standaloneRig) {
			if _, err := r.istore.SetPaused(testsupport.Context(t), r.envID, true, "restore", nil); err != nil {
				t.Fatalf("SetPaused: %v", err)
			}
		}},
		{name: "url unset", code: ingressdeploy.CodeURLUnset, deps: func(d *ingressdeploy.Deps) { d.URL = func() string { return "" } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStandaloneRig(t, standaloneRigOptions{deps: tc.deps})
			if tc.setup != nil {
				tc.setup(t, r)
			}
			res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}
			if f, ok := findingOf(res.Findings, tc.code); !ok || f.Severity != ingressdeploy.SeverityError {
				t.Fatalf("preview findings = %+v, want %s as an error", res.Findings, tc.code)
			}
			if !res.Plan.Standalone {
				t.Error("the preview's plan is not marked standalone")
			}
			wantRefusal(t, r.rig, ingressdeploy.EnableRequest{}, tc.code)
			if r.sys.deployCount() != 0 {
				t.Error("a refused enable deployed")
			}
			if st, _ := r.tokens.TokenStatus(testsupport.Context(t), r.envID); st.Exists {
				t.Error("a refused enable issued a join token")
			}
		})
	}
}
