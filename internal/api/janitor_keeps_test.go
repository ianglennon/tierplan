// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/api"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/proto/testsupport"
)

// Keeping anything out of cleanup from the Janitor page (plan 11-27, JAN-07, JAN-06, D-12, D-21). The
// engines are fake daemons behind the real monitor and aggregator; the routes, the planner, the runner
// and the audit log are the real ones.

// keepOne is the body of a keep for one object.
func keepOne(kind, nodeID, ref string) string {
	return fmt.Sprintf(`{"targets":[{"kind":%q,"nodeId":%q,"ref":%q}]}`, kind, nodeID, ref)
}

// keepsURL is the keep route of an environment.
func keepsURL(envID string) string { return "/api/env/" + envID + "/janitor/keeps" }

// notRemovedRef finds an item the preview left, by its ref.
func notRemovedRef(p janitorPreview, ref string) (janitorItem, bool) {
	for _, it := range p.NotRemoved {
		if it.Ref == ref {
			return it, true
		}
	}
	return janitorItem{}, false
}

// The tracer: a stale task container kept from the Janitor page is listed protected, and neither a
// scheduled run (All) nor Clean now removes it.
func TestJanitorKeepContainerProtects(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, before := s.previewJanitor(t)
	if len(before.Items) != 3 || before.Items[0].Ref != "web-x1" {
		t.Fatalf("preview items = %+v, want web-x1 to web-x3 removable", before.Items)
	}
	victim := before.Items[0]

	rec := s.req(t, http.MethodPut, keepsURL(s.env.ID), keepOne("container", victim.NodeID, victim.Ref))
	if rec.code != http.StatusOK || decode[struct {
		Kept int `json:"kept"`
	}](t, rec).Kept != 1 {
		t.Fatalf("keep: %d %s, want 200 with one new keep", rec.code, rec.body)
	}

	_, after := s.previewJanitor(t)
	var refs []string
	for _, it := range after.Items {
		refs = append(refs, it.Ref)
	}
	if !slices.Equal(refs, []string{"web-x2", "web-x3"}) {
		t.Fatalf("preview items after the keep = %v, want web-x2 and web-x3", refs)
	}
	it, ok := notRemovedRef(after, "web-x1")
	if !ok || it.Class != "protected" || it.Reason.Code != "kept_row" || it.Type != "container" {
		t.Fatalf("web-x1 = %+v (listed %v), want a protected container with reason kept_row", it, ok)
	}

	// A scheduled run removes everything removable on its own fresh plan, and leaves the kept container.
	runner := &janitor.Runner{
		Engine: api.NewJanitorEngine(s.agg, s.clusters, nil, janitor.KeepStore{St: s.st}, nil), Locks: s.stackLocks, Jobs: s.jobHub,
		Clock: nowClock(s.clk.now), Store: &janitor.SQLStore{St: s.st}, Audit: s.aud, Policy: janitor.PolicyStore{St: s.st}.Effective,
		Nodes: janitor.PolicyStore{St: s.st}.Nodes,
	}
	started, err := runner.Start(testsupport.Context(t), janitor.RunRequest{
		EnvID: s.env.ID, EnvName: "swarm", Trigger: janitor.TriggerSchedule, Actor: "system:janitor", Owner: "system:janitor:" + s.env.ID, All: true,
	})
	if err != nil {
		t.Fatalf("Runner.Start: %v", err)
	}
	s.awaitRun(t, started.JobID)
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/web-x2", "DELETE /containers/web-x3"}) {
		t.Fatalf("the daemons saw %v, want web-x2 and web-x3 deleted and web-x1 left", got)
	}
}

type keptItemJSON struct {
	Kind       string `json:"kind"`
	NodeID     string `json:"nodeId"`
	Node       string `json:"node"`
	Ref        string `json:"ref"`
	Name       string `json:"name"`
	KeptBy     string `json:"keptBy"`
	KeptByName string `json:"keptByName"`
	KeptAt     string `json:"keptAt"`
	Present    bool   `json:"present"`
}

type keptListJSON struct {
	Items []keptItemJSON `json:"items"`
}

type keepCount struct {
	Kept int `json:"kept"`
}

// unkeepURL is the stop-keeping route for one object.
func unkeepURL(envID, kind, nodeID, ref string) string {
	return keepsURL(envID) + "?kind=" + kind + "&nodeId=" + nodeID + "&ref=" + ref
}

// keptList reads the janitor's kept list as the fixture's admin.
func (f *envFix) keptList(t *testing.T, envID string) []keptItemJSON {
	t.Helper()
	rec := f.req(t, http.MethodGet, keepsURL(envID), "")
	if rec.code != http.StatusOK {
		t.Fatalf("kept list: %d %s", rec.code, rec.body)
	}
	return decode[keptListJSON](t, rec).Items
}

// janitorKeepAudits reads the janitor.kept and janitor.unkept rows of the audit log.
func janitorKeepAudits(t *testing.T, db *sql.DB, action string) []auditRowFull {
	t.Helper()
	rows, err := db.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []auditRowFull
	for rows.Next() {
		var r auditRowFull
		var details string
		if err := rows.Scan(&r.Action, &r.TargetType, &r.TargetID, &r.EnvID, &r.Result, &r.Reason, &details); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(details), &r.Details); err != nil {
			t.Fatalf("details %q: %v", details, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// danglingImage is an image the janitor would remove: untagged, undigested, old and unused.
func danglingImage(digit string) dockertest.FakeImage {
	return dockertest.FakeImage{ID: imageIDOf(digit), Size: 4096, Created: imageAge(72 * time.Hour)}
}

// itemOf finds an item of the preview's items by its ref.
func itemOf(items []janitorItem, ref string) (janitorItem, bool) {
	for _, it := range items {
		if it.Ref == ref {
			return it, true
		}
	}
	return janitorItem{}, false
}

// Keeping an image protects it (kept_row) and stopping the keep makes it removable again; both are audited,
// each once, with the kind and the image's name.
func TestJanitorKeepImageThenUnkeep(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := agedDanglingEnv(t, janitorImageEngine(danglingImage("a")))
	if p := f.previewJanitor(t, e); len(p.Items) != 1 || p.Items[0].Ref != id {
		t.Fatalf("preview items = %+v, want the dangling image", p.Items)
	}

	rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("image", e.ID, id))
	if rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 1 {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	p := f.previewJanitor(t, e)
	it, ok := itemOf(p.NotRemoved, id)
	if len(p.Items) != 0 || !ok || it.Class != "protected" || it.Reason.Code != "kept_row" || it.Type != "image" {
		t.Fatalf("preview after the keep: items %+v, image %+v (listed %v), want it protected kept_row", p.Items, it, ok)
	}
	if got := f.keptList(t, e.ID); len(got) != 1 || got[0].Kind != "image" || got[0].Ref != id || got[0].KeptBy != "user" || !got[0].Present || got[0].KeptAt == "" {
		t.Fatalf("kept list = %+v, want the image kept by a user and present", got)
	}
	kept := janitorKeepAudits(t, f.st.Read, "janitor.kept")
	if len(kept) != 1 || kept[0].TargetType != "image" || kept[0].TargetID != id || kept[0].EnvID != e.ID || kept[0].Result != "success" ||
		kept[0].Details["kind"] != "image" || kept[0].Details["name"] != id[len("sha256:"):len("sha256:")+12] || kept[0].Details["bulk"] != "" {
		t.Fatalf("janitor.kept = %+v", kept)
	}

	if rec := f.req(t, http.MethodDelete, unkeepURL(e.ID, "image", e.ID, id), ""); rec.code != http.StatusNoContent {
		t.Fatalf("unkeep: %d %s", rec.code, rec.body)
	}
	if after := f.previewJanitor(t, e); len(after.Items) != 1 || after.Items[0].Ref != id || after.Items[0].Class != "removable" {
		t.Fatalf("preview after the unkeep: %+v, want the image removable again", after.Items)
	}
	if got := f.keptList(t, e.ID); len(got) != 0 {
		t.Fatalf("kept list after the unkeep = %+v", got)
	}
	if un := janitorKeepAudits(t, f.st.Read, "janitor.unkept"); len(un) != 1 || un[0].TargetID != id || un[0].Details["kind"] != "image" {
		t.Fatalf("janitor.unkept = %+v", un)
	}
}

// One table: a network kept from the janitor shows on the Address space page's kept list, and one kept on
// the Address space page shows in the janitor's list, kept by the same user.
func TestJanitorKeepSharedWithAddressSpace(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, orphanEngine(idleNet("a1", "alpha"), idleNet("b1", "beta")))
	awaitOnline(t, f.mon, e.ID)

	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("network", "", "alpha")); rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 1 {
		t.Fatalf("keep from the janitor: %d %s", rec.code, rec.body)
	}
	if s := f.space(t, e); len(s.Kept) != 1 || s.Kept[0].Name != "alpha" || s.Kept[0].KeptBy != "user" {
		t.Fatalf("Address space kept list = %+v, want alpha", s.Kept)
	}
	if rows := f.janitorKeeps(t, e.ID); len(rows) != 1 || rows[0].Kind != "network" || rows[0].Ref != "alpha" || rows[0].Name != "alpha" {
		t.Fatalf("janitor_keeps = %+v, want one network row named alpha", rows)
	}

	if rec := f.keep(t, e, `{"targets":[{"name":"beta"}]}`); rec.code != http.StatusOK {
		t.Fatalf("keep from Address space: %d %s", rec.code, rec.body)
	}
	got := f.keptList(t, e.ID)
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("janitor kept list = %+v, want alpha and beta", got)
	}
	for _, it := range got {
		if it.Kind != "network" || it.KeptBy != "user" || it.KeptByName == "" || !it.Present || it.NodeID != e.ID || it.Node == "" {
			t.Errorf("kept item = %+v, want a network kept by a user, present, on its node", it)
		}
	}
	// The planner honours it: neither network is in the janitor's plan.
	if p := f.previewJanitor(t, e); len(p.Items) != 0 {
		t.Errorf("preview items = %+v, want the kept networks left", p.Items)
	}

	// Stopping the keep on the janitor's route clears the Address space page's list too.
	if rec := f.req(t, http.MethodDelete, unkeepURL(e.ID, "network", "", "beta"), ""); rec.code != http.StatusNoContent {
		t.Fatalf("unkeep from the janitor: %d %s", rec.code, rec.body)
	}
	if s := f.space(t, e); len(s.Kept) != 1 || s.Kept[0].Name != "alpha" {
		t.Fatalf("Address space kept list after the unkeep = %+v, want only alpha", s.Kept)
	}
}

// An object kept only by its tierplan.keep label has no row to remove: 409 kept_by_label, for every kind.
func TestJanitorUnkeepLabelKept(t *testing.T) {
	t.Parallel()
	label := map[string]string{"tierplan.keep": "true"}
	net := idleNet("l1", "labelled")
	net.Labels = label
	img := danglingImage("c")
	img.Labels = label
	cfg := janitorImageEngine(img)
	cfg.Networks = append(cfg.Networks, net)
	cfg.Containers = []dockertest.FakeContainer{{ID: "lab-ctr", Names: []string{"/lab"}, Image: "nginx:1", State: "exited", Created: janitorNow.Add(-72 * time.Hour).Unix(), Labels: label}}
	f, e := dashEnv(t, cfg)
	awaitOnline(t, f.mon, e.ID)

	for _, c := range []struct{ kind, node, ref string }{
		{"container", e.ID, "lab-ctr"}, {"image", e.ID, imageIDOf("c")}, {"network", "", "labelled"},
	} {
		rec := f.req(t, http.MethodDelete, unkeepURL(e.ID, c.kind, c.node, c.ref), "")
		if rec.code != http.StatusConflict || rec.ptyp != "kept_by_label" {
			t.Errorf("unkeep %s: %d %q, want 409 kept_by_label: %s", c.kind, rec.code, rec.ptyp, rec.body)
		}
	}
	if n := len(janitorKeepAudits(t, f.st.Read, "janitor.unkept")); n != 0 {
		t.Errorf("janitor.unkept rows = %d, want none from a refused request", n)
	}
}

// Stopping a keep that is not there is 404 keep_not_found and writes nothing.
func TestJanitorUnkeepMissing(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, janitorImageEngine(danglingImage("a")))
	awaitOnline(t, f.mon, e.ID)
	rec := f.req(t, http.MethodDelete, unkeepURL(e.ID, "image", e.ID, imageIDOf("a")), "")
	if rec.code != http.StatusNotFound || rec.ptyp != "keep_not_found" {
		t.Fatalf("unkeep of an image that is not kept: %d %q, want 404 keep_not_found: %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.req(t, http.MethodDelete, unkeepURL(e.ID, "container", "", "never-existed"), ""); rec.code != http.StatusNotFound || rec.ptyp != "keep_not_found" {
		t.Fatalf("unkeep of a container that was never there: %d %q, want 404 keep_not_found", rec.code, rec.ptyp)
	}
	if rec := f.req(t, http.MethodDelete, keepsURL(e.ID), ""); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("unkeep with no kind or ref: %d, want 422", rec.code)
	}
	if n := len(janitorKeepAudits(t, f.st.Read, "janitor.unkept")); n != 0 {
		t.Errorf("janitor.unkept rows = %d, want none", n)
	}
}

// A keep for an object that is not there is refused, as is one over the limit, and nothing is stored.
func TestJanitorKeepAbsentObject(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, janitorImageEngine(danglingImage("a")))
	awaitOnline(t, f.mon, e.ID)
	for kind, ref := range map[string]string{"container": "ghost", "image": imageIDOf("f"), "network": "nowhere"} {
		rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne(kind, "", ref))
		if rec.code != http.StatusNotFound || rec.ptyp != "not_found" {
			t.Errorf("keep of an absent %s: %d %q, want 404 not_found: %s", kind, rec.code, rec.ptyp, rec.body)
		}
	}
	// One absent target refuses the whole request: the present image is not kept either.
	body := fmt.Sprintf(`{"targets":[{"kind":"image","nodeId":%q,"ref":%q},{"kind":"container","ref":"ghost"}]}`, e.ID, imageIDOf("a"))
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), body); rec.code != http.StatusNotFound {
		t.Fatalf("keep with one absent target: %d, want 404", rec.code)
	}
	var targets []string
	for i := range 101 {
		targets = append(targets, fmt.Sprintf(`{"kind":"container","ref":"c%d"}`, i))
	}
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), `{"targets":[`+strings.Join(targets, ",")+`]}`); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("101 targets: %d, want 422", rec.code)
	}
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), `{"targets":[]}`); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("no targets: %d, want 422", rec.code)
	}
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), `{"targets":[{"kind":"volume","ref":"v"}]}`); rec.code != http.StatusUnprocessableEntity {
		t.Fatalf("kind volume: %d, want 422", rec.code)
	}
	if rows := f.janitorKeeps(t, e.ID); len(rows) != 0 {
		t.Fatalf("janitor_keeps = %+v, want none from refused requests", rows)
	}
	if n := len(janitorKeepAudits(t, f.st.Read, "janitor.kept")); n != 0 {
		t.Errorf("janitor.kept rows = %d, want none", n)
	}
}

// A second keep of the same object keeps the first row (who and when) and writes no audit row; a request that
// names it twice keeps it once.
func TestJanitorKeepTwiceNoOp(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := dashEnv(t, janitorImageEngine(danglingImage("a"), danglingImage("b")))
	awaitOnline(t, f.mon, e.ID)
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("image", e.ID, id)); rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 1 {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	first := f.janitorKeeps(t, e.ID)

	rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("image", e.ID, id))
	if rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 0 {
		t.Fatalf("second keep: %d %s, want 200 and none newly kept", rec.code, rec.body)
	}
	if got := f.janitorKeeps(t, e.ID); !slices.Equal(got, first) {
		t.Fatalf("janitor_keeps after a second keep = %+v, want the first row %+v", got, first)
	}
	if n := len(janitorKeepAudits(t, f.st.Read, "janitor.kept")); n != 1 {
		t.Fatalf("janitor.kept rows = %d, want 1", n)
	}

	both := fmt.Sprintf(`{"targets":[{"kind":"image","nodeId":%q,"ref":%q},{"kind":"image","ref":%q},{"kind":"image","ref":%q}]}`, e.ID, id, imageIDOf("b"), imageIDOf("b"))
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), both); rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 1 {
		t.Fatalf("keep of one old and one new image named twice: %d %s, want one newly kept", rec.code, rec.body)
	}
	rows := janitorKeepAudits(t, f.st.Read, "janitor.kept")
	if len(rows) != 2 || rows[1].TargetID != imageIDOf("b") || rows[1].Details["bulk"] != "true" {
		t.Fatalf("janitor.kept rows = %+v, want a second, for b, marked bulk", rows)
	}
}

// Keep and stop keeping need janitor.run (an Env Admin): an Operator is refused with nothing stored; a Viewer
// may read the list.
func TestJanitorKeepNeedsEnvAdmin(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := dashEnv(t, janitorImageEngine(danglingImage("a")))
	f.addUser(t, "operator", false)
	f.addEnvRole(t, "operator", e.ID, "operator")
	f.addUser(t, "viewer", false)
	f.addEnvRole(t, "viewer", e.ID, "viewer")
	awaitOnline(t, f.mon, e.ID)
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("image", e.ID, id)); rec.code != http.StatusOK {
		t.Fatalf("keep as the admin: %d %s", rec.code, rec.body)
	}
	cookie, csrf := f.signIn(t, "operator", false)
	for name, r := range map[string]authReq{
		"keep":   {method: http.MethodPut, path: keepsURL(e.ID), body: keepOne("image", e.ID, imageIDOf("b"))},
		"unkeep": {method: http.MethodDelete, path: unkeepURL(e.ID, "image", e.ID, id)},
	} {
		r.cookie, r.csrf = cookie, csrf
		if rec := f.call(t, r); rec.Code != http.StatusForbidden {
			t.Errorf("%s as an Operator: %d, want 403: %s", name, rec.Code, rec.Body)
		}
	}
	if rows := f.janitorKeeps(t, e.ID); len(rows) != 1 || rows[0].Ref != id {
		t.Errorf("janitor_keeps = %+v, want only the admin's keep: a refused request changed it", rows)
	}
	if n := len(janitorKeepAudits(t, f.st.Read, "janitor.unkept")); n != 0 {
		t.Errorf("janitor.unkept rows = %d, want none from a refused request", n)
	}

	cookie, csrf = f.signIn(t, "viewer", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: keepsURL(e.ID), cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusOK {
		t.Fatalf("list as a Viewer: %d %s, want 200", rec.Code, rec.Body)
	}
	var got keptListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Items) != 1 || got.Items[0].Ref != id {
		t.Fatalf("list as a Viewer = %s (%v), want the one keep", rec.Body, err)
	}
}

// The list holds every keep row, present or not, and every container, image and network that carries the keep
// label (kept by label); an object with both is one item that reads as kept by its label.
func TestJanitorKeptListIncludesLabels(t *testing.T) {
	t.Parallel()
	label := map[string]string{"tierplan.keep": "true"}
	net := idleNet("l1", "labelled")
	net.Labels = label
	img := danglingImage("c")
	img.Labels = label
	both := danglingImage("d")
	both.Labels = label
	cfg := janitorImageEngine(img, both, danglingImage("a"))
	cfg.Networks = append(cfg.Networks, net)
	cfg.Containers = []dockertest.FakeContainer{
		{ID: "lab-ctr", Names: []string{"/lab"}, Image: "nginx:1", State: "exited", Created: janitorNow.Add(-72 * time.Hour).Unix(), Labels: label},
		{ID: "plain-ctr", Names: []string{"/plain"}, Image: "nginx:1", State: "exited", Created: janitorNow.Add(-72 * time.Hour).Unix()},
	}
	f, e := dashEnv(t, cfg)
	awaitOnline(t, f.mon, e.ID)
	body := fmt.Sprintf(`{"targets":[{"kind":"container","ref":"plain-ctr"},{"kind":"image","ref":%q},{"kind":"image","ref":%q}]}`, imageIDOf("a"), imageIDOf("d"))
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), body); rec.code != http.StatusOK || decode[keepCount](t, rec).Kept != 3 {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}

	byRef := map[string]keptItemJSON{}
	for _, it := range f.keptList(t, e.ID) {
		byRef[it.Kind+"/"+it.Ref] = it
	}
	want := map[string]struct {
		keptBy, name string
		byName       bool
	}{
		"container/lab-ctr":       {"label", "lab", false},
		"container/plain-ctr":     {"user", "plain", true},
		"image/" + imageIDOf("c"): {"label", imageIDOf("c")[7:19], false},
		"image/" + imageIDOf("a"): {"user", imageIDOf("a")[7:19], true},
		"image/" + imageIDOf("d"): {"label", imageIDOf("d")[7:19], false},
		"network/labelled":        {"label", "labelled", false},
	}
	if len(byRef) != len(want) {
		t.Fatalf("kept list = %+v, want %d items", byRef, len(want))
	}
	for key, w := range want {
		it, ok := byRef[key]
		if !ok || it.KeptBy != w.keptBy || it.Name != w.name || !it.Present || (it.KeptByName != "") != w.byName || (it.KeptAt != "") != w.byName && w.keptBy == "user" {
			t.Errorf("%s = %+v (listed %v), want kept by %s named %s and present", key, it, ok, w.keptBy, w.name)
		}
	}
	// The one kept both ways says it was kept by its label, and still knows when a person kept it.
	if d := byRef["image/"+imageIDOf("d")]; d.KeptAt == "" {
		t.Errorf("the image kept both ways = %+v, want the time of the person's keep", d)
	}
	// Containers sort before images before networks.
	items := f.keptList(t, e.ID)
	if items[0].Kind != "container" || items[len(items)-1].Kind != "network" {
		t.Errorf("kept list order = %+v, want containers, then images, then networks", items)
	}
}

// JAN-07: nothing a person chose Keep on is removed. A Clean now that ticked a container before it was kept
// records it skipped_protected and sends no delete for it; the engine itself refuses a kept container or image
// that a keep made after the plan reaches, and sends nothing.
func TestJanitorKeptItemsNeverRemoved(t *testing.T) {
	t.Parallel()
	s := janitorSwarm(t, onWorker(exitedSlot(5)), nil)
	_, preview := s.previewJanitor(t)
	if len(preview.Items) != 3 {
		t.Fatalf("preview items = %+v, want 3", preview.Items)
	}
	first, second := preview.Items[0], preview.Items[1]
	if rec := s.req(t, http.MethodPut, keepsURL(s.env.ID), keepOne("container", first.NodeID, first.Ref)); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}

	rec, started := s.runJanitor(t, tick(first, second))
	if rec.code != http.StatusAccepted {
		t.Fatalf("Clean now: %d %s", rec.code, rec.body)
	}
	s.awaitRun(t, started.JobID)
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/" + second.Ref}) {
		t.Fatalf("the daemons saw %v, want only the delete of %s", got, second.Ref)
	}
	_, run := s.getRun(t, started.RunID)
	outcomes := map[string]string{}
	codes := map[string]string{}
	for _, it := range run.Items {
		outcomes[it.Ref], codes[it.Ref] = it.Outcome, it.Code
	}
	if outcomes[first.Ref] != "skipped_protected" || codes[first.Ref] != "no_longer_stale" || outcomes[second.Ref] != "removed" {
		t.Fatalf("run items = %v %v, want %s skipped_protected and %s removed", outcomes, codes, first.Ref, second.Ref)
	}

	// A keep made after the plan was read is still honoured at the moment of removal.
	eng := api.NewJanitorEngine(s.agg, s.clusters, nil, janitor.KeepStore{St: s.st}, nil)
	third := preview.Items[2]
	if rec := s.req(t, http.MethodPut, keepsURL(s.env.ID), keepOne("container", third.NodeID, third.Ref)); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	if err := eng.RemoveContainer(testsupport.Context(t), s.env.ID, third.NodeID, third.Ref); !errors.Is(err, janitor.ErrNoLongerStale) {
		t.Fatalf("RemoveContainer of a kept container = %v, want ErrNoLongerStale", err)
	}
	if got := s.containerDeletes(); !slices.Equal(got, []string{"DELETE /containers/" + second.Ref}) {
		t.Fatalf("the daemons saw %v after the engine's refusal, want nothing more", got)
	}
}

// The same refusal for an image, on a standalone host: a kept image is never sent to the engine.
func TestJanitorKeptImageNeverRemoved(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := agedDanglingEnv(t, janitorImageEngine(danglingImage("a")))
	preview := f.previewJanitor(t, e)
	if rec := f.req(t, http.MethodPut, keepsURL(e.ID), keepOne("image", e.ID, id)); rec.code != http.StatusOK {
		t.Fatalf("keep: %d %s", rec.code, rec.body)
	}
	_, started := f.cleanNow(t, e, preview.Items[0])
	run := f.runRecord(t, e, started.RunID)
	if len(run.Items) != 1 || run.Items[0].Outcome != "skipped_protected" || run.Items[0].Code != "no_longer_stale" || run.Counts.RemovedImages != 0 {
		t.Fatalf("run = %+v, want the ticked kept image recorded skipped_protected", run)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none: a kept image is never sent to the engine", w)
	}
	eng := api.NewJanitorEngine(f.agg, f.clusters, nil, janitor.KeepStore{St: f.st}, nil)
	if err := eng.RemoveImage(testsupport.Context(t), e.ID, e.ID, id); !errors.Is(err, janitor.ErrNoLongerStale) {
		t.Fatalf("RemoveImage of a kept image = %v, want ErrNoLongerStale", err)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v after the engine's refusal, want none", w)
	}
}

// A keep stored against a node ID the environment no longer has, as after an agent is enrolled again,
// still holds (WR-06): the image is protected kept_row in the preview, the engine refuses it at the moment
// of removal, and the Kept list shows the keep as present on the node the image is on now.
func TestJanitorKeepSurvivesANewNodeID(t *testing.T) {
	t.Parallel()
	id := imageIDOf("a")
	f, e := dashEnv(t, janitorImageEngine(danglingImage("a")))
	if _, err := f.st.Write.ExecContext(testsupport.Context(t),
		`INSERT INTO janitor_keeps (env_id, kind, node_id, ref, name, kept_by, kept_at) VALUES (?, 'image', 'old-agent', ?, '', 'someone', 1)`,
		e.ID, id); err != nil {
		t.Fatal(err)
	}
	preview := f.previewJanitor(t, e)
	if it, ok := notRemovedRef(preview, id); len(preview.Items) != 0 || !ok || it.Class != "protected" || it.Reason.Code != "kept_row" {
		t.Fatalf("preview items = %+v kept %+v (%v), want the image protected kept_row", preview.Items, it, ok)
	}
	eng := api.NewJanitorEngine(f.agg, f.clusters, nil, janitor.KeepStore{St: f.st}, nil)
	if err := eng.RemoveImage(testsupport.Context(t), e.ID, e.ID, id); !errors.Is(err, janitor.ErrNoLongerStale) {
		t.Fatalf("RemoveImage of an image kept under an old node ID = %v, want ErrNoLongerStale", err)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	kept := f.keptList(t, e.ID)
	if len(kept) != 1 || kept[0].Ref != id || kept[0].NodeID != "old-agent" || !kept[0].Present || kept[0].Node != "node-1" {
		t.Fatalf("kept list = %+v, want the keep present on node-1 under the node ID it was stored with", kept)
	}
}
