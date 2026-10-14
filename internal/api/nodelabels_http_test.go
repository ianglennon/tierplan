// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Node labels with their impact (plan 06-16, RES-05, D-21, E15). Docker is a fake daemon behind the
// real monitor, aggregator and cluster reader; the router, policy, CSRF guard and audit writer are
// the real ones. The swarm is the one of nodeactions_test.go: wrk-1 (node-4) has disk=ssd and
// zone=a, the db service needs node.labels.disk==ssd and runs on it.

type nlRow struct {
	ServiceID  string `json:"serviceId"`
	Service    string `json:"service"`
	Constraint string `json:"constraint"`
	Before     bool   `json:"before"`
	After      bool   `json:"after"`
}

type nlBody struct {
	Version         uint64  `json:"version"`
	Rows            []nlRow `json:"rows"`
	NoLongerMatches int     `json:"noLongerMatches"`
	TasksMayMove    int     `json:"tasksMayMove"`
	Added           int     `json:"added"`
	Changed         int     `json:"changed"`
	Removed         int     `json:"removed"`
}

func (f *envFix) labelImpact(t *testing.T, e envBody, node, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/nodes/"+node+"/labels/impact", body)
}

func (f *envFix) setLabels(t *testing.T, e envBody, node, body string) *envRec {
	t.Helper()
	return f.req(t, http.MethodPut, "/api/env/"+e.ID+"/nodes/"+node+"/labels", body)
}

func TestNodeLabelImpactEndpoint(t *testing.T) {
	f, e := naEnv(t, nil)

	rec := f.labelImpact(t, e, "node-4", `{"labels":{"zone":"a"}}`)
	if rec.code != http.StatusOK {
		t.Fatalf("impact = %d: %s", rec.code, rec.body)
	}
	b := decode[nlBody](t, rec)
	if b.Version != 3 {
		t.Errorf("version = %d, want the node's own, 3: the version a save made on the strength of this must carry", b.Version)
	}
	if len(b.Rows) != 1 || b.Rows[0].Service != "db" || b.Rows[0].ServiceID != "svc-db" || b.Rows[0].Constraint != "node.labels.disk==ssd" || !b.Rows[0].Before || b.Rows[0].After {
		t.Fatalf("rows = %+v, want db's disk constraint, met before and not after", b.Rows)
	}
	if b.NoLongerMatches != 1 || b.TasksMayMove != 1 || b.Removed != 1 || b.Added != 0 || b.Changed != 0 {
		t.Errorf("body = %+v, want one service that no longer matches, db-1 that may move, one label removed", b)
	}

	// Labels nobody's constraint names change nothing for placement.
	b = decode[nlBody](t, f.labelImpact(t, e, "node-4", `{"labels":{"disk":"ssd","zone":"a","owner":"me"}}`))
	if b.Rows == nil || len(b.Rows) != 0 || b.TasksMayMove != 0 || b.Added != 1 {
		t.Errorf("body = %+v, want an empty (never null) list and one added", b)
	}
	if string(f.labelImpact(t, e, "node-4", `{"labels":{}}`).body) == "" {
		t.Error("an empty label set has an answer")
	}

	// A node that has no ssd now matches db once it has it.
	b = decode[nlBody](t, f.labelImpact(t, e, "node-5", `{"labels":{"disk":"ssd"}}`))
	if len(b.Rows) != 1 || b.Rows[0].Before || !b.Rows[0].After || b.NoLongerMatches != 0 || b.TasksMayMove != 0 || b.Added != 1 {
		t.Errorf("body = %+v, want db's constraint now met and nothing that moves", b)
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none: the impact writes nothing", ups)
	}
	if rows := f.nodeAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a read", rows)
	}
}

func TestNodeLabelImpactRefusals(t *testing.T) {
	f, e := naEnv(t, nil)
	if rec := f.labelImpact(t, e, "node-4", `{"labels":{"com.docker.x":"1"}}`); rec.code != http.StatusUnprocessableEntity || rec.ptyp != "label_reserved" {
		t.Errorf("a reserved label = %d %s (%s), want 422 label_reserved", rec.code, rec.body, rec.ptyp)
	}
	if rec := f.labelImpact(t, e, "node-4", `{}`); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("no labels field = %d %s, want 422", rec.code, rec.body)
	}
	if rec := f.labelImpact(t, e, "ghost", `{"labels":{}}`); rec.code != http.StatusNotFound || rec.ptyp != "node_not_found" {
		t.Errorf("an unknown node = %d %s (%s), want 404 node_not_found", rec.code, rec.body, rec.ptyp)
	}
	f.addUser(t, "nl-operator", false)
	f.addEnvRole(t, "nl-operator", e.ID, "operator")
	cookie, csrf := f.signIn(t, "nl-operator", false)
	rec := f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/nodes/node-4/labels/impact", body: `{"labels":{}}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's impact = %d: %s, want 403: label changes need Admin", rec.Code, rec.Body)
	}

	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return sf.mon.Status(se.ID).State == envs.StateOnline })
	if rec := sf.labelImpact(t, se, "node-1", `{"labels":{}}`); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("a standalone host = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}

func TestSetNodeLabelsRefusals(t *testing.T) {
	f, e := naEnv(t, nil)
	labelsOf := func(n int) string {
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf("%q:%q", fmt.Sprintf("k%d", i), "v"))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	body := func(labels string) string { return `{"labels":` + labels + `,"version":3}` }

	for name, tc := range map[string]struct {
		node, body, ptyp string
		code             int
	}{
		"com.docker.":                    {"node-4", body(`{"com.docker.swarm.x":"1"}`), "label_reserved", http.StatusUnprocessableEntity},
		"mixed case com.docker.":         {"node-4", body(`{"COM.Docker.x":"1"}`), "label_reserved", http.StatusUnprocessableEntity},
		"io.docker.":                     {"node-4", body(`{"io.docker.y":"1"}`), "label_reserved", http.StatusUnprocessableEntity},
		"org.dockerproject.":             {"node-4", body(`{"org.dockerproject.z":"1"}`), "label_reserved", http.StatusUnprocessableEntity},
		"an empty key":                   {"node-4", body(`{"":"1"}`), "label_invalid", http.StatusUnprocessableEntity},
		"a key of 257 bytes":             {"node-4", body(`{"` + strings.Repeat("k", 257) + `":"1"}`), "label_invalid", http.StatusUnprocessableEntity},
		"a key of 129 two-byte runes":    {"node-4", body(`{"` + strings.Repeat("é", 129) + `":"1"}`), "label_invalid", http.StatusUnprocessableEntity},
		"a value of 4097 bytes":          {"node-4", body(`{"a":"` + strings.Repeat("v", 4097) + `"}`), "label_invalid", http.StatusUnprocessableEntity},
		"a value of 2049 two-byte runes": {"node-4", body(`{"a":"` + strings.Repeat("é", 2049) + `"}`), "label_invalid", http.StatusUnprocessableEntity},
		"65 labels":                      {"node-4", body(labelsOf(65)), "label_invalid", http.StatusUnprocessableEntity},
		"no labels field":                {"node-4", `{"version":3}`, "", http.StatusUnprocessableEntity},
		"no version":                     {"node-4", `{"labels":{}}`, "", http.StatusUnprocessableEntity},
		"an unknown field":               {"node-4", `{"labels":{},"version":3,"role":"manager"}`, "", http.StatusUnprocessableEntity},
		"a stale version":                {"node-4", `{"labels":{"a":"1"},"version":2}`, "node_changed", http.StatusConflict},
		"an unknown node":                {"ghost", `{"labels":{"a":"1"},"version":3}`, "node_not_found", http.StatusNotFound},
	} {
		rec := f.setLabels(t, e, tc.node, tc.body)
		if rec.code != tc.code || (tc.ptyp != "" && rec.ptyp != tc.ptyp) {
			t.Errorf("%s = %d %s (%s), want %d %s", name, rec.code, rec.body, rec.ptyp, tc.code, tc.ptyp)
		}
	}
	if ups := f.fake.NodeUpdates(); len(ups) != 0 {
		t.Errorf("updates = %+v, want none: a refusal is made before any write and a stale version is never retried", ups)
	}
	var reserved struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(f.setLabels(t, e, "node-4", body(`{"com.docker.x":"1"}`)).body, &reserved); err != nil {
		t.Fatal(err)
	}
	if reserved.Detail != "Labels starting with com.docker. are reserved." {
		t.Errorf("detail = %q", reserved.Detail)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionNodeLabelsChanged || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "node_changed" || rows[0].TargetID != "node-4" {
		t.Errorf("audit rows = %+v, want one failure node_changed: the stale save is the one attempt on a node that exists", rows)
	}

	// The limits themselves are allowed: 64 labels, a 256-byte key, a 4096-byte value.
	ok := fmt.Sprintf(`{"labels":%s,"version":3}`, labelsOf(64))
	if rec := f.setLabels(t, e, "node-4", ok); rec.code != http.StatusNoContent {
		t.Fatalf("64 labels = %d %s, want 204", rec.code, rec.body)
	}
	ok = `{"labels":{"` + strings.Repeat("é", 128) + `":"` + strings.Repeat("v", 4096) + `"},"version":4}`
	if rec := f.setLabels(t, e, "node-4", ok); rec.code != http.StatusNoContent {
		t.Fatalf("a 256-byte key and a 4096-byte value = %d %s, want 204", rec.code, rec.body)
	}
	// Docker accepts com.dockerish, which is not the reserved prefix.
	if rec := f.setLabels(t, e, "node-4", `{"labels":{"com.dockerish":"1"},"version":5}`); rec.code != http.StatusNoContent {
		t.Fatalf("com.dockerish = %d %s, want 204", rec.code, rec.body)
	}
}

func TestSetNodeLabels(t *testing.T) {
	f, e := naEnv(t, nil)
	rec := f.setLabels(t, e, "node-4", `{"labels":{"zone":"b","owner":"me"},"version":3}`)
	if rec.code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	ups := f.fake.NodeUpdates()
	if len(ups) != 1 {
		t.Fatalf("updates = %+v, want exactly one", ups)
	}
	u := ups[0]
	if u.NodeID != "node-4" || u.Version != "3" || u.Spec.Role != "worker" || u.Spec.Availability != "active" ||
		len(u.Spec.Labels) != 2 || u.Spec.Labels["zone"] != "b" || u.Spec.Labels["owner"] != "me" {
		t.Errorf("update = %+v, want the whole spec at version 3 with only the labels replaced (disk gone, zone changed, owner added)", u)
	}

	rows := f.nodeAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionNodeLabelsChanged || r.Result != string(audit.ResultSuccess) || r.TargetType != "node" || r.TargetID != "node-4" || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"name": "wrk-1", "added": "1", "changed": "1", "removed": "1", "affected": "1", "code": "done"} {
		if got, ok := r.Details[k]; !ok || got != want {
			t.Errorf("detail %s = %q (present %v), want %q", k, got, ok, want)
		}
	}

	// The version moved on, so the same save again is stale; an empty map clears every label.
	if again := f.setLabels(t, e, "node-4", `{"labels":{"zone":"b","owner":"me"},"version":3}`); again.code != http.StatusConflict || again.ptyp != "node_changed" {
		t.Errorf("the same version again = %d %s (%s), want 409 node_changed", again.code, again.body, again.ptyp)
	}
	if rec := f.setLabels(t, e, "node-4", `{"labels":{},"version":4}`); rec.code != http.StatusNoContent {
		t.Fatalf("clearing = %d: %s", rec.code, rec.body)
	}
	ups = f.fake.NodeUpdates()
	if len(ups) != 2 || len(ups[1].Spec.Labels) != 0 {
		t.Errorf("updates = %+v, want a second that leaves no labels", ups)
	}
}

func TestSetNodeLabelsNeedsAdminAndMapsDockersRefusals(t *testing.T) {
	f, e := naEnv(t, func(c *dockertest.FakeConfig) {
		c.NodeUpdateError = "rpc error: code = Unavailable desc = no leader elected"
	})
	f.addUser(t, "nl-operator", false)
	f.addEnvRole(t, "nl-operator", e.ID, "operator")
	cookie, csrf := f.signIn(t, "nl-operator", false)
	rec := f.call(t, authReq{method: http.MethodPut, path: "/api/env/" + e.ID + "/nodes/node-4/labels", body: `{"labels":{"a":"1"},"version":3}`, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusForbidden {
		t.Errorf("an Operator's save = %d: %s, want 403", rec.Code, rec.Body)
	}
	if rows := f.nodeAudits(t); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a refused caller", rows)
	}

	got := f.setLabels(t, e, "node-4", `{"labels":{"a":"1"},"version":3}`)
	if got.code != http.StatusBadGateway || got.ptyp != "node_update_failed" || !strings.Contains(string(got.body), "no leader elected") {
		t.Errorf("status = %d %s (%s), want 502 node_update_failed with Docker's words", got.code, got.body, got.ptyp)
	}
	rows := f.nodeAudits(t)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Reason != "node_update_failed" {
		t.Errorf("audit rows = %+v, want one failure node_update_failed", rows)
	}
}

func TestNodeLabelsOnAStandaloneHostAreNotASwarm(t *testing.T) {
	sf := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1", Version: "29.1.0"})
	sf.clusters = envs.NewClusters(sf.svc, sf.snaps, envs.ClusterOptions{Now: sf.clk.now})
	sf.svc.WithClusters(sf.clusters)
	sf.h = sf.build(t)
	se := sf.add(t)
	waitFor(t, "the monitor's first probe", func() bool { return sf.mon.Status(se.ID).State == envs.StateOnline })
	if rec := sf.setLabels(t, se, "node-1", `{"labels":{},"version":1}`); rec.code != http.StatusConflict || rec.ptyp != "not_a_swarm" {
		t.Errorf("labels = %d %s (%s), want 409 not_a_swarm", rec.code, rec.body, rec.ptyp)
	}
}
