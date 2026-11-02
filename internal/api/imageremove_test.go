// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/proto/wire"
)

// Removing and untagging images (plan 05-16, RES-09, D-28, D-29, D-30, D-31). Docker is a
// fake daemon behind the real monitor, snapshot cache and aggregator; the router and policy
// are the real ones.

var (
	rmApp   = hexID("a")
	rmBase  = hexID("b")
	rmUsed  = hexID("c")
	rmMulti = hexID("d")
	// rmDang2 and rmDangUsed are untagged: the first is free, the second is used by a stopped
	// container on mgr-1 of the swarm fixture (plan 05-17).
	rmDang2    = hexID("9")
	rmDangUsed = hexID("8")
)

func removeEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Images: []dockertest.FakeImage{
			{ID: rmApp, RepoTags: []string{"ghcr.io/acme/app:1"}, Created: 40},
			{ID: rmBase, Created: 30},
			{ID: rmDang2, Created: 25},
			{ID: rmUsed, RepoTags: []string{"redis:8"}, Created: 20},
			{ID: rmMulti, RepoTags: []string{"nginx:1.27", "nginx:latest", "mirror/nginx:1.27"}, RepoDigests: []string{"nginx@sha256:" + strings.Repeat("e", 64)}, Created: 10},
		},
		Containers: []dockertest.FakeContainer{
			{ID: "c-old", Names: []string{"/old-cache"}, Image: "redis:8", ImageID: rmUsed, State: "exited"},
		},
	}
}

type imagesRemoved struct {
	BulkID  string `json:"bulkId"`
	Results []struct {
		NodeID  string `json:"nodeId"`
		ImageID string `json:"imageId"`
		Ref     string `json:"ref"`
		Result  string `json:"result"`
		Code    string `json:"code"`
		Detail  string `json:"detail"`
		Removed bool   `json:"removed"`
	} `json:"results"`
}

func removeImagesJSON(targets ...[2]string) string {
	var ts []string
	for _, t := range targets {
		ts = append(ts, fmt.Sprintf(`{"nodeId":%q,"imageId":%q}`, t[0], t[1]))
	}
	return fmt.Sprintf(`{"targets":[%s]}`, strings.Join(ts, ","))
}

// queryAudits reads the audit rows of the given actions, oldest first.
func queryAudits(t *testing.T, db *sql.DB, actions ...string) []auditRowFull {
	t.Helper()
	rows, err := db.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, COALESCE(reason, ''), COALESCE(details, '{}')
		FROM audit_log ORDER BY seq`)
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
		if slices.Contains(actions, r.Action) {
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// imageDeleter makes a user with a role on the environment and returns the way to post to
// an image route (remove or untag) as them. It waits for the monitor's first probe.
func (f *envFix) imageDeleter(t *testing.T, e envBody, user, role string) func(route, body string) *httptest.ResponseRecorder {
	t.Helper()
	f.addUser(t, user, false)
	f.addEnvRole(t, user, e.ID, role)
	awaitOnline(t, f.mon, e.ID)
	cookie, csrf := f.signIn(t, user, false)
	return func(route, body string) *httptest.ResponseRecorder {
		return f.call(t, authReq{method: http.MethodPost, path: "/api/env/" + e.ID + "/images/" + route, body: body, cookie: cookie, csrf: csrf})
	}
}

func (f *envFix) imageAudits(t *testing.T) []auditRowFull {
	t.Helper()
	return queryAudits(t, f.st.Read, audit.ActionImageRemoved, audit.ActionImageUntagged)
}

// imageWrites are the writes a fake served that are image deletes.
func imageWrites(f *dockertest.Fake) []string {
	var out []string
	for _, w := range f.Writes() {
		if strings.HasPrefix(w, "DELETE /images/") {
			out = append(out, w)
		}
	}
	return out
}

func TestRemoveImageAnOperatorRemovesAnUnusedImageWithAllItsNamesAndItIsAudited(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmMulti}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "done" || r.NodeID != e.ID || r.ImageID != rmMulti || got.BulkID != "" {
		t.Fatalf("result = %+v bulk %q, want done and no bulk for one target", r, got.BulkID)
	}
	w := imageWrites(f.fake)
	if len(w) < 4 || !strings.HasPrefix(w[0], "DELETE /images/nginx:1.27") || slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "force") }) {
		t.Fatalf("writes = %v, want an untag of each name and never a force", w)
	}
	rows := f.imageAudits(t)
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Action != audit.ActionImageRemoved || r.Result != string(audit.ResultSuccess) || r.TargetType != "image" || r.TargetID != rmMulti || r.EnvID != e.ID {
		t.Errorf("audit row = %+v", r)
	}
	for k, want := range map[string]string{"ref": "nginx:1.27", "node": e.ID, "code": "removed"} {
		if r.Details[k] != want {
			t.Errorf("detail %s = %q, want %q (%+v)", k, r.Details[k], want, r.Details)
		}
	}
	if _, ok := r.Details["bulk"]; ok {
		t.Errorf("a single target carries a bulk: %+v", r.Details)
	}
	list := decode[imageList](t, f.get(t, e, "images"))
	for _, it := range list.Items {
		if it.ID == rmMulti {
			t.Fatalf("the image is still listed: %+v", list.Items)
		}
	}
}

func TestRemoveImageAViewerIs403AndNothingIsSent(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "looker", "viewer")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmApp}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("a refused caller reached the engine: %v", w)
	}
	if rows := f.imageAudits(t); len(rows) != 0 {
		t.Fatalf("image audit rows = %+v, want none", rows)
	}
	denied := queryAudits(t, f.st.Read, audit.ActionPermissionDenied)
	if len(denied) == 0 || denied[len(denied)-1].TargetID != "POST /api/env/{envId}/images/remove" {
		t.Fatalf("the refusal was not audited as permission.denied: %+v", denied)
	}
}

func TestRemoveImageASecondRemoveOfTheSameImageAnswersNotFoundAndIsNotAudited(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	body := removeImagesJSON([2]string{e.ID, rmApp})
	if rec := post("remove", body); rec.Code != http.StatusOK {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := post("remove", body)
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("second: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "not_found" {
		t.Fatalf("second result = %+v, want failed not_found", r)
	}
	if rows := f.imageAudits(t); len(rows) != 1 {
		t.Fatalf("audit rows = %+v, want only the first remove's", rows)
	}
}

func TestRemoveImageAReadOnlyHostIsHostReadOnlyAuditedAndNothingIsSent(t *testing.T) {
	t.Parallel()
	f := newAgentFix(t)
	env := f.srv.NewEnvironment(t, "Locked")
	a := f.connect(t, env, removeEngine(), &wire.Facts{
		EngineID: "ENG:ro", Hostname: "node-1", EngineReachable: true, Policy: wire.PolicyFacts{ReadOnly: true},
	})
	f.whenState(t, env.ID, "online")
	f.whenProbed(t, envs.AgentTarget(a.id))

	rec := f.req(t, http.MethodPost, "/api/env/"+env.ID+"/images/remove", removeImagesJSON([2]string{a.id, rmApp}))
	if rec.code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.code, rec.body)
	}
	got := decode[imagesRemoved](t, rec)
	if len(got.Results) != 1 || got.Results[0].Result != "failed" || got.Results[0].Code != "host_read_only" {
		t.Fatalf("results = %+v, want failed with host_read_only", got.Results)
	}
	if w := a.fake.Writes(); len(w) != 0 {
		t.Fatalf("a read-only host received %v", w)
	}
	rows := queryAudits(t, f.st.Read, audit.ActionImageRemoved)
	if len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Details["code"] != "host_read_only" {
		t.Fatalf("audit rows = %+v, want one failure with host_read_only", rows)
	}
}

func TestRemoveImageTheSchemaRefusesAnEmptyListAndAnIDOfTheWrongShape(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	for name, body := range map[string]string{
		"no targets": `{"targets":[]}`,
		"a bad id":   removeImagesJSON([2]string{e.ID, "nginx:latest"}),
	} {
		if rec := post("remove", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want 422: %s", name, rec.Code, rec.Body)
		}
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

// In-use blocking, the fresh re-check and untag (plan 05-16 task 2, D-28, D-29, T-05-54).

var (
	rmSvc  = hexID("e")
	rmFree = hexID("f")
)

// removeSwarm is two managers and a worker that all hold redis:8 (rmUsed), nginx:1.27 (rmSvc,
// which a service spec with no task names), a free image and a two-name image. A stopped
// container uses redis:8 on mgr-1 only.
func removeSwarm(t *testing.T) *swarmFix {
	t.Helper()
	s, _ := removeSwarmCfgs(t)
	return s
}

// removeSwarmCfgs is removeSwarm and the configuration each node's fake was started with, by
// hostname, so a test can change what one answers.
func removeSwarmCfgs(t *testing.T) (*swarmFix, map[string]dockertest.FakeConfig) {
	t.Helper()
	cfgs := map[string]dockertest.FakeConfig{}
	s := newSwarmFixWith(t, func(host string, cfg *dockertest.FakeConfig) {
		cfg.Images = []dockertest.FakeImage{
			{ID: rmUsed, RepoTags: []string{"redis:8"}, Created: 50},
			{ID: rmSvc, RepoTags: []string{"nginx:1.27"}, Created: 40},
			{ID: rmFree, RepoTags: []string{"busybox:1"}, Created: 30},
			{ID: rmMulti, RepoTags: []string{"app:1", "app:2"}, Created: 20},
			{ID: rmDangUsed, Created: 10},
		}
		if host == "mgr-1" {
			cfg.Containers = append(cfg.Containers,
				dockertest.FakeContainer{ID: "c-old", Names: []string{"/old-cache"}, Image: "redis:8", ImageID: rmUsed, State: "exited"},
				dockertest.FakeContainer{ID: "c-dang", Names: []string{"/old-build"}, Image: rmDangUsed, ImageID: rmDangUsed, State: "exited"})
		}
		if strings.HasPrefix(host, "mgr-") {
			cfg.ServiceRows = append(cfg.ServiceRows,
				dockertest.FakeService{ID: "svc-proxy", Name: "proxy", Mode: "replicated", Running: 0, Desired: 0, Image: "nginx:1.27"})
		}
		cfgs[host] = *cfg
	})
	return s, cfgs
}

func (s *swarmFix) postImages(t *testing.T, route, body string) (*envRec, imagesRemoved) {
	t.Helper()
	rec := s.req(t, http.MethodPost, "/api/env/"+s.env.ID+"/images/"+route, body)
	var got imagesRemoved
	if rec.code == http.StatusOK {
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("body = %s (%v)", rec.body, err)
		}
	}
	return rec, got
}

func (s *swarmFix) imageDeletesOn() map[string][]string {
	out := map[string][]string{}
	for name, a := range map[string]*agentConn{"m1": s.m1, "m2": s.m2, "w": s.w} {
		out[name] = imageWrites(a.fake)
	}
	return out
}

func (s *swarmFix) imageAudits(t *testing.T) []auditRowFull {
	t.Helper()
	return queryAudits(t, s.st.Read, audit.ActionImageRemoved, audit.ActionImageUntagged)
}

func TestRemoveImageAContainerBlocksOnlyItsOwnNodeAndTheOthersAreRemoved(t *testing.T) {
	t.Parallel()
	s := removeSwarm(t)
	rec, got := s.postImages(t, "remove", removeImagesJSON([2]string{s.m1.id, rmUsed}, [2]string{s.m2.id, rmUsed}, [2]string{s.w.id, rmUsed}))
	if rec.code != http.StatusOK || len(got.Results) != 3 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "image_in_use" || !strings.Contains(r.Detail, "old-cache") {
		t.Errorf("mgr-1 = %+v, want image_in_use naming old-cache", r)
	}
	if got.Results[1].Result != "done" || got.Results[2].Result != "done" {
		t.Errorf("the other nodes = %+v %+v, want done", got.Results[1], got.Results[2])
	}
	d := s.imageDeletesOn()
	if len(d["m1"]) != 0 || len(d["m2"]) == 0 || len(d["w"]) == 0 {
		t.Fatalf("deletes = %v, want none on mgr-1 and some on the others", d)
	}
	rows := s.imageAudits(t)
	if len(rows) != 3 || got.BulkID == "" {
		t.Fatalf("audit rows = %+v bulk %q, want three with a bulk", rows, got.BulkID)
	}
	for i, r := range rows {
		if r.Details["bulk"] != got.BulkID {
			t.Errorf("row %d bulk = %q, want %q", i, r.Details["bulk"], got.BulkID)
		}
	}
	if rows[0].Result != string(audit.ResultFailure) || rows[0].Details["code"] != "image_in_use" || rows[1].Result != string(audit.ResultSuccess) {
		t.Errorf("rows = %+v, want a failure then successes in request order", rows)
	}
}

func TestRemoveImageDanglingBulkSkipsANamedImageAndRemovesTheUntaggedOnesUnderOneBulkID(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmBase}, [2]string{e.ID, rmApp}, [2]string{e.ID, rmDang2}))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || len(got.Results) != 3 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[1]; r.Result != "skipped" || r.Code != "not_dangling" {
		t.Errorf("the tagged image = %+v, want skipped not_dangling", r)
	}
	if got.Results[0].Result != "done" || got.Results[2].Result != "done" {
		t.Errorf("the dangling ones = %+v %+v, want done", got.Results[0], got.Results[2])
	}
	if got.BulkID == "" {
		t.Fatal("no bulk ID answered")
	}
	w := imageWrites(f.fake)
	if len(w) == 0 || slices.ContainsFunc(w, func(x string) bool { return strings.Contains(x, rmApp) || strings.Contains(x, "ghcr.io") }) {
		t.Fatalf("writes = %v, want deletes of the untagged images only", w)
	}
	rows := f.imageAudits(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want one per image removed and none for the skipped one", rows)
	}
	for i, r := range rows {
		if r.Details["bulk"] != got.BulkID || r.Result != string(audit.ResultSuccess) {
			t.Errorf("row %d = %+v, want a success with bulk %q", i, r, got.BulkID)
		}
	}
}

func TestRemoveImageDanglingASingleTargetStillRemovesATaggedImage(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmApp}))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 || got.Results[0].Result != "done" {
		t.Fatalf("%d body = %s (%v), want done", rec.Code, rec.Body, err)
	}
}

func TestRemoveImageDanglingBulkNeverRemovesANamedImageWhateverTheBrowserSends(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmApp}, [2]string{e.ID, rmUsed}, [2]string{e.ID, rmMulti}))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 3 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	for _, r := range got.Results {
		if r.Result != "skipped" || r.Code != "not_dangling" {
			t.Errorf("result = %+v, want skipped not_dangling", r)
		}
	}
	if w := imageWrites(f.fake); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	if rows := f.imageAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none", rows)
	}
}

func TestRemoveImageDanglingBulkOnASwarmSkipsANamedImageAndRefusesAnUsedOneByNode(t *testing.T) {
	t.Parallel()
	s := removeSwarm(t)
	rec, got := s.postImages(t, "remove", removeImagesJSON([2]string{s.m1.id, rmDangUsed}, [2]string{s.m2.id, rmDangUsed}, [2]string{s.m1.id, rmMulti}))
	if rec.code != http.StatusOK || len(got.Results) != 3 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "image_in_use" || !strings.Contains(r.Detail, "old-build") {
		t.Errorf("mgr-1 = %+v, want image_in_use naming old-build", r)
	}
	if r := got.Results[1]; r.Result != "done" {
		t.Errorf("mgr-2 = %+v, want done", r)
	}
	if r := got.Results[2]; r.Result != "skipped" || r.Code != "not_dangling" {
		t.Errorf("the named image = %+v, want skipped not_dangling", r)
	}
	if d := s.imageDeletesOn(); len(d["m1"]) != 0 || len(d["m2"]) == 0 {
		t.Fatalf("deletes = %v, want none on mgr-1 (used, and the named one skipped) and some on mgr-2", d)
	}
	rows := s.imageAudits(t)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %+v, want the refusal and the removal and none for the skipped one", rows)
	}
	for i, r := range rows {
		if r.Details["bulk"] != got.BulkID {
			t.Errorf("row %d bulk = %q, want %q", i, r.Details["bulk"], got.BulkID)
		}
	}
}

func TestRemoveImageDanglingASingleImageSentWithABulkIDIsStillADanglingOnlyRequest(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	bulk := "0198f8d0-4c2a-7000-8000-000000000001"
	body := fmt.Sprintf(`{"bulkId":%q,"targets":[{"nodeId":%q,"imageId":%q}]}`, bulk, e.ID, rmApp)
	rec := post("remove", body)
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "skipped" || r.Code != "not_dangling" {
		t.Fatalf("result = %+v, want skipped not_dangling", r)
	}
	if w := imageWrites(f.fake); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
}

func TestRemoveImageAServiceSpecWithNoTaskBlocksEveryNodeAndNothingIsSent(t *testing.T) {
	t.Parallel()
	s := removeSwarm(t)
	rec, got := s.postImages(t, "remove", removeImagesJSON([2]string{s.m1.id, rmSvc}, [2]string{s.m2.id, rmSvc}, [2]string{s.w.id, rmSvc}))
	if rec.code != http.StatusOK || len(got.Results) != 3 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	for _, r := range got.Results {
		if r.Result != "failed" || r.Code != "image_in_use" || !strings.Contains(r.Detail, "proxy") {
			t.Errorf("result = %+v, want image_in_use naming the proxy service", r)
		}
	}
	if d := s.imageDeletesOn(); len(d["m1"])+len(d["m2"])+len(d["w"]) != 0 {
		t.Fatalf("a delete reached an engine: %v", d)
	}
}

func TestRemoveImageAContainerThatStartsBetweenTheCheckAndTheCallMakesTheEngineRefuseAsImageInUse(t *testing.T) {
	t.Parallel()
	cfg := removeEngine()
	cfg.BusyImages = []string{rmApp}
	f, e := dashEnv(t, cfg)
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("remove", removeImagesJSON([2]string{e.ID, rmApp}))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "image_in_use" {
		t.Fatalf("result = %+v, want failed image_in_use", r)
	}
	if w := imageWrites(f.fake); len(w) != 1 || strings.Contains(w[0], "force") {
		t.Fatalf("writes = %v, want the one delete the engine refused, never forced", w)
	}
	if rows := f.imageAudits(t); len(rows) != 1 || rows[0].Details["code"] != "image_in_use" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

func TestRemoveImageWhenTheServicesCannotBeReadNothingIsRemovedAndTheTargetIsNodeUnreachable(t *testing.T) {
	t.Parallel()
	s, cfgs := removeSwarmCfgs(t)
	for host, a := range map[string]*agentConn{"mgr-1": s.m1, "mgr-2": s.m2} {
		cfg := cfgs[host]
		cfg.QuorumLost = true
		a.fake.Reconfigure(cfg)
	}
	rec, got := s.postImages(t, "remove", removeImagesJSON([2]string{s.w.id, rmFree}))
	if rec.code != http.StatusOK || len(got.Results) != 1 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "node_unreachable" {
		t.Fatalf("result = %+v, want failed node_unreachable", r)
	}
	if d := s.imageDeletesOn(); len(d["m1"])+len(d["m2"])+len(d["w"]) != 0 {
		t.Fatalf("a delete reached an engine: %v", d)
	}
}

func untagJSON(imageID, ref string, nodeIDs ...string) string {
	b, _ := json.Marshal(map[string]any{"imageId": imageID, "ref": ref, "nodeIds": nodeIDs})
	return string(b)
}

func TestUntagOneOfSeveralNamesIsAllowedWhileTheImageIsInUse(t *testing.T) {
	t.Parallel()
	cfg := removeEngine()
	cfg.Containers = append(cfg.Containers,
		dockertest.FakeContainer{ID: "c-web", Names: []string{"/web"}, Image: "nginx:latest", ImageID: rmMulti, State: "running"})
	f, e := dashEnv(t, cfg)
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("untag", untagJSON(rmMulti, "nginx:latest", e.ID))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "done" || r.Removed || r.Ref != "nginx:latest" {
		t.Fatalf("result = %+v, want done, not removed", r)
	}
	if w := imageWrites(f.fake); len(w) != 1 || !strings.HasPrefix(w[0], "DELETE /images/nginx:latest") {
		t.Fatalf("writes = %v, want one untag of the name", w)
	}
	rows := f.imageAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionImageUntagged || rows[0].Details["ref"] != "nginx:latest" || rows[0].Details["node"] != e.ID || rows[0].Details["code"] != "untagged" {
		t.Fatalf("audit rows = %+v, want one image.untagged", rows)
	}
}

func TestUntagTheLastNameOfAnImageInUseIsLastNameInUseAndNothingIsSent(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("untag", untagJSON(rmUsed, "redis:8", e.ID))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "last_name_in_use" {
		t.Fatalf("result = %+v, want failed last_name_in_use", r)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	if rows := f.imageAudits(t); len(rows) != 1 || rows[0].Result != string(audit.ResultFailure) || rows[0].Details["code"] != "last_name_in_use" {
		t.Fatalf("audit rows = %+v, want one failure", rows)
	}
}

func TestUntagTheOnlyNameOfAnUnusedImageRemovesItAndIsAuditedAsARemove(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("untag", untagJSON(rmApp, "ghcr.io/acme/app:1", e.ID))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "done" || !r.Removed {
		t.Fatalf("result = %+v, want done and removed", r)
	}
	rows := f.imageAudits(t)
	if len(rows) != 1 || rows[0].Action != audit.ActionImageRemoved || rows[0].Details["ref"] != "ghcr.io/acme/app:1" {
		t.Fatalf("audit rows = %+v, want one image.removed", rows)
	}
	if list := decode[imageList](t, f.get(t, e, "images")); slices.ContainsFunc(list.Items, func(i listedImage) bool { return i.ID == rmApp }) {
		t.Fatalf("the image is still listed: %+v", list.Items)
	}
}

func TestUntagANameTheNodeDoesNotHaveIsNotFoundAndIsNotAudited(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	post := f.imageDeleter(t, e, "ops", "operator")
	rec := post("untag", untagJSON(rmMulti, "nginx:9", e.ID))
	var got imagesRemoved
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Results) != 1 {
		t.Fatalf("%d body = %s (%v)", rec.Code, rec.Body, err)
	}
	if r := got.Results[0]; r.Result != "failed" || r.Code != "not_found" {
		t.Fatalf("result = %+v, want failed not_found", r)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
	if rows := f.imageAudits(t); len(rows) != 0 {
		t.Fatalf("audit rows = %+v, want none", rows)
	}
}

func TestUntagWithNoNodesGoesToEveryNodeThatHasTheNameWithOneBulkID(t *testing.T) {
	t.Parallel()
	s := removeSwarm(t)
	rec, got := s.postImages(t, "untag", untagJSON(rmMulti, "app:2"))
	if rec.code != http.StatusOK || len(got.Results) != 3 {
		t.Fatalf("%d %s", rec.code, rec.body)
	}
	for _, r := range got.Results {
		if r.Result != "done" || r.Removed {
			t.Errorf("result = %+v, want done and kept", r)
		}
	}
	d := s.imageDeletesOn()
	if len(d["m1"]) != 1 || len(d["m2"]) != 1 || len(d["w"]) != 1 {
		t.Fatalf("deletes = %v, want one untag per node", d)
	}
	rows := s.imageAudits(t)
	if len(rows) != 3 || got.BulkID == "" {
		t.Fatalf("audit rows = %+v bulk %q", rows, got.BulkID)
	}
	for _, r := range rows {
		if r.Action != audit.ActionImageUntagged || r.Details["bulk"] != got.BulkID {
			t.Errorf("row = %+v, want image.untagged with the shared bulk", r)
		}
	}
}

func TestUntagAViewerIs403AndAnImageNoNodeHasWithNoNodesNamedIs404(t *testing.T) {
	t.Parallel()
	f, e := dashEnv(t, removeEngine())
	viewer := f.imageDeleter(t, e, "looker", "viewer")
	if rec := viewer("untag", untagJSON(rmMulti, "app:1", e.ID)); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d, want 403: %s", rec.Code, rec.Body)
	}
	ops := f.imageDeleter(t, e, "ops", "operator")
	if rec := ops("untag", untagJSON(rmMissing, "app:1")); rec.Code != http.StatusNotFound {
		t.Fatalf("no holder: %d, want 404: %s", rec.Code, rec.Body)
	}
	if w := f.fake.Writes(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

var rmMissing = hexID("9")
