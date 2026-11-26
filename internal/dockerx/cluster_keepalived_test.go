// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// TestKeepalivedServiceIsIngressOwned pins D-14 and A-13: the keepalived service is flagged Ingress, as the
// proxy is, only with the keepalived label and its exact reserved name together. The label on another name, the
// name without the label and the ingress label on the keepalived name are not flagged, on the cluster rows and on
// the inspected detail alike.
func TestKeepalivedServiceIsIngressOwned(t *testing.T) {
	fake := dockertest.NewFake(t, dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true,
		ServiceRows: []dockertest.FakeService{
			{ID: "ka-id", Name: ingress.ServiceNameKeepalived, Mode: "global", Labels: ingress.KeepalivedLabels()},
			{ID: "forged-id", Name: "web", Mode: "replicated", Labels: ingress.KeepalivedLabels()},
			{ID: "bare-id", Name: ingress.ServiceNameKeepalived, Mode: "replicated"},
			{ID: "cross-id", Name: ingress.ServiceNameKeepalived, Mode: "replicated", Labels: ingress.ObjectLabels()},
		},
	})
	c := newClient(t, fake)
	want := map[string]bool{"ka-id": true, "forged-id": false, "bare-id": false, "cross-id": false}

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
