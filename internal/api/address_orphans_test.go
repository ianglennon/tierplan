// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
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

type keptRowJSON struct {
	Name       string   `json:"name"`
	Node       string   `json:"node"`
	NodeID     string   `json:"nodeId"`
	Subnets    []string `json:"subnets"`
	KeptBy     string   `json:"keptBy"`
	KeptByName string   `json:"keptByName"`
	KeptAt     string   `json:"keptAt"`
	Present    bool     `json:"present"`
}

type keptSpace struct {
	Orphans []orphanRow   `json:"orphans"`
	Kept    []keptRowJSON `json:"kept"`
}

func (f *envFix) keep(t *testing.T, e envBody, body string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/address-space/keeps", body)
}

func (f *envFix) unkeep(t *testing.T, e envBody, name, query string) *envRec {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	return f.req(t, http.MethodDelete, "/api/env/"+e.ID+"/address-space/keeps/"+name+query, "")
}

func (f *envFix) space(t *testing.T, e envBody) keptSpace {
	t.Helper()
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "address-space")
	if rec.code != http.StatusOK {
		t.Fatalf("address-space: %d %s", rec.code, rec.body)
	}
	return decode[keptSpace](t, rec)
}

func orphanNames(s keptSpace) []string {
	out := []string{}
	for _, o := range s.Orphans {
		out = append(out, o.Name)
	}
	return out
}

func (f *envFix) keepAudits(t *testing.T, action string) []auditRowFull {
	t.Helper()
	var out []auditRowFull
	for _, r := range f.networkAudits(t) {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

func TestKeepNetworks(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha"), idleNet("b1", "beta"), idleNet("c1", "gamma")))
	if got := orphanNames(f.space(t, e)); !slices.Equal(got, []string{"alpha", "beta", "gamma"}) {
		t.Fatalf("orphans = %v, want the three idle networks", got)
	}

	rec := f.keep(t, e, `{"targets":[{"name":"alpha"},{"name":"beta"}]}`)
	if rec.code != http.StatusOK || string(rec.body) == "" {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	if got := decode[struct {
		Kept int `json:"kept"`
	}](t, rec); got.Kept != 2 {
		t.Fatalf("kept = %d, want 2", got.Kept)
	}
	audits := f.keepAudits(t, audit.ActionNetworkKept)
	if len(audits) != 2 {
		t.Fatalf("network.kept entries = %+v, want one each", audits)
	}
	for i, name := range []string{"alpha", "beta"} {
		a := audits[i]
		if a.Details["name"] != name || a.Details["bulk"] != "true" || a.Details["node"] != e.ID || a.Result != string(audit.ResultSuccess) || a.EnvID != e.ID {
			t.Errorf("entry %d = %+v", i, a)
		}
	}
	// The list no longer has them, at once and without waiting for the report to age.
	s := f.space(t, e)
	if got := orphanNames(s); !slices.Equal(got, []string{"gamma"}) {
		t.Fatalf("orphans after the keep = %v, want only gamma", got)
	}
	if len(s.Kept) != 2 {
		t.Fatalf("kept = %+v", s.Kept)
	}

	// Keeping one of them again, with a new one, writes only the new.
	rec = f.keep(t, e, `{"targets":[{"name":"alpha"},{"name":"gamma","nodeId":"`+e.ID+`"}]}`)
	if got := decode[struct {
		Kept int `json:"kept"`
	}](t, rec); rec.code != http.StatusOK || got.Kept != 1 {
		t.Fatalf("second keep: %d %s", rec.code, rec.body)
	}
	if audits = f.keepAudits(t, audit.ActionNetworkKept); len(audits) != 3 {
		t.Fatalf("network.kept entries = %d, want 3: a network kept already records nothing", len(audits))
	}
	rec = f.keep(t, e, `{"targets":[{"name":"alpha"}]}`)
	if got := decode[struct {
		Kept int `json:"kept"`
	}](t, rec); got.Kept != 0 {
		t.Fatalf("a keep of a kept network = %d, want 0", got.Kept)
	}
	if audits = f.keepAudits(t, audit.ActionNetworkKept); len(audits) != 3 {
		t.Fatalf("a second keep wrote an audit entry: %d", len(audits))
	}
	if bad := f.keep(t, e, `{"targets":[]}`); bad.code != http.StatusUnprocessableEntity {
		t.Fatalf("no targets: %d, want 422", bad.code)
	}
}

// janitorKeeps reads the janitor_keeps rows of one environment straight from the table, so a test
// sees where a keep is stored and not what the API says about it.
func (f *envFix) janitorKeeps(t *testing.T, envID string) []keepRow {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT kind, node_id, ref, name, kept_by FROM janitor_keeps WHERE env_id = ? ORDER BY ref`, envID)
	if err != nil {
		t.Fatalf("read janitor_keeps: %v", err)
	}
	defer rows.Close()
	var out []keepRow
	for rows.Next() {
		var r keepRow
		if err := rows.Scan(&r.Kind, &r.NodeID, &r.Ref, &r.Name, &r.KeptBy); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

type keepRow struct{ Kind, NodeID, Ref, Name, KeptBy string }

// TestKeepNetworkLandsInJanitorKeeps: a network kept on the Address space page is one
// janitor_keeps row of kind network whose ref is the network's name, the kept list shows it, and
// stopping the keep removes the row (D-12, one table for every keep).
func TestKeepNetworkLandsInJanitorKeeps(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha"), idleNet("b1", "beta")))
	if got := f.janitorKeeps(t, e.ID); len(got) != 0 {
		t.Fatalf("janitor_keeps before any keep = %+v", got)
	}

	if rec := f.keep(t, e, `{"targets":[{"name":"alpha"}]}`); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	rows := f.janitorKeeps(t, e.ID)
	if len(rows) != 1 {
		t.Fatalf("janitor_keeps = %+v, want one row", rows)
	}
	if r := rows[0]; r.Kind != "network" || r.Ref != "alpha" || r.Name != "alpha" || r.KeptBy == "" {
		t.Fatalf("janitor_keeps row = %+v, want kind network with the network's name as ref", r)
	}
	s := f.space(t, e)
	if len(s.Kept) != 1 || s.Kept[0].Name != "alpha" {
		t.Fatalf("kept list = %+v, want alpha", s.Kept)
	}
	if got := orphanNames(s); !slices.Equal(got, []string{"beta"}) {
		t.Fatalf("orphans = %v, want only beta", got)
	}

	// A second keep of the same network keeps the first row.
	if rec := f.keep(t, e, `{"targets":[{"name":"alpha"}]}`); rec.code != http.StatusOK {
		t.Fatalf("second keep: %d %s", rec.code, rec.body)
	}
	if got := f.janitorKeeps(t, e.ID); len(got) != 1 || got[0] != rows[0] {
		t.Fatalf("janitor_keeps after a second keep = %+v, want the first row unchanged", got)
	}

	if rec := f.unkeep(t, e, "alpha", ""); rec.code != http.StatusNoContent {
		t.Fatalf("unkeep: %d %s", rec.code, rec.body)
	}
	if got := f.janitorKeeps(t, e.ID); len(got) != 0 {
		t.Fatalf("janitor_keeps after the unkeep = %+v, want none", got)
	}
	if s := f.space(t, e); len(s.Kept) != 0 {
		t.Fatalf("kept list after the unkeep = %+v", s.Kept)
	}
}

func TestUnkeepNetwork(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha")))
	if rec := f.keep(t, e, `{"targets":[{"name":"alpha"}]}`); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	if got := orphanNames(f.space(t, e)); len(got) != 0 {
		t.Fatalf("orphans = %v", got)
	}
	rec := f.unkeep(t, e, "alpha", "")
	if rec.code != http.StatusNoContent {
		t.Fatalf("unkeep: %d %s", rec.code, rec.body)
	}
	audits := f.keepAudits(t, audit.ActionNetworkUnkept)
	if len(audits) != 1 || audits[0].Details["name"] != "alpha" || audits[0].Result != string(audit.ResultSuccess) {
		t.Fatalf("network.unkept = %+v", audits)
	}
	s := f.space(t, e)
	if got := orphanNames(s); !slices.Equal(got, []string{"alpha"}) || len(s.Kept) != 0 {
		t.Fatalf("after the unkeep: orphans %v kept %+v, want alpha listed again", got, s.Kept)
	}
	if again := f.unkeep(t, e, "alpha", ""); again.code != http.StatusNotFound || again.ptyp != "keep_not_found" {
		t.Fatalf("a second unkeep: %d %q, want 404 keep_not_found", again.code, again.ptyp)
	}
	if n := len(f.keepAudits(t, audit.ActionNetworkUnkept)); n != 1 {
		t.Fatalf("network.unkept entries = %d, want 1", n)
	}
}

func TestUnkeepLabelKept(t *testing.T) {
	t.Parallel()
	labelled := idleNet("l1", "labelled")
	labelled.Labels = map[string]string{"tierplan.keep": "true"}
	f, e := dashEnv(t, orphanEngine(labelled))
	rec := f.unkeep(t, e, "labelled", "")
	if rec.code != http.StatusConflict || rec.ptyp != "kept_by_label" {
		t.Fatalf("unkeep: %d %q, want 409 kept_by_label: %s", rec.code, rec.ptyp, rec.body)
	}
	if !strings.Contains(string(rec.body), "Kept by the tierplan.keep label on the network. Remove the label to stop keeping it.") {
		t.Errorf("body = %s", rec.body)
	}
	if n := len(f.keepAudits(t, audit.ActionNetworkUnkept)); n != 0 {
		t.Fatalf("network.unkept entries = %d, want none", n)
	}
	if unknown := f.unkeep(t, e, "nothing-here", ""); unknown.code != http.StatusNotFound || unknown.ptyp != "keep_not_found" {
		t.Fatalf("a network that is not kept: %d %q", unknown.code, unknown.ptyp)
	}
}

func TestKeepNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha")))
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	awaitOnline(t, f.mon, e.ID)
	cookie, csrf := f.signIn(t, "operator", false)
	base := "/api/env/" + e.ID + "/address-space/"
	for name, r := range map[string]authReq{
		"keep":   {method: http.MethodPut, path: base + "keeps", body: `{"targets":[{"name":"alpha"}]}`},
		"unkeep": {method: http.MethodDelete, path: base + "keeps/alpha"},
		"delete": {method: http.MethodDelete, path: base + "orphans/a1"},
	} {
		r.cookie, r.csrf = cookie, csrf
		if rec := f.call(t, r); rec.Code != http.StatusForbidden {
			t.Errorf("%s as an Operator: %d, want 403: %s", name, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Errorf("writes = %v, want none", w)
	}
	if n := len(f.networkAudits(t)); n != 0 {
		t.Errorf("audit rows = %d, want none from a refused request", n)
	}
	if got := orphanNames(f.space(t, e)); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("orphans = %v: a refused keep changed the list", got)
	}
}

func TestAddressSpaceKeptList(t *testing.T) {
	t.Parallel()
	labelled := idleNet("l1", "labelled")
	labelled.Labels = map[string]string{"tierplan.keep": "true"}
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha"), labelled))
	if rec := f.keep(t, e, `{"targets":[{"name":"alpha"},{"name":"vanished","nodeId":"`+e.ID+`"}]}`); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	raw := f.get(t, e, "address-space")
	var generic struct {
		Kept []map[string]any `json:"kept"`
	}
	if err := json.Unmarshal(raw.body, &generic); err != nil {
		t.Fatal(err)
	}
	if generic.Kept == nil || len(generic.Kept) != 3 {
		t.Fatalf("kept = %v, want three rows\n%s", generic.Kept, f.logs)
	}
	if _, has := generic.Kept[1]["keptAt"]; has {
		t.Errorf("a label-kept row has no keptAt: %v", generic.Kept[1])
	}
	got := decode[keptSpace](t, raw).Kept
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "labelled" || got[2].Name != "vanished" {
		t.Fatalf("kept = %+v, want alpha, labelled and vanished by name", got)
	}
	a, l, v := got[0], got[1], got[2]
	if a.KeptBy != "user" || a.KeptByName != "admin" || !a.Present || a.Node != "node-1" || a.NodeID != e.ID || !slices.Equal(a.Subnets, []string{"172.30.0.0/24"}) {
		t.Errorf("alpha = %+v", a)
	}
	if _, err := time.Parse(time.RFC3339, a.KeptAt); err != nil {
		t.Errorf("alpha keptAt = %q: %v", a.KeptAt, err)
	}
	if l.KeptBy != "label" || l.KeptByName != "" || l.KeptAt != "" || !l.Present {
		t.Errorf("labelled = %+v", l)
	}
	if v.KeptBy != "user" || v.Present || v.Subnets == nil || len(v.Subnets) != 0 {
		t.Errorf("vanished = %+v, want a user keep with no network present", v)
	}
	if orphanNames(decode[keptSpace](t, raw)) == nil {
		t.Errorf("orphans must be an array")
	}
}

func (f *envFix) removedAudits(t *testing.T) []auditRowFull {
	return f.keepAudits(t, audit.ActionNetworkRemoved)
}

// noInspectSaved fails when a network.inspect_saved entry was written.
func (f *envFix) noInspectSaved(t *testing.T) {
	t.Helper()
	if n := len(f.keepAudits(t, audit.ActionNetworkInspectSaved)); n != 0 {
		t.Fatalf("%d network.inspect_saved entries, want none", n)
	}
}

func wantKept(t *testing.T, rec *envRec, reason, name string) {
	t.Helper()
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.code, rec.body)
	}
	if got := decode[orphanDeleteBody](t, rec); got.Outcome != "kept" || got.Reason != reason || got.Name != name {
		t.Fatalf("body = %+v, want kept/%s for %s", got, reason, name)
	}
}

func wantFailureAudit(t *testing.T, rows []auditRowFull, code string) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("network.removed entries = %+v, want exactly one", rows)
	}
	r := rows[0]
	if r.Result != string(audit.ResultFailure) || r.Reason != code || r.Details["code"] != code || r.Details["source"] != "orphan_cleanup" {
		t.Fatalf("network.removed = %+v, want a failure with code %s from orphan_cleanup", r, code)
	}
}

// Something attached after the list was read: the re-check sees it, nothing is sent.
func TestDeleteOrphanInUseAgain(t *testing.T) {
	t.Parallel()
	net := idleNet("idle1", "idle")
	cfg := orphanEngine(net)
	f, e := dashEnv(t, cfg)
	if got := orphanNames(f.space(t, e)); !slices.Equal(got, []string{"idle"}) {
		t.Fatalf("orphans = %v", got)
	}
	cfg.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/late"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"idle": {NetworkID: "idle1"}}}}
	f.fake.Reconfigure(cfg)

	wantKept(t, f.delOrphan(t, e, "idle1", ""), "in_use_again", "idle")
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none: the network is in use", w)
	}
	f.noInspectSaved(t)
	wantFailureAudit(t, f.removedAudits(t), "in_use_again")
}

// The engine itself refuses the remove as in use: a safety success, sent once.
func TestDeleteOrphanEngineRefusalIsKept(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine(idleNet("idle1", "idle"))
	cfg.RemoveErrors = map[string]string{"idle": "error while removing network: network idle id idle1 has active endpoints"}
	f, e := dashEnv(t, cfg)
	wantKept(t, f.delOrphan(t, e, "idle1", ""), "in_use_again", "idle")
	if w := f.fake.Writes(); !slices.Equal(w, []string{"DELETE /networks/idle1"}) {
		t.Fatalf("writes = %v, want exactly one remove request", w)
	}
	wantFailureAudit(t, f.removedAudits(t), "network_in_use")
	if n := len(f.keepAudits(t, audit.ActionNetworkInspectSaved)); n != 1 {
		t.Fatalf("network.inspect_saved entries = %d, want 1: it was saved before the remove was sent", n)
	}
}

// Its stack exists again, or it was kept meanwhile.
func TestDeleteOrphanNotOrphaned(t *testing.T) {
	t.Parallel()
	owned := idleNet("shop1", "shop_default")
	owned.Subnets = []string{"172.31.0.0/24"}
	owned.Labels = map[string]string{"com.docker.compose.project": "shop"}
	cfg := orphanEngine(owned, idleNet("keepme", "keepme"))
	f, e := dashEnv(t, cfg)
	s := f.space(t, e)
	if got := orphanNames(s); !slices.Equal(got, []string{"shop_default", "keepme"}) {
		t.Fatalf("orphans = %v, want the orphaned one and then the unused one", got)
	}

	// The project is deployed again: a container carries its label.
	cfg.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/shop-web-1"}, State: "running", Labels: map[string]string{"com.docker.compose.project": "shop"}}}
	f.fake.Reconfigure(cfg)
	wantKept(t, f.delOrphan(t, e, "shop1", ""), "not_orphaned", "shop_default")

	// Kept by a person after the list was read.
	if rec := f.keep(t, e, `{"targets":[{"name":"keepme"}]}`); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	wantKept(t, f.delOrphan(t, e, "keepme", ""), "not_orphaned", "keepme")
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	f.noInspectSaved(t)
	rows := f.removedAudits(t)
	if len(rows) != 2 || rows[0].Reason != "not_orphaned" || rows[1].Reason != "not_orphaned" {
		t.Fatalf("network.removed = %+v, want two not_orphaned failures", rows)
	}
}

// No manager answered, so TierPlan cannot tell whether anything on the swarm uses the network.
func TestDeleteOrphanNodeMissing(t *testing.T) {
	t.Parallel()
	s := swarmWith(t, dockertest.FakeNetwork{ID: "spare-id", Name: "spare", Driver: "overlay", Scope: "swarm", Created: longAgo})
	path := "/api/env/" + s.env.ID + "/address-space/orphans/spare-id"
	// With every manager answering the network is an orphan candidate and would be removed; with none, it is kept.
	s.m1.fake.LoseQuorum()
	s.m2.fake.LoseQuorum()
	rec := s.req(t, http.MethodDelete, path, "")
	wantKept(t, rec, "node_missing", "spare")
	s.noDeletes(t)
}

func TestDeleteOrphanGone(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("idle1", "idle")))
	rec := f.delOrphan(t, e, "nothing-here", "")
	if rec.code != http.StatusNotFound || rec.ptyp != "network_not_found" {
		t.Fatalf("%d %q, want 404 network_not_found: %s", rec.code, rec.ptyp, rec.body)
	}
	if n := len(f.networkAudits(t)); n != 0 || len(f.fake.Writes()) != 0 {
		t.Fatalf("a network that is not there was audited (%d rows) or written to (%v)", n, f.fake.Writes())
	}
}

func TestDeleteOrphanBuiltin(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine())
	rec := f.delOrphan(t, e, "br0", "")
	if rec.code != http.StatusConflict || rec.ptyp != "network_builtin" {
		t.Fatalf("%d %q, want 409 network_builtin: %s", rec.code, rec.ptyp, rec.body)
	}
	if len(f.fake.Writes()) != 0 {
		t.Fatalf("writes = %v, want none", f.fake.Writes())
	}
	f.noInspectSaved(t)
	wantFailureAudit(t, f.removedAudits(t), "network_builtin")
}

// Secret-like values are withheld in the saved document, everything else is exact.
func TestDeleteOrphanSavedInspectIsMaskedAndExact(t *testing.T) {
	t.Parallel()
	// The made-up credential in a URL is put together here, as a URL with user-info is what the masking is for.
	dsn := "https://user:" + "pass@db.example/x"
	n := idleNet("idle1", "idle")
	n.Labels = map[string]string{"password": "hunter2", "dsn": dsn, "note": "a\x7fb<&>", "tier": "backend"}
	n.Options = map[string]string{"token": "tok-123", "com.docker.network.driver.mtu": "1400"}
	f, e := dashEnv(t, orphanEngine(n))
	if rec := f.delOrphan(t, e, "idle1", ""); rec.code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.code, rec.body)
	}
	saved := f.keepAudits(t, audit.ActionNetworkInspectSaved)
	if len(saved) != 1 || saved[0].Details["masked"] != "true" {
		t.Fatalf("network.inspect_saved = %+v, want one, masked", saved)
	}
	raw := saved[0].Details["inspect"]
	for _, secret := range []string{"hunter2", "tok-123", "user:pass"} {
		if strings.Contains(raw, secret) {
			t.Errorf("the saved inspect holds %q: %s", secret, raw)
		}
	}
	var doc struct {
		Labels, Options map[string]string
		ID              string `json:"Id"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if doc.Labels["password"] != "<hidden>" || doc.Options["token"] != "<hidden>" || doc.Labels["dsn"] == dsn {
		t.Errorf("labels = %v options = %v, want the secrets withheld", doc.Labels, doc.Options)
	}
	if doc.Labels["note"] != "a\x7fb<&>" || doc.Labels["tier"] != "backend" || doc.Options["com.docker.network.driver.mtu"] != "1400" || doc.ID != "idle1" {
		t.Errorf("labels = %v options = %v id = %q, want every other value exactly as the engine sent it", doc.Labels, doc.Options, doc.ID)
	}
}

// A document the audit log cannot store whole refuses the delete (D-12).
func TestDeleteOrphanInspectTooLarge(t *testing.T) {
	t.Parallel()
	n := idleNet("idle1", "idle")
	n.Labels = map[string]string{}
	for i := range 300 {
		n.Labels[fmt.Sprintf("label-%03d", i)] = strings.Repeat("x", 250)
	}
	f, e := dashEnv(t, orphanEngine(n))
	rec := f.delOrphan(t, e, "idle1", "")
	if rec.code != http.StatusConflict || rec.ptyp != "inspect_too_large" {
		t.Fatalf("%d %q, want 409 inspect_too_large: %s", rec.code, rec.ptyp, rec.body)
	}
	if !strings.Contains(string(rec.body), "TierPlan didn't delete idle: its details are over 64 KB, too large to save in the audit log first.") {
		t.Errorf("body = %s", rec.body)
	}
	if len(f.fake.Writes()) != 0 {
		t.Fatalf("writes = %v, want none: the remove must not be sent", f.fake.Writes())
	}
	f.noInspectSaved(t)
	wantFailureAudit(t, f.removedAudits(t), "inspect_too_large")
}

func TestDeleteOrphanNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("idle1", "idle")))
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	awaitOnline(t, f.mon, e.ID)
	cookie, csrf := f.signIn(t, "operator", false)
	rec := f.call(t, authReq{method: http.MethodDelete, path: "/api/env/" + e.ID + "/address-space/orphans/idle1", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if len(f.fake.Writes()) != 0 || len(f.networkAudits(t)) != 0 {
		t.Fatalf("a refused request wrote %v or audited %d rows", f.fake.Writes(), len(f.networkAudits(t)))
	}
}

// The refactor that lets the janitor remove a network by the same code changed nothing for the
// Address space page: the route answers as before and every audit row still says orphan_cleanup.
func TestDeleteOrphanSourceUnchanged(t *testing.T) {
	t.Parallel()
	cfg := orphanEngine(idleNet("idle1", "idle"), idleNet("idle2", "busy"))
	f, e := dashEnv(t, cfg)
	if got := orphanNames(f.space(t, e)); !slices.Equal(got, []string{"busy", "idle"}) {
		t.Fatalf("orphans = %v", got)
	}
	rec := f.delOrphan(t, e, "idle1", "")
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	if got := decode[orphanDeleteBody](t, rec); got.Outcome != "deleted" || got.Reason != "" || got.Name != "idle" {
		t.Fatalf("body = %+v, want deleted idle", got)
	}
	// Something attached to the other after the list was read: kept, with the same body as ever.
	cfg.Containers = []dockertest.FakeContainer{{ID: "c1", Names: []string{"/late"}, State: "running", Networks: map[string]dockertest.FakeEndpoint{"busy": {NetworkID: "idle2"}}}}
	f.fake.Reconfigure(cfg)
	wantKept(t, f.delOrphan(t, e, "idle2", ""), "in_use_again", "busy")

	rows := f.networkAudits(t)
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.Action+" "+r.Result)
		if r.Details["source"] != "orphan_cleanup" {
			t.Errorf("%s of %s has source %q, want orphan_cleanup", r.Action, r.Details["name"], r.Details["source"])
		}
	}
	want := []string{
		audit.ActionNetworkInspectSaved + " success", audit.ActionNetworkRemoved + " success", audit.ActionNetworkRemoved + " failure",
	}
	if !slices.Equal(actions, want) {
		t.Fatalf("audit = %v, want %v", actions, want)
	}
}
