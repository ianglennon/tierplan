// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"fmt"
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
	awaitOnline(t, f.mon, e.ID)
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
	awaitOnline(t, f.mon, e.ID)
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
	s.awaitNodesProbed(t)
	rec := s.get(t, s.env.ID, "images/"+id)
	if rec.code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.code, rec.body)
	}
	got := decode[imageDetail](t, rec)
	if len(got.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want the three nodes", got.Nodes)
	}
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
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "images/"+strings.Replace(imgID, ":", "%3A", 1))
	if rec.code != http.StatusOK {
		t.Fatalf("encoded colon: %d %s", rec.code, rec.body)
	}
}

// The Inspect tab and the Layers tab (plan 05-14 task 2, D-18, T-05-47).

const imageSecret = "hunter2-baked-into-the-image"

var imgEnv = "sha256:" + strings.Repeat("e", 64)

func imageInspectEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Images: []dockertest.FakeImage{
			{ID: imgEnv, RepoTags: []string{"app:1"}, Size: 300, Created: 1788000000,
				Env: []string{"API_KEY=" + imageSecret, "PLAIN=1"},
				History: []dockertest.FakeLayer{
					{ID: imgEnv, CreatedBy: `CMD ["serve"]`, Size: 0, Created: 1788000000},
					{ID: "<missing>", CreatedBy: "RUN echo " + "build", Comment: "buildkit.dockerfile.v0", Size: 300, Created: 1787999000},
				}},
			{ID: imgDangle, Size: 7},
		},
	}
}

type imageInspect struct {
	Masked  bool           `json:"masked"`
	Inspect map[string]any `json:"inspect"`
}

type imageHistory struct {
	Layers []struct {
		ID        string `json:"id"`
		CreatedBy string `json:"createdBy"`
		Size      int64  `json:"size"`
		Created   string `json:"created"`
		Comment   string `json:"comment"`
	} `json:"layers"`
}

func envOf(t *testing.T, doc map[string]any, section string) []any {
	t.Helper()
	cfg, ok := doc[section].(map[string]any)
	if !ok {
		t.Fatalf("no %s in %v", section, doc)
	}
	env, _ := cfg["Env"].([]any)
	return env
}

func TestImageInspectAViewerGetsTheDocumentWithEveryEnvValueReplacedAndMaskedSaysSo(t *testing.T) {
	f, e := dashEnv(t, imageInspectEngine())
	get := f.asRole(t, e, "img-viewer", "viewer")
	rec := get("images/" + imgEnv + "/inspect")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), imageSecret) {
		t.Fatalf("a viewer's response carries the baked-in secret: %s", rec.Body)
	}
	var got imageInspect
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Masked || got.Inspect["Id"] != imgEnv {
		t.Errorf("masked=%v id=%v", got.Masked, got.Inspect["Id"])
	}
	for _, section := range []string{"Config", "ContainerConfig"} {
		env := envOf(t, got.Inspect, section)
		if len(env) != 2 || env[0] != "API_KEY=<hidden>" || env[1] != "PLAIN=<hidden>" {
			t.Errorf("%s.Env = %v, want every value hidden", section, env)
		}
	}
}

func TestImageInspectAHolderOfSecretsRevealGetsTheDocumentAsTheEngineSentIt(t *testing.T) {
	f, e := dashEnv(t, imageInspectEngine())
	get := f.asRole(t, e, "img-admin", "admin")
	rec := get("images/" + imgEnv + "/inspect")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body)
	}
	var got imageInspect
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, got.Inspect, "Config")
	if got.Masked || len(env) != 2 || env[0] != "API_KEY="+imageSecret {
		t.Errorf("masked=%v Config.Env=%v, want the values as the engine sent them", got.Masked, env)
	}
}

func TestImageInspectAnImageWithNoEnvIsNotMasked(t *testing.T) {
	f, e := dashEnv(t, imageInspectEngine())
	get := f.asRole(t, e, "img-viewer2", "viewer")
	rec := get("images/" + imgDangle + "/inspect")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body)
	}
	if got := decode[imageInspect](t, &envRec{code: rec.Code, body: rec.Body.Bytes()}); got.Masked {
		t.Errorf("masked = true for an image with no Env")
	}
}

func TestImageInspectAndHistoryUseTheDetailRoutesRulesForTheIDAndTheNode(t *testing.T) {
	f, e := dashEnv(t, imageInspectEngine())
	awaitOnline(t, f.mon, e.ID)
	for _, what := range []string{"inspect", "history"} {
		if rec := f.get(t, e, "images/nginx/"+what); rec.code != http.StatusNotFound && rec.code != http.StatusUnprocessableEntity {
			t.Errorf("%s of a bad path: %d", what, rec.code)
		}
		if rec := f.get(t, e, "images/"+imgMissing+"/"+what); rec.code != http.StatusNotFound || rec.ptyp != "image_not_found" {
			t.Errorf("%s unknown: %d %q, want 404 image_not_found: %s", what, rec.code, rec.ptyp, rec.body)
		}
		if rec := f.get(t, e, "images/"+imgEnv+"/"+what+"?node=nobody"); rec.code != http.StatusServiceUnavailable || rec.ptyp != "node_unavailable" {
			t.Errorf("%s unknown node: %d %q, want 503 node_unavailable: %s", what, rec.code, rec.ptyp, rec.body)
		}
		if rec := f.get(t, e, "images/"+imgEnv+"/"+what+"?node="+e.ID); rec.code != http.StatusOK {
			t.Errorf("%s the node named: %d %s", what, rec.code, rec.body)
		}
	}
}

func TestImageHistoryAnswersTheLayersInTheOrderTheEngineGaveThem(t *testing.T) {
	f, e := dashEnv(t, imageInspectEngine())
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "images/"+imgEnv+"/history")
	if rec.code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.code, rec.body)
	}
	h := decode[imageHistory](t, rec)
	if len(h.Layers) != 2 {
		t.Fatalf("layers = %+v", h.Layers)
	}
	if l := h.Layers[0]; l.ID != imgEnv || l.CreatedBy != `CMD ["serve"]` || l.Size != 0 || l.Created == "" {
		t.Errorf("layer 0 = %+v", l)
	}
	if l := h.Layers[1]; l.ID != "<missing>" || l.CreatedBy != "RUN echo build" || l.Comment != "buildkit.dockerfile.v0" || l.Size != 300 {
		t.Errorf("layer 1 = %+v", l)
	}
}

func TestImageHistoryFromAnAgentThatRefusesTheRouteIsAgentPolicyDenied(t *testing.T) {
	cfg := imageInspectEngine()
	cfg.HistoryDeny = true
	f, e := dashEnv(t, cfg)
	awaitOnline(t, f.mon, e.ID)
	rec := f.get(t, e, "images/"+imgEnv+"/history")
	if rec.code != http.StatusBadGateway || rec.ptyp != "agent_policy_denied" {
		t.Errorf("history: %d %q, want 502 agent_policy_denied: %s", rec.code, rec.ptyp, rec.body)
	}
	// The other image routes still answer.
	if rec := f.get(t, e, "images/"+imgEnv+"/inspect"); rec.code != http.StatusOK {
		t.Errorf("inspect: %d %s", rec.code, rec.body)
	}
}

func TestSwarmImageInspectAndHistoryReadTheFirstNodeThatHasItOrTheNodeNamed(t *testing.T) {
	s := newSwarmFix(t)
	id := "sha256:" + strings.Repeat("a", 64)
	s.awaitNodesProbed(t)
	if rec := s.get(t, s.env.ID, "images/"+id+"/history"); rec.code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.code, rec.body)
	}
	if rec := s.get(t, s.env.ID, "images/"+id+"/inspect"); rec.code != http.StatusOK {
		t.Errorf("inspect: %d %s", rec.code, rec.body)
	}
	if rec := s.get(t, s.env.ID, "images/"+id+"/inspect?node="+s.m2.id); rec.code != http.StatusOK {
		t.Errorf("inspect on m2: %d %s", rec.code, rec.body)
	}
	// The worker is connected but does not have it.
	if rec := s.get(t, s.env.ID, "images/"+id+"/history?node="+s.w.id); rec.code != http.StatusNotFound || rec.ptyp != "image_not_found" {
		t.Errorf("history on the worker: %d %q, want 404 image_not_found", rec.code, rec.ptyp)
	}
}

// The merged swarm list, the Per node view, the Dangling filter and the search (plan 05-15
// task 2, D-16, D-17). The engines are fakes behind the real aggregator.

type listNode struct {
	NodeID     string `json:"nodeId"`
	Name       string `json:"name"`
	Containers int    `json:"containers"`
}

type listedImage struct {
	Node             string     `json:"node"`
	NodeID           string     `json:"nodeId"`
	ID               string     `json:"id"`
	RepoTags         []string   `json:"repoTags"`
	Dangling         bool       `json:"dangling"`
	ReadOnly         bool       `json:"readOnly"`
	Created          string     `json:"created"`
	UsedByContainers int        `json:"usedByContainers"`
	UsedByServices   int        `json:"usedByServices"`
	Nodes            []listNode `json:"nodes"`
}

type imageList struct {
	Items           []listedImage `json:"items"`
	Total           int           `json:"total"`
	Truncated       bool          `json:"truncated"`
	NodeCount       int           `json:"nodeCount"`
	DanglingCount   int           `json:"danglingCount"`
	UnfilteredTotal int           `json:"unfilteredTotal"`
	NodeRefs        []listNode    `json:"nodeRefs"`
	NotReporting    []string      `json:"notReporting"`
}

var (
	listWeb   = hexID("1")
	listDrift = hexID("2")
	listAlt   = hexID("3")
)

// listSwarm is two managers holding nginx:1.27 under one ID, and a worker holding the same tag
// under another: a drifted node.
func listSwarm(t *testing.T) *swarmFix {
	t.Helper()
	return newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		if host == "wrk-1" {
			cfg.Images = []dockertest.FakeImage{
				{ID: listDrift, RepoTags: []string{"nginx:1.27"}, Created: 50},
				{ID: listAlt, Created: 10},
			}
			return
		}
		cfg.Images = []dockertest.FakeImage{
			{ID: listWeb, RepoTags: []string{"nginx:1.27", "nginx:latest"}, Created: 40, Size: 100},
			{ID: listAlt, Created: 10},
		}
		cfg.Containers = append(cfg.Containers,
			dockertest.FakeContainer{ID: "c-" + host, Names: []string{"/proxy"}, ImageID: listWeb, State: "exited"})
	})
}

// awaitNodesProbed waits until the monitor has probed the engine of every node and found it
// online. A read of the images of the cluster calls each node through the monitor's client,
// which is ErrOffline until that node's first probe has finished.
func (s *swarmFix) awaitNodesProbed(t *testing.T) {
	t.Helper()
	awaitProbed(t, s.mon, envs.AgentTarget(s.m1.id), envs.AgentTarget(s.m2.id), envs.AgentTarget(s.w.id))
}

func (s *swarmFix) imageList(t *testing.T, query string) imageList {
	t.Helper()
	rec := s.get(t, s.env.ID, "images"+query)
	if rec.code != http.StatusOK {
		t.Fatalf("images%s: %d %s", query, rec.code, rec.body)
	}
	return decode[imageList](t, rec)
}

func TestImageListMergesNodeCopiesByIDAndKeepsTheSameTagWithAnotherIDAsASecondRow(t *testing.T) {
	s := listSwarm(t)
	s.awaitNodesProbed(t)
	got := s.imageList(t, "")
	if got.Total != 3 {
		t.Fatalf("total = %d, want 3: %+v", got.Total, got.Items)
	}
	// Newest first: the drifted copy (50), the managers' nginx (40), the untagged image (10).
	if len(got.Items) != 3 || got.Items[0].ID != listDrift || got.Items[1].ID != listWeb || got.Items[2].ID != listAlt {
		t.Fatalf("items = %+v, want drift, web, alt", got.Items)
	}
	web := got.Items[1]
	if len(web.Nodes) != 2 || web.Nodes[0].Name != "mgr-1" || web.Nodes[1].Name != "mgr-2" || got.NodeCount != 3 {
		t.Errorf("web nodes = %+v of %d, want mgr-1 and mgr-2 of 3", web.Nodes, got.NodeCount)
	}
	if web.Node != "" || web.NodeID != "" {
		t.Errorf("a row of two nodes names one: %q %q", web.Node, web.NodeID)
	}
	if !slices.Equal(web.RepoTags, []string{"nginx:1.27", "nginx:latest"}) || web.UsedByContainers != 2 {
		t.Errorf("web = %+v, want both tags and a stopped container on each manager", web)
	}
	for _, n := range web.Nodes {
		if n.Containers != 1 {
			t.Errorf("node %s has %d containers, want 1", n.Name, n.Containers)
		}
	}
	drift := got.Items[0]
	if len(drift.Nodes) != 1 || drift.Nodes[0].Name != "wrk-1" || drift.Node != "wrk-1" || drift.NodeID != s.w.id {
		t.Errorf("drift = %+v, want the one node wrk-1", drift)
	}
	if alt := got.Items[2]; len(alt.Nodes) != 3 || !alt.Dangling || !slices.Equal(alt.RepoTags, []string{"<none>"}) {
		t.Errorf("alt = %+v, want an untagged image on all three nodes", alt)
	}
	if len(got.NodeRefs) != 3 || got.NotReporting == nil {
		t.Errorf("nodeRefs = %+v notReporting = %v", got.NodeRefs, got.NotReporting)
	}
	if got.UnfilteredTotal != 3 || got.DanglingCount != 1 {
		t.Errorf("unfilteredTotal %d danglingCount %d, want 3 and 1", got.UnfilteredTotal, got.DanglingCount)
	}
}

func TestImageListPerNodeViewHasOneRowPerNodeCopyAndTheNodeFilterNarrowsIt(t *testing.T) {
	s := listSwarm(t)
	s.awaitNodesProbed(t)
	got := s.imageList(t, "?view=node")
	if got.Total != 6 {
		t.Fatalf("total = %d, want 6: %+v", got.Total, got.Items)
	}
	for _, it := range got.Items {
		if it.NodeID == "" || it.Node == "" || len(it.Nodes) != 1 || it.Nodes[0].NodeID != it.NodeID {
			t.Errorf("row %+v, want exactly the node it is on", it)
		}
	}
	one := s.imageList(t, "?view=node&node=mgr-2")
	if one.Total != 2 || one.Items[0].Node != "mgr-2" || one.Items[0].UsedByContainers != 1 {
		t.Errorf("node=mgr-2: %+v", one)
	}
	// Merged and narrowed to a node: the rows are that node's copies, and the node count is still the cluster's.
	merged := s.imageList(t, "?node=wrk-1")
	if merged.Total != 2 || merged.NodeCount != 3 {
		t.Errorf("merged on wrk-1: total %d nodeCount %d", merged.Total, merged.NodeCount)
	}
	if rec := s.get(t, s.env.ID, "images?view=sideways"); rec.code != http.StatusUnprocessableEntity {
		t.Errorf("a view that is not merged or node: %d, want 422", rec.code)
	}
}

func TestImageListSearchMatchesTagRepositoryOrIDPrefixAndDanglingKeepsUntaggedRows(t *testing.T) {
	s := listSwarm(t)
	s.awaitNodesProbed(t)
	for q, want := range map[string]int{
		"nginx":                            2,
		"NGINX:LATEST":                     1,
		"1.27":                             2,
		"sha256:" + strings.Repeat("1", 8): 1,
		strings.Repeat("3", 6):             1,
		"nothing-like-this":                0,
	} {
		if got := s.imageList(t, "?q="+q); got.Total != want {
			t.Errorf("q=%q: total %d, want %d", q, got.Total, want)
		}
	}
	dangling := s.imageList(t, "?dangling=1")
	if dangling.Total != 1 || dangling.Items[0].ID != listAlt {
		t.Errorf("dangling=1: %+v, want only the untagged image", dangling.Items)
	}
	// The counts are taken before the search and the dangling filter.
	if dangling.UnfilteredTotal != 3 || dangling.DanglingCount != 1 {
		t.Errorf("dangling=1 counts: unfiltered %d dangling %d", dangling.UnfilteredTotal, dangling.DanglingCount)
	}
	none := s.imageList(t, "?q=nginx&dangling=1")
	if none.Total != 0 || none.Items == nil || len(none.Items) != 0 || none.UnfilteredTotal != 3 {
		t.Errorf("nginx and dangling: %+v, want no rows and an empty list", none)
	}
}

func TestImageListFiltersRunBeforeTheCapAndRowsSortNewestFirstThenByID(t *testing.T) {
	var imgs []dockertest.FakeImage
	// 501 untagged images made later than the one tag, so the cap alone would hide it.
	for i := range 501 {
		imgs = append(imgs, dockertest.FakeImage{ID: "sha256:" + strings.Repeat("0", 60) + fmt.Sprintf("%04x", i), Created: 1000})
	}
	imgs = append(imgs, dockertest.FakeImage{ID: hexID("f"), RepoTags: []string{"needle:1"}, Created: 1})
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1", Images: imgs})
	awaitOnline(t, f.mon, e.ID)

	all := decode[imageList](t, f.get(t, e, "images"))
	if all.Total != 502 || !all.Truncated || len(all.Items) != 500 || all.UnfilteredTotal != 502 || all.DanglingCount != 501 {
		t.Fatalf("all: total %d truncated %v items %d unfiltered %d dangling %d", all.Total, all.Truncated, len(all.Items), all.UnfilteredTotal, all.DanglingCount)
	}
	// Equal creation times keep a stable order, by image ID.
	if all.Items[0].ID >= all.Items[1].ID || all.Items[0].ID != "sha256:"+strings.Repeat("0", 60)+"0000" {
		t.Errorf("ties = %s, %s, want the lower ID first", all.Items[0].ID, all.Items[1].ID)
	}
	found := decode[imageList](t, f.get(t, e, "images?q=needle"))
	if found.Total != 1 || found.Truncated || found.Items[0].ID != hexID("f") {
		t.Errorf("q=needle: %+v, want the one tagged image even though the cap hides it unfiltered", found)
	}
	dangling := decode[imageList](t, f.get(t, e, "images?dangling=1"))
	if dangling.Total != 501 || !dangling.Truncated || len(dangling.Items) != 500 {
		t.Errorf("dangling=1: total %d items %d, want 501 and 500", dangling.Total, len(dangling.Items))
	}
}

func TestImageListWithNoImagesIsEmptyNotNull(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	awaitOnline(t, f.mon, e.ID)
	got := decode[imageList](t, f.get(t, e, "images"))
	if got.Items == nil || len(got.Items) != 0 || got.Total != 0 || got.UnfilteredTotal != 0 || got.NodeCount != 1 {
		t.Errorf("empty = %+v", got)
	}
}

// A Viewer cannot read from the image's other documents what the inspect hides: its history, its
// command and its labels are withheld as its environment is (CR-B02, WR-A04).

func imageSecretsEngine() dockertest.FakeConfig {
	cfg := imageInspectEngine()
	cfg.Images[0].Cmd = []string{"serve", "--password=" + imageSecret}
	cfg.Images[0].Entrypoint = []string{"/entry", "--token=" + imageSecret}
	cfg.Images[0].Labels = map[string]string{"ci.api_token": imageSecret, "org.opencontainers.image.source": "https://example.org/r"}
	cfg.Images[0].History = []dockertest.FakeLayer{
		{ID: imgEnv, CreatedBy: "/bin/sh -c #(nop)  ENV API_KEY=" + imageSecret, Created: 1788000000},
		{ID: "<missing>", CreatedBy: "RUN /bin/sh -c curl -H 'Authorization: " + imageSecret + "' https://x # buildkit", Size: 300, Created: 1787999000},
	}
	return cfg
}

func TestImageHistoryAViewerSeesTheKindOfEachInstructionAndNotItsText(t *testing.T) {
	f, e := dashEnv(t, imageSecretsEngine())
	get := f.asRole(t, e, "img-viewer", "viewer")
	rec := get("images/" + imgEnv + "/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), imageSecret) {
		t.Fatalf("a viewer's history carries the baked-in secret: %s", rec.Body)
	}
	var h struct {
		Masked bool `json:"masked"`
		Layers []struct {
			CreatedBy string `json:"createdBy"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	if !h.Masked || len(h.Layers) != 2 || h.Layers[0].CreatedBy != "ENV <hidden>" || h.Layers[1].CreatedBy != "RUN <hidden>" || h.Layers[1].Size != 300 {
		t.Errorf("history = %+v, want the kind of instruction and the size kept and masked set", h)
	}
}

func TestImageHistoryAHolderOfSecretsRevealSeesTheCommandsAsTheEngineGaveThem(t *testing.T) {
	f, e := dashEnv(t, imageSecretsEngine())
	get := f.asRole(t, e, "img-admin", "admin")
	rec := get("images/" + imgEnv + "/history")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ENV API_KEY="+imageSecret) || strings.Contains(rec.Body.String(), `"masked":true`) {
		t.Errorf("admin history: %d %s, want the commands unmasked", rec.Code, rec.Body)
	}
}

func TestImageInspectAViewerDoesNotGetTheCommandOrASecretNamedLabel(t *testing.T) {
	f, e := dashEnv(t, imageSecretsEngine())
	viewer := f.asRole(t, e, "img-viewer", "viewer")
	rec := viewer("images/" + imgEnv + "/inspect")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), imageSecret) {
		t.Fatalf("viewer inspect: %d, carries the secret: %s", rec.Code, rec.Body)
	}
	var got imageInspect
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cfg, _ := got.Inspect["Config"].(map[string]any)
	if cmd, _ := cfg["Cmd"].([]any); len(cmd) != 1 || cmd[0] != "<hidden>" {
		t.Errorf("Config.Cmd = %v, want the placeholder", cfg["Cmd"])
	}
	if ep, _ := cfg["Entrypoint"].([]any); len(ep) != 1 || ep[0] != "<hidden>" {
		t.Errorf("Config.Entrypoint = %v, want the placeholder", cfg["Entrypoint"])
	}

	// The detail route's labels follow the same rule, and say so.
	rec = viewer("images/" + imgEnv)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), imageSecret) {
		t.Fatalf("viewer detail: %d, carries the secret: %s", rec.Code, rec.Body)
	}
	var d struct {
		Masked bool              `json:"masked"`
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Masked || d.Labels["ci.api_token"] != "<hidden>" || d.Labels["org.opencontainers.image.source"] != "https://example.org/r" {
		t.Errorf("detail = %+v", d)
	}

	// A holder of secrets.reveal gets everything as the engine sent it.
	admin := f.asRole(t, e, "img-admin", "admin")
	rec = admin("images/" + imgEnv + "/inspect")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "--password="+imageSecret) {
		t.Errorf("admin inspect: %d %s, want the command unmasked", rec.Code, rec.Body)
	}
	rec = admin("images/" + imgEnv)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), imageSecret) || strings.Contains(rec.Body.String(), `"masked":true`) {
		t.Errorf("admin detail: %d %s, want the labels unmasked", rec.Code, rec.Body)
	}
}
