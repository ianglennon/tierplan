// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// Adding the local host on a swarm node is refused by the server (SWM-01, D-02, D-21): the UI never offers
// it, and no other client can make the request succeed either.

func TestTracerAddingTheLocalHostOnASwarmNodeIsRefused(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{
		Hostname: "mgr-1", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-local", SwarmClusterID: "cluster-1",
	})
	for _, body := range []string{`{"kind":"local"}`, `{"kind":"local","allowUnsupported":true}`} {
		rec := f.req(t, http.MethodPost, "/api/environments", body)
		if rec.code != http.StatusConflict || rec.ptyp != "local_is_swarm_node" {
			t.Fatalf("POST %s: status = %d, type %q: %s", body, rec.code, rec.ptyp, rec.body)
		}
	}
	if got := f.req(t, http.MethodGet, "/api/environments", ""); strings.Contains(string(got.body), `"id"`) {
		t.Errorf("a refused add stored an environment: %s", got.body)
	}
	if entries := f.audits(t, audit.ActionEnvironmentAdded); len(entries) != 0 {
		t.Errorf("a refused add wrote environment.added rows: %+v", entries)
	}
}
