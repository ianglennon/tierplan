// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// The orphan delete and the keeps (plan 10-12, NET-04, D-11, D-12). Docker is a fake daemon behind
// the real monitor, aggregator and address reader; the router, policy and audit log are the real ones.

type orphanDeleteBody struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Name    string `json:"name"`
}

var longAgo = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// orphanEngine is a standalone host with an unused network and the engine's bridge.
func orphanEngine(nets ...dockertest.FakeNetwork) dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Networks: append([]dockertest.FakeNetwork{
			{ID: "br0", Name: "bridge", Driver: "bridge", Scope: "local", Created: longAgo},
		}, nets...),
	}
}

func idleNet(id, name string) dockertest.FakeNetwork {
	return dockertest.FakeNetwork{ID: id, Name: name, Driver: "bridge", Scope: "local", Subnets: []string{"172.30.0.0/24"}, Created: longAgo}
}

// delOrphan sends the delete as the signed-in admin once the monitor has seen the node.
func (f *envFix) delOrphan(t *testing.T, e envBody, id, query string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodDelete, "/api/env/"+e.ID+"/address-space/orphans/"+id+query, "")
}

func TestDeleteOrphanUnused(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("idle1", "idle")))
	rec := f.delOrphan(t, e, "idle1", "")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got := decode[orphanDeleteBody](t, rec); got.Outcome != "deleted" || got.Reason != "" || got.Name != "idle" {
		t.Fatalf("body = %+v, want deleted idle", got)
	}
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/idle1"}) {
		t.Fatalf("writes = %v, want exactly one remove of the network", w)
	}
	rows := f.networkAudits(t)
	if len(rows) != 2 || rows[0].Action != audit.ActionNetworkInspectSaved || rows[1].Action != audit.ActionNetworkRemoved {
		t.Fatalf("audit = %+v, want network.inspect_saved and then network.removed", rows)
	}
	saved, removed := rows[0], rows[1]
	if saved.Result != string(audit.ResultSuccess) || saved.TargetID != "idle1" || saved.EnvID != e.ID || saved.Details["name"] != "idle" ||
		saved.Details["source"] != "orphan_cleanup" || saved.Details["masked"] != "false" || saved.Details["node"] != e.ID {
		t.Errorf("inspect_saved = %+v", saved)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(saved.Details["inspect"]), &doc); err != nil {
		t.Fatalf("the saved inspect is not JSON: %v: %q", err, saved.Details["inspect"])
	}
	if doc["Name"] != "idle" || doc["Id"] != "idle1" || doc["Driver"] != "bridge" || doc["Scope"] != "local" {
		t.Errorf("the saved inspect = %v, want the fake's inspect document", doc)
	}
	if removed.Result != string(audit.ResultSuccess) || removed.TargetID != "idle1" ||
		removed.Details["source"] != "orphan_cleanup" || removed.Details["reason"] != "unused" || removed.Details["code"] != "removed" || removed.Details["name"] != "idle" {
		t.Errorf("removed = %+v", removed)
	}
}
