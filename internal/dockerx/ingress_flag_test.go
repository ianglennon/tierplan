// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
)

// TestServiceRowIngressFlag pins D-28 and D-41: a service is the ingress proxy only with the component label
// and the reserved name together. The label on another name, the name without the label and the agent service
// are not flagged, on the cluster rows and on the inspected detail alike.
func TestServiceRowIngressFlag(t *testing.T) {
	proxy := ingressdeploy.ProxyServiceSpec(ingressdeploy.SpecInput{Image: "ghcr.io/x/y:1", URL: "wss://h/ingress/connect", SecretName: "s"})
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "proxy-id", Name: proxy.Name, Mode: "global", Labels: proxy.Labels},
			{ID: "forged-id", Name: "web", Mode: "replicated", Labels: ingress.ObjectLabels()},
			{ID: "bare-id", Name: ingress.ServiceName, Mode: "replicated"},
			{ID: "agent-id", Name: "tierplan-agent", Mode: "global"},
		},
	})
	c := newClient(t, fake)
	want := map[string]bool{"proxy-id": true, "forged-id": false, "bare-id": false, "agent-id": false}

	got, err := c.Cluster(context.Background())
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if len(got.Services) != len(want) {
		t.Fatalf("services = %+v, want %d", got.Services, len(want))
	}
	for _, r := range got.Services {
		if r.Ingress != want[r.ID] {
			t.Errorf("row %s: Ingress = %v, want %v", r.ID, r.Ingress, want[r.ID])
		}
	}
	for id, w := range want {
		d, _, err := c.ServiceInspect(context.Background(), id)
		if err != nil {
			t.Fatalf("ServiceInspect %s: %v", id, err)
		}
		if d.Ingress != w {
			t.Errorf("detail %s: Ingress = %v, want %v", id, d.Ingress, w)
		}
	}
}
