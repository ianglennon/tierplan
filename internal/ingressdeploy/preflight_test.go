// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// findingOf returns the first finding with the code, and whether there is one.
func findingOf(fs []ingressdeploy.Finding, code string) (ingressdeploy.Finding, bool) {
	for _, f := range fs {
		if f.Code == code {
			return f, true
		}
	}
	return ingressdeploy.Finding{}, false
}

// wantRefusal fails the test unless Enable is refused by a *PreflightError that holds an error finding with
// the code, and nothing was written, locked or started.
func wantRefusal(t *testing.T, r *rig, req ingressdeploy.EnableRequest, code string) ingressdeploy.Finding {
	t.Helper()
	_, err := r.svc.Enable(testsupport.Context(t), testActor, testOwner, r.envID, req)
	var pe *ingressdeploy.PreflightError
	if !errors.As(err, &pe) {
		t.Fatalf("Enable = %v, want a *PreflightError with %s", err, code)
	}
	f, ok := findingOf(pe.Findings, code)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want %s as an error", pe.Findings, code)
	}
	if _, held := r.locks.Held(r.envID); held {
		t.Error("the refusal left the lock held")
	}
	if got := r.hub.List(func(jobs.Job) bool { return true }); len(got) != 0 {
		t.Errorf("%d jobs were started", len(got))
	}
	if got := r.fake.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
	return f
}

// TestPreflightRefusesServicePorts is the tracer: a service of a stack, or of none, that publishes TCP 80 or
// 443 in either mode refuses the preview and the enable, and names each; a UDP 443 does not clash.
func TestPreflightRefusesServicePorts(t *testing.T) {
	r := newRig(t, rigOptions{config: func(c *dockertest.FakeConfig) {
		c.ServiceRows = []dockertest.FakeService{
			{ID: "s1", Name: "shop_web", Mode: "replicated", Desired: 1, Labels: map[string]string{"com.docker.stack.namespace": "shop"},
				Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 8080, PublishedPort: 80, PublishMode: "ingress"}}},
			{ID: "s2", Name: "plain", Mode: "global",
				Ports: []swarm.PortConfig{{Protocol: "tcp", TargetPort: 8443, PublishedPort: 443, PublishMode: "host"}}},
			{ID: "s3", Name: "quic", Mode: "replicated", Desired: 1,
				Ports: []swarm.PortConfig{{Protocol: "udp", TargetPort: 8443, PublishedPort: 443, PublishMode: "ingress"}}},
		}
	}})

	res, err := r.svc.Preview(testsupport.Context(t), r.envID, ingressdeploy.EnableRequest{})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	f, ok := findingOf(res.Findings, ingressdeploy.CodePortInUse)
	if !ok || f.Severity != ingressdeploy.SeverityError {
		t.Fatalf("findings = %+v, want ingress_port_in_use as an error", res.Findings)
	}
	want := []string{"plain publishes 443 (host mode)", "shop_web publishes 80 (ingress mode)"}
	if !slices.Equal(f.Items, want) {
		t.Errorf("items = %q, want %q", f.Items, want)
	}
	if ingressdeploy.CodePortInUse != "ingress_port_in_use" {
		t.Errorf("code = %q", ingressdeploy.CodePortInUse)
	}

	got := wantRefusal(t, r, ingressdeploy.EnableRequest{}, ingressdeploy.CodePortInUse)
	if !slices.Equal(got.Items, want) {
		t.Errorf("Enable's items = %q, want %q", got.Items, want)
	}
}
