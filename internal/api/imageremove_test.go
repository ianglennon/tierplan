// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Removing and untagging images (plan 05-16, RES-09, D-28, D-29, D-30, D-31). Docker is a
// fake daemon behind the real monitor, snapshot cache and aggregator; the router and policy
// are the real ones.

var (
	rmApp   = hexID("a")
	rmBase  = hexID("b")
	rmUsed  = hexID("c")
	rmMulti = hexID("d")
)

func removeEngine() dockertest.FakeConfig {
	return dockertest.FakeConfig{
		Hostname: "node-1", Version: "29.1.0",
		Images: []dockertest.FakeImage{
			{ID: rmApp, RepoTags: []string{"ghcr.io/acme/app:1"}, Created: 40},
			{ID: rmBase, Created: 30},
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
	waitFor(t, "the monitor's first probe", func() bool { return f.mon.Status(e.ID).State == envs.StateOnline })
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
