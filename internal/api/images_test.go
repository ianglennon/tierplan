// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
)

// Image detail (plan 05-14, RES-09, D-18). Docker is a fake daemon behind the real monitor,
// snapshot cache and aggregator; the router and policy are the real ones.

var (
	imgID      = "sha256:" + strings.Repeat("a", 64)
	imgDangle  = "sha256:" + strings.Repeat("d", 64)
	imgDigest  = "nginx@sha256:" + strings.Repeat("c", 64)
	imgMissing = "sha256:" + strings.Repeat("9", 64)
)

func imageDetailEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Images: []dockertest.FakeImage{
			{ID: imgID, RepoTags: []string{"nginx:latest", "nginx:1.27"}, RepoDigests: []string{imgDigest},
				Size: 4096, Created: 1788000000, Architecture: "arm64", Variant: "v8",
				Labels: map[string]string{"maintainer": "ops", "org.opencontainers.image.title": "nginx"}},
			{ID: imgDangle, Size: 7, Created: 1788000100},
		},
	}
}

type imageDetail struct {
	ID       string `json:"id"`
	ShortID  string `json:"shortId"`
	Dangling bool   `json:"dangling"`
	Names    []struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"names"`
	Size         int64             `json:"size"`
	Created      string            `json:"created"`
	OS           string            `json:"os"`
	Architecture string            `json:"architecture"`
	Variant      string            `json:"variant"`
	Labels       map[string]string `json:"labels"`
	Nodes        []struct {
		NodeID  string   `json:"nodeId"`
		Name    string   `json:"name"`
		Present bool     `json:"present"`
		Names   []string `json:"names"`
	} `json:"nodes"`
	NotReporting []string `json:"notReporting"`
}

func TestImageDetailAViewerSeesTheNamesDetailsLabelsAndTheNodeThatHasIt(t *testing.T) {
	f, e := dashEnv(t, imageDetailEngine())
	get := f.asRole(t, e, "img-viewer", "viewer")

	rec := get("images/" + imgID)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body)
	}
	var d imageDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != imgID || d.ShortID != strings.Repeat("a", 12) || d.Dangling || d.Size != 4096 ||
		d.OS != "linux" || d.Architecture != "arm64" || d.Variant != "v8" || d.Created == "" {
		t.Errorf("details = %+v", d)
	}
	var names []string
	for _, n := range d.Names {
		names = append(names, n.Kind+" "+n.Name)
	}
	if !slices.Equal(names, []string{"tag nginx:1.27", "tag nginx:latest", "digest " + imgDigest}) {
		t.Errorf("names = %v, want the tags then the digest", names)
	}
	if d.Labels["maintainer"] != "ops" || len(d.Labels) != 2 {
		t.Errorf("labels = %v", d.Labels)
	}
	if len(d.Nodes) != 1 || d.Nodes[0].NodeID != e.ID || d.Nodes[0].Name != "node-1" || !d.Nodes[0].Present ||
		!slices.Equal(d.Nodes[0].Names, []string{"nginx:1.27", "nginx:latest", imgDigest}) {
		t.Errorf("nodes = %+v", d.Nodes)
	}
	if d.NotReporting == nil || len(d.NotReporting) != 0 {
		t.Errorf("notReporting = %v, want an empty list", d.NotReporting)
	}
}

func TestImageDetailAnImageWithNoTagIsDanglingWithNoNames(t *testing.T) {
	f, e := dashEnv(t, imageDetailEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.get(t, e, "images/"+imgDangle)
	if rec.code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.code, rec.body)
	}
	d := decode[imageDetail](t, rec)
	if !d.Dangling || len(d.Names) != 0 || d.Names == nil || d.Labels == nil || d.Variant != "" || d.Architecture != "amd64" {
		t.Errorf("detail = %+v, want dangling with no names, empty labels and no variant", d)
	}
}

func TestImageDetailAnIDOfTheWrongShapeIs422AnUnknownImageIs404AndANodeThatIsNotTheEnvironmentsIs503(t *testing.T) {
	f, e := dashEnv(t, imageDetailEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	for _, bad := range []string{"nginx", "sha256:abc", strings.ToUpper(imgID), imgID + "0", "sha256:" + strings.Repeat("g", 64)} {
		if rec := f.get(t, e, "images/"+bad); rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%q: %d, want 422: %s", bad, rec.code, rec.body)
		}
	}
	if rec := f.get(t, e, "images/"+imgMissing); rec.code != http.StatusNotFound || rec.ptyp != "image_not_found" {
		t.Errorf("unknown image: %d %q, want 404 image_not_found: %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.get(t, e, "images/"+imgID+"?node=nobody"); rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Errorf("unknown node: %d %q, want 503 node_unavailable: %s", rec.code, rec.ptyp, rec.body)
	}
	if rec := f.get(t, e, "images/"+imgID+"?node="+e.ID); rec.code != http.StatusOK {
		t.Errorf("the node named: %d %s", rec.code, rec.body)
	}
	// The pull routes keep their own paths.
	if rec := f.get(t, e, "images/pull-targets"); rec.code == http.StatusUnprocessableEntity || rec.ptyp == "image_not_found" {
		t.Errorf("pull-targets was routed to an image: %d %s", rec.code, rec.body)
	}
}

func TestImageDetailNeedsImagesRead(t *testing.T) {
	f, e := dashEnv(t, imageDetailEngine())
	f.addUser(t, "stranger", false)
	cookie, csrf := f.signIn(t, "stranger", false)
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/env/" + e.ID + "/images/" + imgID, cookie: cookie, csrf: csrf})
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("a user with no role: %d, want 403 or 404", rec.Code)
	}
}

func TestSwarmImageDetailNamesEveryNodeAndSaysWhichDoNotHaveIt(t *testing.T) {
	s := newSwarmFix(t)
	id := "sha256:" + strings.Repeat("a", 64)
	var got imageDetail
	waitFor(t, "the image on both managers", func() bool {
		rec := s.get(t, s.env.ID, "images/"+id)
		if rec.code != http.StatusOK {
			return false
		}
		got = decode[imageDetail](t, rec)
		return len(got.Nodes) == 3
	})
	var present, absent []string
	for _, n := range got.Nodes {
		if n.Present {
			present = append(present, n.Name)
		} else {
			absent = append(absent, n.Name)
			if len(n.Names) != 0 || n.Names == nil {
				t.Errorf("%s names = %v, want an empty list", n.Name, n.Names)
			}
		}
	}
	if !slices.Equal(present, []string{"mgr-1", "mgr-2"}) || !slices.Equal(absent, []string{"wrk-1"}) {
		t.Errorf("present %v, absent %v", present, absent)
	}
	if len(got.Names) != 1 || got.Names[0].Name != "nginx:1" || got.Names[0].Kind != "tag" {
		t.Errorf("names = %+v, want nginx:1 once", got.Names)
	}

	// node limits the read to that node.
	one := decode[imageDetail](t, s.get(t, s.env.ID, "images/"+id+"?node="+s.m2.id))
	if len(one.Nodes) != 1 || one.Nodes[0].NodeID != s.m2.id || !one.Nodes[0].Present {
		t.Errorf("node= nodes = %+v", one.Nodes)
	}
	// The worker is connected but does not have it.
	if rec := s.get(t, s.env.ID, "images/"+id+"?node="+s.w.id); rec.code != http.StatusNotFound || rec.ptyp != "image_not_found" {
		t.Errorf("the worker: %d %q, want 404 image_not_found", rec.code, rec.ptyp)
	}
	if rec := s.get(t, s.env.ID, "images/"+id+"?node=nobody"); rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
		t.Errorf("unknown node: %d %q, want 503 node_unavailable", rec.code, rec.ptyp)
	}
}

func TestImageDetailAnEncodedColonInThePathIsTheSameImage(t *testing.T) {
	f, e := dashEnv(t, imageDetailEngine())
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
	rec := f.get(t, e, "images/"+strings.Replace(imgID, ":", "%3A", 1))
	if rec.code != http.StatusOK {
		t.Fatalf("encoded colon: %d %s", rec.code, rec.body)
	}
}
