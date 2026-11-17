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

// Folding retires the Local environment (D-48). Adding the host again is the hand-unfold, and it is the one add on a
// swarm node that stays open, because the node is already managed through the swarm environment (D-21).
func TestTheLocalHostCanBeAddedAgainAfterAFold(t *testing.T) {
	t.Parallel()
	f := newFoldFix(t, nil)
	if rec := f.req(t, http.MethodPost, f.foldPath(), ""); rec.code != http.StatusOK {
		t.Fatalf("fold: %d %s", rec.code, rec.body)
	}
	rec := f.req(t, http.MethodPost, "/api/environments", `{"kind":"local"}`)
	if rec.code != http.StatusCreated {
		t.Fatalf("add after a fold: %d %s, want 201 (the hand-unfold)", rec.code, rec.body)
	}
	if e := rec.env(t); e.Kind != "local" || e.ID == "" {
		t.Errorf("environment = %+v", e)
	}
}
