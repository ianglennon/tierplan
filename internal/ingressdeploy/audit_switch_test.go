// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/proto/ingresswire"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// switchCanary is a join token the fake engine puts in its refusals, so a row that copied the engine's words would hold it.
var switchCanary = ingresswire.TokenPrefix + "canary-0123456789abcdef"

// auditCountResult is the number of audit rows of an action with a result.
func (r *rig) auditCountResult(action string, result audit.Result) int {
	r.t.Helper()
	var n int
	if err := r.st.Read.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND result = ?`, action, string(result)).Scan(&n); err != nil {
		r.t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// wantFailureRow fails the test unless rows holds exactly one row of the given result for action, by the test's actor on the
// environment, whose reason is code and whose details are exactly the step and the code.
func wantFailureRow(t *testing.T, rows []audit.Row, envID, action, step, code string) {
	t.Helper()
	var failures []audit.Row
	for _, r := range rows {
		if r.Result == string(audit.ResultFailure) {
			failures = append(failures, r)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("%d %s failure rows, want 1: %+v", len(failures), action, rows)
	}
	r := failures[0]
	if r.Action != action || r.ActorName != testActor.Name || r.TargetType != "environment" || r.TargetID != envID || r.EnvID != envID {
		t.Errorf("row = %+v, want %s by %s on environment %s", r, action, testActor.Name, envID)
	}
	if r.Reason != code {
		t.Errorf("reason = %q, want %q", r.Reason, code)
	}
	if len(r.Details) != 2 || r.Details["step"] != step || r.Details["code"] != code {
		t.Errorf("details = %v, want exactly step %q and code %q", r.Details, step, code)
	}
}

// TestAuditProofEnabledFailure is the proof of A-01 class F for a swarm switch-on: the service step fails with an engine
// answer that holds a join token, and the log gets one ingress.enabled failure row with the step and its code and none of
// the engine's words.
func TestAuditProofEnabledFailure(t *testing.T) {
	r := newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
		c.ServiceCreateError = "the manager cannot create the service with " + switchCanary
	}})
	job := r.enable(ingressdeploy.EnableRequest{})
	if it := itemOf(job, ingressdeploy.ItemService); job.State == jobs.JobDone || it.State != jobs.ItemFailed {
		t.Fatalf("the enable ended %q with the service item %q, want it failed", job.State, it.State)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressEnabled)
	wantFailureRow(t, rows, r.envID, audit.ActionIngressEnabled, ingressdeploy.ItemService, ingressdeploy.CodeEnableServiceFailed)
	if len(rows) != 1 {
		t.Errorf("%d ingress.enabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}

// failingDownSystem is a standalone deployer whose project down is refused with an answer that holds a join token.
type failingDownSystem struct{ ingressdeploy.SystemDeployer }

func (failingDownSystem) DownSystem(context.Context, string, string) error {
	return errors.New("the host refused the down: " + switchCanary)
}

// TestAuditProofDisabledFailure is the proof of A-01 class F for a switch-off: a swarm switch-off whose service removal is
// refused and a standalone one whose project down is refused each log one ingress.disabled failure row with the step and its
// code and none of the engine's words. The success row of the switch itself, written before the step, stays beside it.
func TestAuditProofDisabledFailure(t *testing.T) {
	t.Run("swarm", func(t *testing.T) {
		f := newDisableFix(t, disableOpts{})
		f.fake.SetServiceRemoveError(ingress.ServiceName, "the manager cannot remove the service "+switchCanary)
		job := f.disable(seesAll)
		if it := itemOf(job, ingressdeploy.ItemService); it.State != jobs.ItemFailed {
			t.Fatalf("service item = %q (code %q), want failed; job %q", it.State, it.Code, job.State)
		}

		rows := audittest.Rows(t, f.st, audit.ActionIngressDisabled)
		wantFailureRow(t, rows, f.envID, audit.ActionIngressDisabled, ingressdeploy.ItemService, ingressdeploy.CodeDisableServiceFailed)
		audittest.AssertNoSecrets(t, rows, switchCanary)
	})
	t.Run("standalone", func(t *testing.T) {
		r := newStandaloneDisableRig(t, func(d *ingressdeploy.Deps) { d.System = failingDownSystem{d.System} })
		job := r.disable()
		if it := itemOf(job, "proxy"); it.State != jobs.ItemFailed {
			t.Fatalf("proxy item = %q (code %q), want failed; job %q", it.State, it.Code, job.State)
		}

		rows := audittest.Rows(t, r.st, audit.ActionIngressDisabled)
		wantFailureRow(t, rows, r.envID, audit.ActionIngressDisabled, "proxy", ingressdeploy.CodeDisableProxyFailed)
		audittest.AssertNoSecrets(t, rows, switchCanary)
	})
}

// TestStandaloneEnableFailureIsAudited: a standalone switch-on whose project does not come up logs one ingress.enabled
// failure row that names the system step as a whole, and no success row.
func TestStandaloneEnableFailureIsAudited(t *testing.T) {
	r := newStandaloneRig(t, standaloneRigOptions{result: stackops.DeployResult{Code: stackops.CodeNameClash}})
	job := r.enable(ingressdeploy.EnableRequest{})
	if job.State == jobs.JobDone {
		t.Fatalf("the enable ended done, want it failed; items = %+v", job.Items)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressEnabled)
	wantFailureRow(t, rows, r.envID, audit.ActionIngressEnabled, "system", ingressdeploy.CodeEnableSystemFailed)
	if len(rows) != 1 {
		t.Errorf("%d ingress.enabled rows, want only the failure: %+v", len(rows), rows)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary)
}

// afterFirstWriteEngine lets the first write on the manager finish and then holds the job until the test has cancelled it,
// so the job is cancelled between two steps with no sleep.
type afterFirstWriteEngine struct {
	ingressdeploy.Engine
	g *gate
}

func (e afterFirstWriteEngine) WriteOnManager(ctx context.Context, envID string, fn func(*dockerx.Client) error) error {
	err := e.Engine.WriteOnManager(ctx, envID, fn)
	e.g.once.Do(func() {
		close(e.g.entered)
		<-e.g.proceed
	})
	return err
}

// TestCancelledSwitchIsAudited: a switch-on that is cancelled after its first step logs one ingress.enabled failure row whose
// reason is cancelled, naming the step it did not start.
func TestCancelledSwitchIsAudited(t *testing.T) {
	g := newGate()
	r := newRig(t, rigOptions{deps: func(d *ingressdeploy.Deps) { d.Engine = afterFirstWriteEngine{Engine: d.Engine, g: g} }})
	ctx := testsupport.Context(t)
	id, err := r.svc.Enable(ctx, testActor, testOwner, r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	select {
	case <-g.entered:
	case <-ctx.Done():
		t.Fatal("the job never finished its first write")
	}
	if err := r.hub.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(g.proceed)
	job := r.wait(id)
	if it := itemOf(job, ingressdeploy.ItemNetwork); it.State != jobs.ItemCancelled {
		t.Fatalf("network item = %q (code %q), want cancelled; job %q", it.State, it.Code, job.State)
	}

	rows := audittest.Rows(t, r.st, audit.ActionIngressEnabled)
	wantFailureRow(t, rows, r.envID, audit.ActionIngressEnabled, ingressdeploy.ItemNetwork, jobs.CodeCancelled)
	if len(rows) != 1 {
		t.Errorf("%d ingress.enabled rows, want only the cancelled one: %+v", len(rows), rows)
	}
}

// wantOneSuccessRow fails the test unless rows is exactly one success row of action by the test's actor on the environment.
func wantOneSuccessRow(t *testing.T, rows []audit.Row, envID, action string) audit.Row {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%d %s rows, want 1 success row: %+v", len(rows), action, rows)
	}
	r := rows[0]
	if r.Result != string(audit.ResultSuccess) || r.ActorName != testActor.Name || r.TargetType != "environment" || r.TargetID != envID || r.EnvID != envID {
		t.Fatalf("row = %+v, want a success of %s by %s on environment %s", r, action, testActor.Name, envID)
	}
	return r
}

// TestAuditProofEnabled: a successful swarm switch-on writes its one success row, with the subnet and the image, in the
// switch's transaction and no failure row, and no row holds a join token.
func TestAuditProofEnabled(t *testing.T) {
	r := newRig(t, rigOptions{})
	wantAllDone(t, r.enable(ingressdeploy.EnableRequest{}))

	rows := audittest.Rows(t, r.st, audit.ActionIngressEnabled)
	row := wantOneSuccessRow(t, rows, r.envID, audit.ActionIngressEnabled)
	if row.Details["subnet"] != "10.0.4.0/22" || row.Details["image"] != ingressdeploy.IngressImage(testServerVersion) || len(row.Details) != 2 {
		t.Errorf("details = %v, want exactly the subnet and the image", row.Details)
	}
	audittest.AssertNoSecrets(t, rows, switchCanary, ingresswire.TokenPrefix)
}

// TestAuditProofDisabled: a successful swarm switch-off writes its one success row and no failure row, and no row holds a
// join token.
func TestAuditProofDisabled(t *testing.T) {
	f := newDisableFix(t, disableOpts{})
	job := f.disable(seesAll)
	if job.State != jobs.JobDone {
		t.Fatalf("the switch-off ended %q, want done; items = %+v", job.State, job.Items)
	}

	rows := audittest.Rows(t, f.st, audit.ActionIngressDisabled)
	wantOneSuccessRow(t, rows, f.envID, audit.ActionIngressDisabled)
	audittest.AssertNoSecrets(t, rows, switchCanary, ingresswire.TokenPrefix)
}
