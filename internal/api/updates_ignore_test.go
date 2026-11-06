// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
)

// Skipping a version and ignoring updates (plan 12-13, IMG-08, D-10, D-15, D-16). Docker is a fake daemon, the
// registry a scripted Heads, everything between them the real code.

// heldInfo is the update object with the skip or ignore it rests on.
type heldInfo struct {
	updateInfo
	Ignore *struct {
		Mode   string `json:"mode"`
		Scope  string `json:"scope"`
		SetBy  string `json:"setBy"`
		SetAt  string `json:"setAt"`
		Digest string `json:"digest"`
	} `json:"ignore"`
}

type heldList struct {
	Items []struct {
		Name   string    `json:"name"`
		Update *heldInfo `json:"update"`
	} `json:"items"`
}

type heldAnswer struct {
	Update *heldInfo `json:"update"`
}

// set moves what the scripted registry says for a repository.
func (h *scriptedHeads) set(repo, digest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[repo] = headAnswer{digest: digest}
}

// updatesEnvOf is updatesEnv for a swarm manager the caller configured.
func updatesEnvOf(t *testing.T, heads *scriptedHeads, cfg dockertest.FakeConfig) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, cfg)
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

// heldOf is the update object of the service named name in the services list.
func (f *envFix) heldOf(t *testing.T, e envBody, name string) *heldInfo {
	t.Helper()
	list := decode[heldList](t, f.get(t, e, "services"))
	for _, it := range list.Items {
		if it.Name == name {
			if it.Update == nil {
				t.Fatalf("service %q has no update object", name)
			}
			return it.Update
		}
	}
	t.Fatalf("no service %q in %+v", name, list)
	return nil
}

func skipBody(kind, name string) string {
	return fmt.Sprintf(`{"kind":%q,"name":%q}`, kind, name)
}

func (f *envFix) skip(t *testing.T, e envBody, method, name string) *envRec {
	t.Helper()
	return f.req(t, method, "/api/env/"+e.ID+"/updates/skip", skipBody("service", name))
}

func (f *envFix) ignoreReq(t *testing.T, e envBody, method, body string) *envRec {
	t.Helper()
	return f.req(t, method, "/api/env/"+e.ID+"/updates/ignore", body)
}

// The tracer (plan 12-13, IMG-08, D-15): an Admin skips the version a service is offered; the service reads
// skipped, with who and which version, until the tag moves again, and the skip is not an update to count.
func TestSkipHidesThisVersionOnly(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updatesEnv(t, heads, updatedService("web", 'a'))
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	if u := f.heldOf(t, e, "web"); u.State != "update_available" || u.RemoteDigest != updDigest('b') || u.Ignore != nil {
		t.Fatalf("before the skip = %+v, want update_available at b", u)
	}

	rec := f.skip(t, e, http.MethodPost, "web")
	if rec.code != http.StatusOK {
		t.Fatalf("skip: %d %s", rec.code, rec.body)
	}
	if got := decode[heldAnswer](t, rec).Update; got == nil || got.State != "skipped" {
		t.Errorf("skip answer = %+v, want the update object, skipped", got)
	}
	u := f.heldOf(t, e, "web")
	if u.State != "skipped" || u.Ignore == nil || u.Ignore.Mode != "skip" || u.Ignore.Scope != "workload" || u.Ignore.SetBy == "" ||
		u.Ignore.SetAt == "" || u.Ignore.Digest != updDigest('b') || u.RemoteDigest != updDigest('b') || u.RunningDigest != updDigest('a') {
		t.Fatalf("after the skip = %+v (ignore %+v), want skipped at b with who and when", u, u.Ignore)
	}
	if got := decode[updateCheckAnswer](t, f.checkNow(t, e)); got.Updates != 0 {
		t.Errorf("check now counts %d updates for a skipped version, want 0", got.Updates)
	}
	// Skipping what is skipped changes nothing and records nothing more.
	if rec := f.skip(t, e, http.MethodPost, "web"); rec.code != http.StatusOK {
		t.Fatalf("skip again: %d %s", rec.code, rec.body)
	}

	// The tag moves to a newer version: the skip lifts and the update is offered again.
	heads.set("team/web", updDigest('c'))
	f.checkNow(t, e)
	if u := f.heldOf(t, e, "web"); u.State != "update_available" || u.RemoteDigest != updDigest('c') || u.Ignore != nil {
		t.Fatalf("after the tag moved = %+v, want update_available at c", u)
	}

	// Taking the skip back means that even the version it passed over is offered again.
	if rec := f.skip(t, e, http.MethodDelete, "web"); rec.code != http.StatusNoContent {
		t.Fatalf("unskip: %d %s", rec.code, rec.body)
	}
	heads.set("team/web", updDigest('b'))
	f.checkNow(t, e)
	if u := f.heldOf(t, e, "web"); u.State != "update_available" || u.RemoteDigest != updDigest('b') {
		t.Fatalf("after the skip was taken back = %+v, want update_available at b", u)
	}
	if rec := f.skip(t, e, http.MethodDelete, "web"); rec.code != http.StatusNotFound || rec.ptyp != "skip_not_found" {
		t.Errorf("unskip of nothing = %d %s, want 404 skip_not_found", rec.code, rec.ptyp)
	}

	rows := f.auditRows(t, "image.update_skipped")
	if len(rows) != 1 || rows[0].EnvID != e.ID || rows[0].Details["workload"] != "web" || rows[0].Details["kind"] != "service" ||
		rows[0].Details["digest"] != updDigest('b') || rows[0].Details["image"] != "registry.test/team/web:1" {
		t.Errorf("skip audit rows = %+v, want one naming the workload, the image and the digest", rows)
	}
	if rows := f.auditRows(t, "image.update_unskipped"); len(rows) != 1 || rows[0].Details["workload"] != "web" {
		t.Errorf("unskip audit rows = %+v, want one", rows)
	}
}

// Ignoring an image covers every service using it in this environment and no other service (plan 12-13, D-16).
func TestIgnoreImageScopeCoversEveryWorkload(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/app": {digest: updDigest('b')}, "team/other": {digest: updDigest('b')}})
	app := func(name string, tag string) dockertest.FakeService {
		return dockertest.FakeService{
			ID: "svc-" + name, Name: name, Mode: "replicated", Running: 1, Desired: 1, Version: 1,
			Image: fmt.Sprintf("registry.test/team/app:%s@%s", tag, updDigest('a')),
		}
	}
	other := updatedService("other", 'a')
	f, e := updatesEnv(t, heads, app("one", "1"), app("two", "1"), app("three", "2"), other)
	heads.set("team/app", updDigest('b'))
	f.checkNow(t, e)
	for _, name := range []string{"one", "two", "three", "other"} {
		if u := f.heldOf(t, e, name); u.State != "update_available" {
			t.Fatalf("%s before = %s, want update_available", name, u.State)
		}
	}

	body := `{"scope":"image","image":"registry.test/team/app:1"}`
	if rec := f.ignoreReq(t, e, http.MethodPost, body); rec.code != http.StatusOK {
		t.Fatalf("ignore: %d %s", rec.code, rec.body)
	}
	for name, want := range map[string]string{"one": "ignored", "two": "ignored", "three": "update_available", "other": "update_available"} {
		u := f.heldOf(t, e, name)
		if u.State != want {
			t.Errorf("%s = %s, want %s", name, u.State, want)
		}
		if want == "ignored" && (u.Ignore == nil || u.Ignore.Mode != "ignore" || u.Ignore.Scope != "image" || u.Ignore.SetBy == "" || u.Ignore.SetAt == "") {
			t.Errorf("%s ignore = %+v, want an image ignore with who and when", name, u.Ignore)
		}
	}
	if got := decode[updateCheckAnswer](t, f.checkNow(t, e)); got.Updates != 2 {
		t.Errorf("check now counts %d updates, want the 2 that are not ignored", got.Updates)
	}
	// Choosing it again changes nothing.
	if rec := f.ignoreReq(t, e, http.MethodPost, body); rec.code != http.StatusOK {
		t.Fatalf("ignore again: %d %s", rec.code, rec.body)
	}
	if rows := f.auditRows(t, "image.updates_ignored"); len(rows) != 1 || rows[0].Details["scope"] != "image" || rows[0].Details["image"] != "registry.test/team/app:1" {
		t.Errorf("ignore audit rows = %+v, want one naming the scope and the image", rows)
	}

	// One service alone.
	if rec := f.ignoreReq(t, e, http.MethodPost, `{"scope":"workload","kind":"service","name":"three"}`); rec.code != http.StatusOK {
		t.Fatalf("ignore a workload: %d %s", rec.code, rec.body)
	} else if got := decode[heldAnswer](t, rec).Update; got == nil || got.State != "ignored" || got.Ignore == nil || got.Ignore.Scope != "workload" {
		t.Errorf("ignore a workload answer = %+v, want its update object, ignored", got)
	}
	if u := f.heldOf(t, e, "other"); u.State != "update_available" {
		t.Errorf("other = %s after ignoring another workload, want update_available", u.State)
	}

	// Refusals.
	for name, c := range map[string]struct{ body, want string }{
		"no tag via a digest": {`{"scope":"image","image":"registry.test/team/app@` + updDigest('a') + `"}`, "invalid_image"},
		"not a reference":     {`{"scope":"image","image":"NOT AN IMAGE"}`, "invalid_image"},
		"no name":             {`{"scope":"workload","kind":"service"}`, "invalid_ignore_target"},
	} {
		if rec := f.ignoreReq(t, e, http.MethodPost, c.body); rec.code != http.StatusUnprocessableEntity || rec.ptyp != c.want {
			t.Errorf("%s = %d %s, want 422 %s", name, rec.code, rec.ptyp, c.want)
		}
	}
	if rec := f.ignoreReq(t, e, http.MethodPost, `{"scope":"workload","kind":"service","name":"nope"}`); rec.code != http.StatusNotFound {
		t.Errorf("ignore of a service that is not there = %d, want 404", rec.code)
	}

	// Taking them back.
	for _, b := range []string{body, `{"scope":"workload","kind":"service","name":"three"}`} {
		if rec := f.ignoreReq(t, e, http.MethodDelete, b); rec.code != http.StatusNoContent {
			t.Fatalf("unignore %s: %d %s", b, rec.code, rec.body)
		}
	}
	if rec := f.ignoreReq(t, e, http.MethodDelete, body); rec.code != http.StatusNotFound || rec.ptyp != "ignore_not_found" {
		t.Errorf("unignore of nothing = %d %s, want 404 ignore_not_found", rec.code, rec.ptyp)
	}
	for _, name := range []string{"one", "two", "three"} {
		if u := f.heldOf(t, e, name); u.State != "update_available" {
			t.Errorf("%s after unignore = %s, want update_available", name, u.State)
		}
	}
	if rows := f.auditRows(t, "image.updates_unignored"); len(rows) != 2 {
		t.Errorf("unignore audit rows = %+v, want 2", rows)
	}
}

// The label in the compose file (plan 12-13, D-16): on the service, or on the containers of its tasks, and only
// its owner can remove it.
func TestIgnoreByLabel(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/deploy": {digest: updDigest('b')}, "team/tasks": {digest: updDigest('b')}, "team/plain": {digest: updDigest('b')},
	})
	deploy, tasks, plain := updatedService("deploy", 'a'), updatedService("tasks", 'a'), updatedService("plain", 'a')
	deploy.Labels = map[string]string{"tierplan.updates": "ignore"}
	cfg := swarmManagerEngine(deploy, tasks, plain)
	taskContainer := func(name, service string, labels map[string]string) dockertest.FakeContainer {
		l := map[string]string{"com.docker.swarm.service.id": service}
		for k, v := range labels {
			l[k] = v
		}
		return dockertest.FakeContainer{ID: "id-" + name, Names: []string{"/" + name}, Image: "registry.test/team/x:1", ImageID: updImage('4'), State: "running", Status: "Up 1 hour", Labels: l}
	}
	cfg.Containers = []dockertest.FakeContainer{
		taskContainer("tasks.1", "svc-tasks", map[string]string{"tierplan.updates": "ignore"}),
		taskContainer("plain.1", "svc-plain", nil),
	}
	f, e := updatesEnvOf(t, heads, cfg)
	f.checkNow(t, e)

	for _, name := range []string{"deploy", "tasks"} {
		u := f.heldOf(t, e, name)
		if u.State != "ignored" || u.Ignore == nil || u.Ignore.Scope != "label" || u.Ignore.Mode != "ignore" || u.Ignore.SetBy != "" {
			t.Errorf("%s = %s %+v, want ignored by label with nobody who set it", name, u.State, u.Ignore)
		}
	}
	if u := f.heldOf(t, e, "plain"); u.State != "update_available" {
		t.Errorf("plain = %s, want update_available", u.State)
	}
	if got := decode[updateCheckAnswer](t, f.checkNow(t, e)); got.Updates != 1 {
		t.Errorf("check now counts %d updates, want only the plain service", got.Updates)
	}

	for _, name := range []string{"deploy", "tasks"} {
		rec := f.ignoreReq(t, e, http.MethodDelete, fmt.Sprintf(`{"scope":"workload","kind":"service","name":%q}`, name))
		if rec.code != http.StatusConflict || rec.ptyp != "ignored_by_label" {
			t.Errorf("unignore %s = %d %s, want 409 ignored_by_label", name, rec.code, rec.ptyp)
		}
	}
	if rows := f.auditRows(t, "image.updates_unignored"); len(rows) != 0 {
		t.Errorf("unignore audit rows = %+v, want none for a refusal", rows)
	}
}

// Only an Admin may hide updates (plan 12-13, D-10, T-12-31), and a skip needs an update to skip.
func TestIgnoreRoutesNeedAdmin(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('b')}})
	f, e := updatesEnv(t, heads, updatedService("web", 'a'))
	f.checkNow(t, e)
	awaitOnline(t, f.mon, e.ID)

	f.addUser(t, "vera", false)
	f.addEnvRole(t, "vera", e.ID, "viewer")
	f.addUser(t, "olive", false)
	f.addEnvRole(t, "olive", e.ID, "operator")
	f.addUser(t, "adam", false)
	f.addEnvRole(t, "adam", e.ID, "admin")
	base := "/api/env/" + e.ID + "/updates/"
	calls := []struct{ method, path, body string }{
		{http.MethodPost, "skip", skipBody("service", "web")},
		{http.MethodDelete, "skip", skipBody("service", "web")},
		{http.MethodPost, "ignore", `{"scope":"workload","kind":"service","name":"web"}`},
		{http.MethodDelete, "ignore", `{"scope":"workload","kind":"service","name":"web"}`},
	}
	for _, user := range []string{"vera", "olive"} {
		cookie, csrf := f.signIn(t, user, false)
		for _, c := range calls {
			if rec := f.call(t, authReq{method: c.method, path: base + c.path, body: c.body, cookie: cookie, csrf: csrf}); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d %s, want 403", user, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	if u := f.heldOf(t, e, "web"); u.State != "update_available" {
		t.Fatalf("web after the refusals = %s, want update_available", u.State)
	}
	for _, a := range []string{"image.update_skipped", "image.updates_ignored", "image.update_unskipped", "image.updates_unignored"} {
		if rows := f.auditRows(t, a); len(rows) != 0 {
			t.Errorf("%s audit rows after refusals = %+v, want none", a, rows)
		}
	}

	cookie, csrf := f.signIn(t, "adam", false)
	if rec := f.call(t, authReq{method: http.MethodPost, path: base + "skip", body: skipBody("service", "web"), cookie: cookie, csrf: csrf}); rec.Code != http.StatusOK {
		t.Errorf("an admin's skip = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := f.call(t, authReq{method: http.MethodPost, path: base + "ignore", body: calls[2].body, cookie: cookie, csrf: csrf}); rec.Code != http.StatusOK {
		t.Errorf("an admin's ignore = %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestSkipNothingToSkip(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/same": {digest: updDigest('a')}})
	f, e := updatesEnv(t, heads, updatedService("same", 'a'), updatedService("unchecked", 'a'))
	f.checkNow(t, e)

	for name, svc := range map[string]string{"up to date": "same", "never checked": "unchecked"} {
		if rec := f.skip(t, e, http.MethodPost, svc); rec.code != http.StatusConflict || rec.ptyp != "nothing_to_skip" {
			t.Errorf("skip of a service %s = %d %s, want 409 nothing_to_skip", name, rec.code, rec.ptyp)
		}
	}
	if rec := f.skip(t, e, http.MethodPost, "ghost"); rec.code != http.StatusNotFound {
		t.Errorf("skip of a service that is not there = %d, want 404", rec.code)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT COUNT(*) FROM image_update_ignores`).Scan(&n); err != nil || n != 0 {
		t.Errorf("stored skips = %d, %v, want none", n, err)
	}
	if rows := f.auditRows(t, "image.update_skipped"); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}
