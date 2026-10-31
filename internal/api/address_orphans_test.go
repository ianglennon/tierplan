// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
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
	labelled.Labels = map[string]string{"coaming.keep": "true"}
	f, e := dashEnv(t, orphanEngine(labelled))
	rec := f.unkeep(t, e, "labelled", "")
	if rec.code != http.StatusConflict || rec.ptyp != "kept_by_label" {
		t.Fatalf("unkeep: %d %q, want 409 kept_by_label: %s", rec.code, rec.ptyp, rec.body)
	}
	if !strings.Contains(string(rec.body), "Kept by the coaming.keep label on the network. Remove the label to stop keeping it.") {
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
	labelled.Labels = map[string]string{"coaming.keep": "true"}
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
