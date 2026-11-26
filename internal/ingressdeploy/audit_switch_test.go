// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/audit/audittest"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/ingresswire"
)

// switchCanary is a join token the fake engine puts in its refusals, so a row that copied the engine's words would hold it.
var switchCanary = ingresswire.TokenPrefix + "canary-0123456789abcdef"

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
