// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// What uses an image (plan 05-15 task 1, RES-09, D-18, D-28). Docker is a fake daemon behind
// the real monitor, snapshot cache, cluster reader and aggregator.

func hexID(c string) string { return "sha256:" + strings.Repeat(c, 64) }

type usageItem struct {
	Node             string   `json:"node"`
	NodeID           string   `json:"nodeId"`
	ID               string   `json:"id"`
	RepoTags         []string `json:"repoTags"`
	Dangling         bool     `json:"dangling"`
	ReadOnly         bool     `json:"readOnly"`
	UsedByContainers int      `json:"usedByContainers"`
	UsedByServices   int      `json:"usedByServices"`
}

type usedByBody struct {
	Items []struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		NodeID    string `json:"nodeId"`
		Node      string `json:"node"`
		State     string `json:"state"`
		MatchedBy string `json:"matchedBy"`
	} `json:"items"`
	UsedByContainers int      `json:"usedByContainers"`
	UsedByServices   int      `json:"usedByServices"`
	NotReporting     []string `json:"notReporting"`
}

func imageUsageEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1",
		Images: []dockertest.FakeImage{
			{ID: hexID("a"), RepoTags: []string{"nginx:1.27"}, Created: 30},
			{ID: hexID("b"), RepoTags: []string{"redis:8"}, Created: 20},
			{ID: hexID("c"), Created: 10},
			{ID: hexID("d"), RepoTags: []string{"busybox:1"}, Created: 5},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c-web", Names: []string{"/web"}, Image: "nginx:1.27", ImageID: hexID("a"), State: "running"},
			{ID: "c-old", Names: []string{"/old"}, Image: "nginx:1.27", ImageID: hexID("a"), State: "exited"},
			{ID: "c-cache", Names: []string{"/cache"}, Image: "redis:8", ImageID: hexID("b"), State: "running"},
			// Created from nginx:1.27 when that tag still named another image: it is the
			// untagged one that it uses now.
			{ID: "c-drift", Names: []string{"/drift"}, Image: "nginx:1.27", ImageID: hexID("c"), State: "running"},
		},
	}
}

func TestImageUsageTheListCountsContainersByImageIDWhetherTheyRunOrNot(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, imageUsageEngine())
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "images")
	if rec.code != http.StatusOK {
		t.Fatalf("images: %d %s", rec.code, rec.body)
	}
	list := decode[listBody[usageItem]](t, rec)
	byID := map[string]usageItem{}
	for _, it := range list.Items {
		byID[it.ID] = it
	}
	for id, want := range map[string][2]int{hexID("a"): {2, 0}, hexID("b"): {1, 0}, hexID("c"): {1, 0}, hexID("d"): {0, 0}} {
		if got := byID[id]; got.UsedByContainers != want[0] || got.UsedByServices != want[1] {
			t.Errorf("%s: %d containers, %d services, want %v", id[:14], got.UsedByContainers, got.UsedByServices, want)
		}
	}
	if c := byID[hexID("c")]; !c.Dangling || !slices.Equal(c.RepoTags, []string{"<none>"}) {
		t.Errorf("the untagged image = %+v, want dangling", c)
	}
	if byID[hexID("a")].Dangling || byID[hexID("a")].NodeID != e.ID || byID[hexID("a")].ReadOnly {
		t.Errorf("the tagged image = %+v", byID[hexID("a")])
	}
}

func TestImageUsedByListsEachContainerOfTheImageByNameAndNothingForAnUnusedImage(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, imageUsageEngine())
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "images/"+hexID("a")+"/used-by")
	if rec.code != http.StatusOK {
		t.Fatalf("used-by: %d %s", rec.code, rec.body)
	}
	got := decode[usedByBody](t, rec)
	if got.UsedByContainers != 2 || got.UsedByServices != 0 || len(got.Items) != 2 {
		t.Fatalf("used-by = %+v, want the two containers", got)
	}
	// By name: old (stopped) then web.
	if u := got.Items[0]; u.Kind != "container" || u.Name != "old" || u.ID != "c-old" || u.State != "exited" || u.MatchedBy != "image_id" || u.Node != "node-1" || u.NodeID != e.ID {
		t.Errorf("items[0] = %+v", u)
	}
	if u := got.Items[1]; u.Name != "web" || u.State != "running" || u.MatchedBy != "image_id" {
		t.Errorf("items[1] = %+v", u)
	}
	if got.NotReporting == nil {
		t.Error("notReporting is null, want an empty list")
	}

	none := decode[usedByBody](t, f.get(t, e, "images/"+hexID("d")+"/used-by"))
	if none.Items == nil || len(none.Items) != 0 || none.UsedByContainers != 0 {
		t.Errorf("an unused image answers %+v, want an empty list and not null", none)
	}
}

func TestImageUsedByAnIDOfTheWrongShapeIs422AnUnknownImageIs404AndAForeignNodeIs503(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, imageUsageEngine())
	awaitOnline(t, f.mon, e.ID)
	if rec := f.get(t, e, "images/nginx/used-by"); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a name: %d, want 422: %s", rec.code, rec.body)
	}
	if rec := f.get(t, e, "images/"+imgMissing+"/used-by"); rec.code != http.StatusNotFound || rec.ptyp != "image_not_found" {
		t.Errorf("unknown image: %d %q, want 404 image_not_found", rec.code, rec.ptyp)
	}
	if rec := f.get(t, e, "images/"+hexID("a")+"/used-by?node=nobody"); rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Errorf("foreign node: %d %q, want 503 node_unavailable", rec.code, rec.ptyp)
	}
	if rec := f.get(t, e, "images/"+hexID("a")+"/used-by?node="+e.ID); rec.code != http.StatusOK {
		t.Errorf("the node named: %d %s", rec.code, rec.body)
	}
}

func TestImageUsedByNeedsImagesRead(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, imageUsageEngine())
	f.addUser(t, "stranger", false)
	cookie, csrf := f.signIn(t, "stranger", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/images/" + hexID("a") + "/used-by", cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("a user with no role: %d, want 403 or 404", rec.Code)
	}
}

// A swarm of two managers and a worker: the services are on the managers' specs.

var (
	usageNginx  = hexID("1")
	usageOld    = hexID("2")
	usageNew    = hexID("3")
	usageShort  = hexID("4")
	pinDigest   = "sha256:" + strings.Repeat("d", 64)
	shortDigest = "sha256:" + strings.Repeat("e", 64)
)

func usageSwarm(t *testing.T) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if host == "wrk-1" {
			return
		}
		cfg.Images = []dockertest.FakeImage{
			{ID: usageNginx, RepoTags: []string{"nginx:1.27"}, Created: 40},
			// app:2 moved on to a new image; the old one keeps its digest.
			{ID: usageOld, RepoDigests: []string{"app@" + pinDigest}, Created: 30},
			{ID: usageNew, RepoTags: []string{"app:2"}, RepoDigests: []string{"app@sha256:" + strings.Repeat("f", 64)}, Created: 35},
			{ID: usageShort, RepoDigests: []string{"docker.io/library/docker@" + shortDigest}, Created: 20},
		}
		cfg.Containers = append(cfg.Containers, dockertest.FakeContainer{ID: "c-" + host, Names: []string{"/proxy"}, ImageID: usageNginx, State: "exited"})
		cfg.ServiceRows = append(cfg.ServiceRows,
			// Scaled to zero, and written in the short form of the tag.
			dockertest.FakeService{ID: "svc-tag", Name: "tagged", Mode: "replicated", Image: "nginx:1.27"},
			// Pinned: the tag has moved, the digest has not.
			dockertest.FakeService{ID: "svc-pin", Name: "pinned", Mode: "replicated", Running: 2, Desired: 2, Image: "app:2@" + pinDigest},
			dockertest.FakeService{ID: "svc-short", Name: "short", Mode: "replicated", Running: 0, Desired: 1, Image: "docker@" + shortDigest},
		)
	})
}

func TestImageUsageAServiceCountsFromItsSpecEvenWithNoTasksByDigestElseByTag(t *testing.T) {
	t.Parallel()
	s := usageSwarm(t)
	// usageSwarm returned with every agent connected and probed; the request makes the cluster
	// read behind "used by" itself, so the list is read once.
	rec := s.get(t, s.env.ID, "images?view=node")
	if rec.code != http.StatusOK {
		t.Fatalf("images: %d %s", rec.code, rec.body)
	}
	byNode := map[string]usageItem{}
	for _, it := range decode[listBody[usageItem]](t, rec).Items {
		byNode[it.Node+" "+it.ID] = it
	}
	for _, node := range []string{"mgr-1", "mgr-2"} {
		want := map[string][2]int{usageNginx: {1, 1}, usageOld: {0, 1}, usageNew: {0, 0}, usageShort: {0, 1}}
		for id, w := range want {
			if got := byNode[node+" "+id]; got.UsedByContainers != w[0] || got.UsedByServices != w[1] {
				t.Errorf("%s %s: %d containers, %d services, want %v", node, id[:14], got.UsedByContainers, got.UsedByServices, w)
			}
		}
	}

	rec = s.get(t, s.env.ID, "images/"+usageNginx+"/used-by")
	if rec.code != http.StatusOK {
		t.Fatalf("used-by: %d %s", rec.code, rec.body)
	}
	got := decode[usedByBody](t, rec)
	// The proxy container of each manager, then the service by name.
	if got.UsedByContainers != 2 || got.UsedByServices != 1 || len(got.Items) != 3 {
		t.Fatalf("used-by = %+v", got)
	}
	if u := got.Items[2]; u.Kind != "service" || u.Name != "tagged" || u.State != "no_tasks" || u.MatchedBy != "tag" || u.Node != "" {
		t.Errorf("the service = %+v, want tagged, no_tasks, matched by tag", u)
	}

	old := decode[usedByBody](t, s.get(t, s.env.ID, "images/"+usageOld+"/used-by"))
	if len(old.Items) != 1 || old.Items[0].Name != "pinned" || old.Items[0].MatchedBy != "digest" || old.Items[0].State != "running" {
		t.Errorf("the image the pin names = %+v, want pinned by digest, running", old.Items)
	}
	fresh := decode[usedByBody](t, s.get(t, s.env.ID, "images/"+usageNew+"/used-by"))
	if len(fresh.Items) != 0 {
		t.Errorf("the image the tag moved to = %+v, want nothing: a pinned service ignores the tag", fresh.Items)
	}
	short := decode[usedByBody](t, s.get(t, s.env.ID, "images/"+usageShort+"/used-by"))
	if len(short.Items) != 1 || short.Items[0].Name != "short" || short.Items[0].MatchedBy != "digest" || short.Items[0].State != "starting" {
		t.Errorf("the short digest form = %+v, want short matched by digest, starting", short.Items)
	}
}
