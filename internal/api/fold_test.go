// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/tunnel/tunneltest"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Folding the local host into the swarm environment (ENV-02, D-48, T-02-96): the API is
// the production router, the local host is a real monitored local environment on a fake
// daemon and the swarm is enrolled through a real agent session.

type foldFix struct {
	*agentFix
	env   tunneltest.Environment
	local envBody
}

// newFoldFix has the local host (local-1, node-local) manage cluster-1, the cluster
// that the agent environment's manager mgr-1 bound the swarm environment to. cfg may
// change the local daemon.
func newFoldFix(t *testing.T, cfg func(*dockertest.FakeConfig)) *foldFix {
	t.Helper()
	lc := dockertest.FakeConfig{
		Hostname: "local-1", EngineID: "ENG:local", SwarmState: "active", SwarmControl: true,
		SwarmNodeID: "node-local", SwarmClusterID: "cluster-1",
		NodeRows: []dockertest.FakeNode{
			{ID: "node-mgr-1", Hostname: "mgr-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux"},
			{ID: "node-local", Hostname: "local-1", Role: "manager", EngineVersion: "29.1.0", OS: "linux", Leader: true},
		},
	}
	if cfg != nil {
		cfg(&lc)
	}
	fake := dockertest.NewFake(t, lc)
	f := newAgentFixHost(t, "unix://"+fake.SocketPath())
	local := f.add(t)
	f.awaitFirstProbe(t, local.ID)
	env := f.srv.NewEnvironment(t, "Swarm")
	f.connect(t, env, dockertest.FakeConfig{
		Hostname: "mgr-1", SwarmState: "active", SwarmControl: true, SwarmNodeID: "node-mgr-1", SwarmClusterID: "cluster-1",
	}, &wire.Facts{
		EngineID: "ENG:mgr-1", Hostname: "mgr-1", EngineReachable: true, EngineVersion: "29.1.0",
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-mgr-1", ClusterID: "cluster-1", Control: true},
	})
	f.whenState(t, env.ID, "online")
	return &foldFix{agentFix: f, env: env, local: local}
}

type foldEnv struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	FoldOffered bool   `json:"foldOffered"`
}

func (f *foldFix) list(t *testing.T) map[string]foldEnv {
	t.Helper()
	rec := f.req(t, http.MethodGet, "/api/environments", "")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.code, rec.body)
	}
	var body struct {
		Items []foldEnv `json:"items"`
	}
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatal(err)
	}
	out := map[string]foldEnv{}
	for _, e := range body.Items {
		out[e.ID] = e
	}
	return out
}

func (f *foldFix) foldPath() string { return "/api/environments/" + f.env.ID + "/fold-local" }

func TestBothEnvironmentsReportFoldOfferedWhenTheLocalHostManagesTheEnrolledSwarm(t *testing.T) {
	f := newFoldFix(t, nil)
	got := f.list(t)
	if !got[f.local.ID].FoldOffered || !got[f.env.ID].FoldOffered {
		t.Fatalf("foldOffered = local %v swarm %v, want both true", got[f.local.ID].FoldOffered, got[f.env.ID].FoldOffered)
	}
}

func TestFoldOfferedIsFalseForAnotherClusterAndFoldingItAnswers409FoldNotOffered(t *testing.T) {
	f := newFoldFix(t, func(c *dockertest.FakeConfig) { c.SwarmClusterID = "cluster-elsewhere" })
	got := f.list(t)
	if got[f.local.ID].FoldOffered || got[f.env.ID].FoldOffered {
		t.Fatalf("foldOffered = %+v, want false for both", got)
	}
	rec := f.req(t, http.MethodPost, f.foldPath(), "")
	if rec.code != http.StatusConflict || rec.ptyp != "fold_not_offered" {
		t.Fatalf("fold: %d type %q %s, want 409 fold_not_offered", rec.code, rec.ptyp, rec.body)
	}
	if n := len(f.audits(t, audit.ActionEnvironmentLocalFolded)); n != 0 {
		t.Errorf("%d audit rows for a refused fold", n)
	}
	if _, ok := f.list(t)[f.local.ID]; !ok {
		t.Error("a refused fold retired the local environment")
	}
}

func TestFoldLocalIsForInstanceAdminsThenRetiresTheLocalEnvironmentAndAuditsIt(t *testing.T) {
	f := newFoldFix(t, nil)

	f.addAllEnvAdmin(t, "ops")
	cookie, csrf := f.signIn(t, "ops", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: f.foldPath(), cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
		t.Fatalf("fold as an admin of every environment: %d %s, want 403", rec.Code, rec.Body)
	}
	if denied := f.audits(t, audit.ActionPermissionDenied); len(denied) != 1 || denied[0].ActorID != "id-ops" {
		t.Fatalf("permission.denied entries = %+v", denied)
	}
	if !f.list(t)[f.local.ID].FoldOffered {
		t.Fatal("a refused fold changed the environments")
	}

	rec := f.req(t, http.MethodPost, f.foldPath(), "")
	if rec.code != http.StatusOK {
		t.Fatalf("fold: %d %s", rec.code, rec.body)
	}
	var body foldEnv
	if err := json.Unmarshal(rec.body, &body); err != nil || body.ID != f.env.ID || body.FoldOffered {
		t.Fatalf("body = %s (%v), want the swarm environment with no fold offered", rec.body, err)
	}
	got := f.list(t)
	if _, ok := got[f.local.ID]; ok {
		t.Error("the local environment is still listed after the fold")
	}
	if got[f.env.ID].FoldOffered {
		t.Error("the folded environment still offers a fold")
	}
	entries := f.audits(t, audit.ActionEnvironmentLocalFolded)
	if len(entries) != 1 || entries[0].ActorID != "id-admin" || entries[0].TargetID != f.env.ID || entries[0].IP != authIP {
		t.Fatalf("environment.local_folded = %+v", entries)
	}

	if rec := f.req(t, http.MethodPost, f.foldPath(), ""); rec.code != http.StatusConflict || rec.ptyp != "fold_not_offered" {
		t.Errorf("a second fold: %d type %q, want 409 fold_not_offered", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodPost, "/api/environments/0197b8a0-0000-7000-8000-000000000000/fold-local", ""); rec.code != http.StatusNotFound || rec.ptyp != "environment_not_found" {
		t.Errorf("an unknown environment: %d type %q, want 404 environment_not_found", rec.code, rec.ptyp)
	}
}

func TestTheNodeListOfAFoldedEnvironmentHasALocalSocketRowWithNoCertificateAndNoClock(t *testing.T) {
	f := newFoldFix(t, nil)
	if rec := f.req(t, http.MethodPost, f.foldPath(), ""); rec.code != http.StatusOK {
		t.Fatalf("fold: %d %s", rec.code, rec.body)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	var row map[string]any
	// The fold re-registers the socket, so its row reads as unprobed until the
	// next probe answers; wait for that rather than racing it.
	waitFor(t, "the folded host to be probed", func() bool {
		rec := f.req(t, http.MethodGet, "/api/env/"+f.env.ID+"/agents", "")
		if rec.code != http.StatusOK {
			t.Fatalf("agents: %d %s", rec.code, rec.body)
		}
		if err := json.Unmarshal(rec.body, &list); err != nil {
			t.Fatal(err)
		}
		row = nil
		for _, it := range list.Items {
			if it["role"] == "local" {
				row = it
			}
		}
		return row != nil && row["connected"] == true
	})
	if len(list.Items) != 2 {
		t.Fatalf("items = %v, want mgr-1 and the folded host", list.Items)
	}
	if row["node"] != "local-1" || row["status"] != "active" || row["connected"] != true || row["blocked"] != false {
		t.Errorf("local row = %v, want local-1, active, connected, not blocked", row)
	}
	for _, key := range []string{"certNotAfter", "certFingerprint", "revokedAt", "manualUpdateCommand"} {
		if v, ok := row[key]; ok && v != "" {
			t.Errorf("local row has %s = %v, want none", key, v)
		}
	}
	if row["renewalFailing"] != false || row["updateAvailable"] != false || row["clockOffsetSeconds"] != float64(0) {
		t.Errorf("local row = %v, want no renewal, update or clock to report", row)
	}
}
