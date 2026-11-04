// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/regclient/regclient/types/errs"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Check now and the update object of the services list (plan 12-37, IMG-04, IMG-05). Docker is a fake
// daemon behind the real monitor, snapshot cache, aggregator and cluster reader; the registry is a
// scripted Heads; the update service, its SQLite store, the router, policy and permission checks are the
// real ones.

// updDigest is a digest made of one repeated character.
func updDigest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// headAnswer is what the scripted registry says for a repository: a digest, or an error.
type headAnswer struct {
	digest string
	err    error
	// platforms, when set, makes the digest an index that lists these platforms (linux/amd64 to manifest digest).
	platforms map[string]string
}

// scriptedHeads answers for the registry by repository path and counts nothing else. A repository nobody
// scripted is unreachable. Every digest names one image, not an index.
type scriptedHeads struct {
	mu      sync.Mutex
	answers map[string]headAnswer
	asked   int
}

func newScriptedHeads(answers map[string]headAnswer) *scriptedHeads {
	return &scriptedHeads{answers: answers}
}

func (h *scriptedHeads) Digest(_ context.Context, _ string, ref registry.Ref) (string, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked++
	a, ok := h.answers[ref.Path]
	if !ok {
		return "", "", errors.New("connection refused")
	}
	return a.digest, "", a.err
}

// requests is how many tags were asked about.
func (h *scriptedHeads) requests() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked
}

func (h *scriptedHeads) Index(_ context.Context, _ string, ref registry.Ref, _ string) (bool, map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.answers[ref.Path].platforms; len(p) > 0 {
		return true, p, nil
	}
	return false, nil, nil
}

// updatesEnv is a local swarm manager environment whose routes check images with heads.
func updatesEnv(t *testing.T, heads *scriptedHeads, services ...dockertest.FakeService) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, swarmManagerEngine(services...))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

// updatedService is a service running the digest given of repo team/<name>.
func updatedService(name string, running byte) dockertest.FakeService {
	return dockertest.FakeService{
		ID: "svc-" + name, Name: name, Mode: "replicated", Running: 1, Desired: 1, Version: 1,
		Image: fmt.Sprintf("registry.test/team/%s:1@%s", name, updDigest(running)),
	}
}

type updateInfo struct {
	State            string   `json:"state"`
	Reason           string   `json:"reason"`
	Registry         string   `json:"registry"`
	Repository       string   `json:"repository"`
	Tag              string   `json:"tag"`
	RunningDigest    string   `json:"runningDigest"`
	RemoteDigest     string   `json:"remoteDigest"`
	ChangedPlatforms []string `json:"changedPlatforms"`
	CheckedAt        string   `json:"checkedAt"`
	SuccessAt        string   `json:"successAt"`
	Note             string   `json:"note"`
	Task             bool     `json:"task"`
	JobID            string   `json:"jobId"`
}

type updatedItem struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Update *updateInfo `json:"update"`
}

type updatedList struct {
	Items []updatedItem `json:"items"`
}

type updateCheckAnswer struct {
	Checked int `json:"checked"`
	Updates int `json:"updates"`
	Failed  int `json:"failed"`
}

// updateOf is the update object of the service named name in a list, nil when it has none.
func updateOf(t *testing.T, list updatedList, name string) *updateInfo {
	t.Helper()
	for _, it := range list.Items {
		if it.Name == name {
			return it.Update
		}
	}
	t.Fatalf("no service %q in %+v", name, list)
	return nil
}

// envCheckSpacing is how long the server makes one person wait between two checks of one environment.
const envCheckSpacing = 10 * time.Second

// checkNow posts to the update check of e as the fixture's administrator. It moves the fixture's clock past the spacing the server
// keeps between checks first, so a test may check as often as it needs; checkNowSoon does not.
func (f *envFix) checkNow(t *testing.T, e envBody) *envRec {
	t.Helper()
	f.clk.t = f.clk.t.Add(envCheckSpacing)
	return f.checkNowSoon(t, e)
}

// checkNowSoon is checkNow without moving the clock: a check made soon after the last is refused.
func (f *envFix) checkNowSoon(t *testing.T, e envBody) *envRec {
	t.Helper()
	return f.req(t, http.MethodPost, "/api/env/"+e.ID+"/updates/check", "")
}

func TestCheckNowThenServicesListShowsState(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/behind": {digest: updDigest('b')},
		"team/same":   {digest: updDigest('c')},
		"team/denied": {err: fmt.Errorf("denied: token=abc: %w", errs.ErrHTTPUnauthorized)},
	})
	f, e := updatesEnv(t, heads,
		updatedService("behind", 'a'), updatedService("same", 'c'), updatedService("denied", 'd'))

	// Nothing has been checked yet: the list says so, in every row.
	before := decode[updatedList](t, f.get(t, e, "services"))
	for _, name := range []string{"behind", "same", "denied"} {
		if u := updateOf(t, before, name); u == nil || u.State != "not_checked" {
			t.Errorf("%s before the check = %+v, want not_checked", name, u)
		}
	}

	rec := f.checkNow(t, e)
	if rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	if got := decode[updateCheckAnswer](t, rec); got != (updateCheckAnswer{Checked: 3, Updates: 1, Failed: 1}) {
		t.Errorf("check now = %+v, want 3 checked, 1 update, 1 failed", got)
	}

	after := decode[updatedList](t, f.get(t, e, "services"))
	behind := updateOf(t, after, "behind")
	if behind == nil || behind.State != "update_available" || behind.RunningDigest != updDigest('a') || behind.RemoteDigest != updDigest('b') ||
		behind.Registry != "registry.test" || behind.Repository != "team/behind" || behind.Tag != "1" || behind.CheckedAt == "" || behind.SuccessAt == "" {
		t.Errorf("behind = %+v, want update_available naming both digests and the image", behind)
	}
	if same := updateOf(t, after, "same"); same == nil || same.State != "up_to_date" || same.Reason != "" {
		t.Errorf("same = %+v, want up_to_date", same)
	}
	if denied := updateOf(t, after, "denied"); denied == nil || denied.State != "check_failed" || denied.Reason != "auth" || denied.SuccessAt != "" {
		t.Errorf("denied = %+v, want check_failed with reason auth", denied)
	}
}

// checkNowAs posts to the update check of e as a person holding role on it, and answers the response.
func (f *envFix) checkNowAs(t *testing.T, e envBody, user, role string) *httptest.ResponseRecorder {
	t.Helper()
	f.addUser(t, user, false)
	f.addEnvRole(t, user, e.ID, role)
	awaitOnline(t, f.mon, e.ID)
	cookie, csrf := f.signIn(t, user, false)
	return f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/updates/check", cookie: cookie, csrf: csrf})
}

func TestCheckNowPermissions(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/web": {digest: updDigest('a')}})
	f, e := updatesEnv(t, heads, updatedService("web", 'a'))

	if rec := f.checkNowAs(t, e, "vera", "viewer"); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer's check = %d %s, want 403", rec.Code, rec.Body)
	}
	if got := heads.requests(); got != 0 {
		t.Errorf("a refused check asked the registry %d times, want 0", got)
	}
	rec := f.checkNowAs(t, e, "olive", "operator")
	if rec.Code != http.StatusOK {
		t.Fatalf("an operator's check = %d %s, want 200", rec.Code, rec.Body)
	}
	if got := heads.requests(); got != 1 {
		t.Errorf("the operator's check asked the registry %d times, want 1", got)
	}
}

func TestUpdateObjectCarriesNoRegistryText(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/web": {err: fmt.Errorf("denied: token=abc: %w", errs.ErrHTTPUnauthorized)},
	})
	f, e := updatesEnv(t, heads, updatedService("web", 'a'))

	check := f.checkNow(t, e)
	if check.code != http.StatusOK {
		t.Fatalf("check now: %d %s", check.code, check.body)
	}
	list := f.get(t, e, "services")
	if list.code != http.StatusOK {
		t.Fatalf("list: %d %s", list.code, list.body)
	}
	u := updateOf(t, decode[updatedList](t, list), "web")
	if u == nil || u.State != "check_failed" || u.Reason != "auth" {
		t.Fatalf("update = %+v, want check_failed with reason auth", u)
	}
	for _, body := range []string{string(check.body), string(list.body), f.logs.String()} {
		if strings.Contains(body, "denied") || strings.Contains(body, "token=abc") {
			t.Errorf("the registry's own text reached an answer or the log: %s", body)
		}
	}
}

func TestNoUpdateObjectForAgentAndJobs(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{})
	f, e := updatesEnv(t, heads,
		updatedService("web", 'a'),
		dockertest.FakeService{ID: "svc-agent", Name: "tierplan-agent", Mode: "global", Running: 3, Desired: 3, Image: "registry.test/tierplan/agent:1@" + updDigest('f')},
		dockertest.FakeService{ID: "svc-job", Name: "backup", Mode: "replicated-job", UpdateState: "completed", Image: "registry.test/team/backup:1@" + updDigest('e')})

	list := decode[updatedList](t, f.get(t, e, "services"))
	if len(list.Items) != 3 {
		t.Fatalf("items = %+v, want the three services", list.Items)
	}
	if u := updateOf(t, list, "tierplan-agent"); u != nil {
		t.Errorf("the agent service carries an update object: %+v", u)
	}
	if u := updateOf(t, list, "backup"); u != nil {
		t.Errorf("a job service carries an update object: %+v", u)
	}
	if u := updateOf(t, list, "web"); u == nil || u.State != "not_checked" {
		t.Errorf("a plain service never checked = %+v, want not_checked", u)
	}
	if got := heads.requests(); got != 0 {
		t.Errorf("listing asked the registry %d times, want 0: only Check now does", got)
	}
}

func TestServicesListWithoutUpdates(t *testing.T) {
	t.Parallel()
	f, e := serviceEnv(t, webService())
	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.code, rec.body)
	}
	if strings.Contains(string(rec.body), `"update":`) {
		t.Errorf("a list with update checks not wired carries an update object: %s", rec.body)
	}
	if got := decode[serviceList](t, rec); got.Total != 1 || got.Items[0].Name != "web" {
		t.Errorf("list = %+v, want the web service", got)
	}
}

// failingChecks is an update store that cannot be read.
type failingChecks struct{ updates.Store }

func (failingChecks) Checks(context.Context, []updates.Key) (map[updates.Key]updates.Check, error) {
	return nil, errors.New("disk I/O error")
}

func TestVerdictFailureNeverFailsList(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{})
	f := newEnvFix(t, swarmManagerEngine(webService()))
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads, f.updateStore = heads, failingChecks{}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)

	rec := f.get(t, e, "services")
	if rec.code != http.StatusOK {
		t.Fatalf("list: %d %s, want 200 though the verdicts could not be read", rec.code, rec.body)
	}
	got := decode[serviceList](t, rec)
	if got.Total != 1 || len(got.Items) != 1 {
		t.Fatalf("list = %+v, want the one service", got)
	}
	web := got.Items[0]
	if web.ID != "svc-web" || web.Name != "web" || web.Mode != "replicated" || web.Running != 2 || web.Desired != 2 || web.Image != scaleImage || web.Stack != "shop" {
		t.Errorf("web = %+v, want every other field present", web)
	}
	if strings.Contains(string(rec.body), `"update":`) {
		t.Errorf("an update object was served though the verdicts could not be read: %s", rec.body)
	}
	if n := strings.Count(f.logs.String(), "could not judge the images of the services"); n != 1 {
		t.Errorf("log lines about the failed verdicts = %d, want 1:\n%s", n, f.logs.String())
	}
}

// The containers list, the container detail and the dashboard count (plan 12-06, IMG-05, D-20): a
// standalone host whose fake daemon holds the containers and their images; the registry is the scripted
// Heads and Check now is the real route.

type containerRow struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Update *updateInfo `json:"update"`
}

type containerRows struct {
	Items []containerRow `json:"items"`
	Total int            `json:"total"`
}

func (c containerRows) of(t *testing.T, name string) *updateInfo {
	t.Helper()
	for _, it := range c.Items {
		if it.Name == name {
			return it.Update
		}
	}
	t.Fatalf("no container %q in %+v", name, c)
	return nil
}

func (c containerRows) has(name string) bool {
	for _, it := range c.Items {
		if it.Name == name {
			return true
		}
	}
	return false
}

// updImage is an image ID made of one repeated character.
func updImage(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// hostWithContainers is a standalone host running these containers, with the cluster reader a real server
// has, whose registry answers as heads does.
func hostWithContainers(t *testing.T, heads *scriptedHeads, inspect func(ids []string) map[string]string, containers []dockertest.FakeContainer, images []dockertest.FakeImage) (*envFix, envBody) {
	t.Helper()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "host-1", Version: "29.1.0", Containers: containers, Images: images})
	f.clusters = envs.NewClusters(f.svc, f.snaps, envs.ClusterOptions{Now: f.clk.now})
	f.svc.WithClusters(f.clusters)
	f.updateHeads = heads
	if inspect != nil {
		f.updateInspect = func(_ context.Context, _ string, ids []string) (map[string]string, error) { return inspect(ids), nil }
	}
	f.h = f.build(t)
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	return f, e
}

func appContainer(name, image, imageID string, labels map[string]string) dockertest.FakeContainer {
	return dockertest.FakeContainer{
		ID: "id-" + name, Names: []string{"/" + name}, Image: image, ImageID: imageID, State: "running", Status: "Up 1 hour", Labels: labels,
	}
}

func TestContainersCarryUpdateState(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{
		"team/app":   {digest: updDigest('b')},
		"team/other": {digest: updDigest('c')},
	})
	var inspected [][]string
	var mu sync.Mutex
	f, e := hostWithContainers(t, heads,
		func(ids []string) map[string]string {
			mu.Lock()
			defer mu.Unlock()
			inspected = append(inspected, ids)
			return map[string]string{"id-moved": "registry.test/team/other:2"}
		},
		[]dockertest.FakeContainer{
			// Running a digest the tag has left.
			appContainer("old", "registry.test/team/app:1", updImage('1'), nil),
			// The tag moved to another image since: the list shows the image ID, the inspect the name.
			appContainer("moved", updImage('2'), updImage('2'), nil),
			// A build nobody pushed, on the classic image store (no repo digest).
			appContainer("built", "local/build:1", updImage('3'), nil),
		},
		[]dockertest.FakeImage{
			{ID: updImage('1'), RepoTags: []string{"registry.test/team/app:1"}, RepoDigests: []string{"registry.test/team/app@" + updDigest('a')}},
			{ID: updImage('2'), RepoDigests: []string{"registry.test/team/other@" + updDigest('c')}},
			{ID: updImage('3'), RepoTags: []string{"local/build:1"}},
		})

	before := decode[containerRows](t, f.get(t, e, "containers"))
	if u := before.of(t, "old"); u == nil || u.State != "not_checked" {
		t.Errorf("old before any check = %+v, want not_checked", u)
	}
	if u := before.of(t, "built"); u == nil || u.State != "not_from_registry" {
		t.Errorf("built = %+v, want not_from_registry without any check", u)
	}

	rec := f.checkNow(t, e)
	if rec.code != http.StatusOK {
		t.Fatalf("check now on a standalone host: %d %s", rec.code, rec.body)
	}
	if got := decode[updateCheckAnswer](t, rec); got != (updateCheckAnswer{Checked: 2, Updates: 1}) {
		t.Errorf("check now = %+v, want 2 checked and 1 update", got)
	}
	if n := heads.requests(); n != 2 {
		t.Errorf("%d registry requests, want 2: the local build is never asked about", n)
	}

	list := decode[containerRows](t, f.get(t, e, "containers"))
	old := list.of(t, "old")
	if old == nil || old.State != "update_available" || old.RunningDigest != updDigest('a') || old.RemoteDigest != updDigest('b') ||
		old.Registry != "registry.test" || old.Repository != "team/app" || old.Tag != "1" {
		t.Errorf("old = %+v, want update_available, judged from the repo digest and not the image ID", old)
	}
	if u := list.of(t, "moved"); u == nil || u.State != "up_to_date" || u.Repository != "team/other" || u.Tag != "2" {
		t.Errorf("moved = %+v, want up_to_date for team/other:2, named by the inspect", u)
	}
	if u := list.of(t, "built"); u == nil || u.State != "not_from_registry" {
		t.Errorf("built = %+v, want not_from_registry", u)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ids := range inspected {
		if len(ids) != 1 || ids[0] != "id-moved" {
			t.Errorf("inspected %v, want only the container whose list row shows an image ID", ids)
		}
	}

	// The detail carries the same object.
	var detail struct {
		Update *updateInfo `json:"update"`
	}
	d := f.get(t, e, "containers/id-old")
	if d.code != http.StatusOK {
		t.Fatalf("detail: %d %s", d.code, d.body)
	}
	detail = decode[struct {
		Update *updateInfo `json:"update"`
	}](t, d)
	if detail.Update == nil || detail.Update.State != "update_available" || detail.Update.RemoteDigest != updDigest('b') {
		t.Errorf("detail update = %+v, want update_available", detail.Update)
	}
}

func TestSummaryCountsImageUpdates(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/app": {digest: updDigest('b')}})
	f, e := hostWithContainers(t, heads, nil,
		[]dockertest.FakeContainer{
			appContainer("old-1", "registry.test/team/app:1", updImage('1'), nil),
			appContainer("old-2", "registry.test/team/app:1", updImage('1'), nil),
			// A task container of a swarm service is its service's to count.
			appContainer("task", "registry.test/team/app:1", updImage('1'), map[string]string{"com.docker.swarm.service.id": "svc-1"}),
			appContainer("current", "registry.test/team/app:1", updImage('2'), nil),
		},
		[]dockertest.FakeImage{
			{ID: updImage('1'), RepoDigests: []string{"registry.test/team/app@" + updDigest('a')}},
			{ID: updImage('2'), RepoDigests: []string{"registry.test/team/app@" + updDigest('b')}},
		})

	type summary struct {
		ImageUpdates *int `json:"imageUpdates"`
	}
	if got := decode[summary](t, f.get(t, e, "summary")); got.ImageUpdates == nil || *got.ImageUpdates != 0 {
		t.Errorf("before any check imageUpdates = %v, want 0 (known, and nothing is known to wait)", got.ImageUpdates)
	}
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}
	got := decode[summary](t, f.get(t, e, "summary"))
	if got.ImageUpdates == nil || *got.ImageUpdates != 2 {
		t.Errorf("imageUpdates = %v, want 2: the two plain containers behind, not the task and not the current one", got.ImageUpdates)
	}
}

func TestSummaryImageUpdatesAbsentWithoutUpdateChecks(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "host-1", Version: "29.1.0"})
	e := f.add(t)
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "summary")
	if strings.Contains(string(rec.body), "imageUpdates") {
		t.Errorf("summary = %s, want no imageUpdates when update checks are not wired", rec.body)
	}
}

func TestContainersUpdatesFilter(t *testing.T) {
	t.Parallel()
	heads := newScriptedHeads(map[string]headAnswer{"team/app": {digest: updDigest('b')}})
	f, e := hostWithContainers(t, heads, nil,
		[]dockertest.FakeContainer{
			appContainer("old", "registry.test/team/app:1", updImage('1'), nil),
			appContainer("task", "registry.test/team/app:1", updImage('1'), map[string]string{"com.docker.swarm.service.id": "svc-1"}),
			appContainer("current", "registry.test/team/app:1", updImage('2'), nil),
			appContainer("built", "local/build:1", updImage('3'), nil),
		},
		[]dockertest.FakeImage{
			{ID: updImage('1'), RepoDigests: []string{"registry.test/team/app@" + updDigest('a')}},
			{ID: updImage('2'), RepoDigests: []string{"registry.test/team/app@" + updDigest('b')}},
			{ID: updImage('3')},
		})
	if rec := f.checkNow(t, e); rec.code != http.StatusOK {
		t.Fatalf("check now: %d %s", rec.code, rec.body)
	}

	all := decode[containerRows](t, f.get(t, e, "containers"))
	if all.Total != 4 {
		t.Fatalf("unfiltered total = %d, want 4", all.Total)
	}
	only := decode[containerRows](t, f.get(t, e, "containers?updates=available"))
	if only.Total != 2 || len(only.Items) != 2 || !only.has("old") || !only.has("task") {
		t.Errorf("updates=available = %+v, want the two containers behind and total 2", only)
	}
	if u := only.of(t, "task"); u == nil || !u.Task {
		t.Errorf("task = %+v, want task true", u)
	}
	// It combines with the search, and the search narrows it further.
	one := decode[containerRows](t, f.get(t, e, "containers?updates=available&q=old"))
	if one.Total != 1 || !one.has("old") {
		t.Errorf("updates=available&q=old = %+v, want only old", one)
	}
	if rec := f.get(t, e, "containers?updates=nonsense"); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("updates=nonsense = %d, want 422", rec.code)
	}
}
