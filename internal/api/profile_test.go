// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ianglennon/tierplan/internal/config"
)

// The signed-in user's own profile (plan 03-15, AUTH-04, D-24). Negative cases
// are mandatory (D-81).

type profileResp struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	UpdatedAt   string `json:"updatedAt"`
}

func (f *authFixture) getProfile(t *testing.T, cookie string) profileResp {
	t.Helper()
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me/profile", cookie: cookie})
	if rec.Code != http.StatusOK {
		t.Fatalf("get profile: %d %s", rec.Code, rec.Body)
	}
	var p profileResp
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *authFixture) saveProfile(t *testing.T, cookie, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: http.MethodPatch, path: "/api/me/profile", body: body, cookie: cookie, csrf: csrf})
}

func TestMyProfileIsReadAndSavedByTheUserThemselvesAndAuditedWithThemAsActor(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	f.addUser(t, "alice", false) // nothing to do with the user being edited
	cookie, csrf := f.signIn(t, "alice", false)

	p := f.getProfile(t, cookie)
	if p.Username != "alice" || p.DisplayName != "" || p.Email != "" || p.UpdatedAt == "" {
		t.Fatalf("profile = %+v", p)
	}
	rec := f.saveProfile(t, cookie, csrf, `{"displayName":"Alice L","email":"alice@example.test","updatedAt":"`+p.UpdatedAt+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var saved profileResp
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Username != "alice" || saved.DisplayName != "Alice L" || saved.Email != "alice@example.test" || saved.UpdatedAt == p.UpdatedAt {
		t.Fatalf("saved = %+v, want the new values and a new version", saved)
	}
	if got := f.getProfile(t, cookie); got.DisplayName != "Alice L" {
		t.Fatalf("profile after save = %+v", got)
	}
	entries := f.audits(t, "user.updated")
	if len(entries) != 1 || entries[0].ActorID != "id-alice" || entries[0].TargetID != "id-alice" || entries[0].Result != "success" {
		t.Fatalf("audit = %+v", entries)
	}
	// The admin's row was not touched.
	var name string
	if err := f.st.Read.QueryRow(`SELECT display_name FROM users WHERE id = 'id-admin'`).Scan(&name); err != nil || name != "" {
		t.Fatalf("admin display name = %q (%v), want it untouched", name, err)
	}
}

func TestMyProfileSaveWithAnOldVersionIs409StaleChangeAndSavesNothing(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	p := f.getProfile(t, cookie)
	if rec := f.saveProfile(t, cookie, csrf, `{"displayName":"First","updatedAt":"`+p.UpdatedAt+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", rec.Code, rec.Body)
	}
	rec := f.saveProfile(t, cookie, csrf, `{"displayName":"Second","updatedAt":"`+p.UpdatedAt+`"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stale_change" {
		t.Fatalf("stale save: %d %s, want 409 stale_change", rec.Code, rec.Body)
	}
	if got := f.getProfile(t, cookie); got.DisplayName != "First" {
		t.Fatalf("display name = %q, want First", got.DisplayName)
	}
}

func TestMyProfileSaveRefusesABadEmailAndAnOverlongNameWithFieldCodes(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	p := f.getProfile(t, cookie)
	rec := f.saveProfile(t, cookie, csrf, `{"displayName":"`+strings.Repeat("n", 65)+`","email":"not an email","updatedAt":"`+p.UpdatedAt+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	for _, code := range []string{"invalid_email", "display_name_too_long"} {
		if !strings.Contains(rec.Body.String(), code) {
			t.Errorf("body lacks %s: %s", code, rec.Body)
		}
	}
	if got := f.getProfile(t, cookie); got.UpdatedAt != p.UpdatedAt {
		t.Fatal("a refused save moved the version")
	}
}

func TestMyProfileHasNoUsernameFieldAndABodyThatNamesOneIsRefused(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, csrf := f.signIn(t, "admin", false)
	p := f.getProfile(t, cookie)
	rec := f.saveProfile(t, cookie, csrf, `{"username":"root","displayName":"X","updatedAt":"`+p.UpdatedAt+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a body naming the username: %d %s, want 422", rec.Code, rec.Body)
	}
	if got := f.getProfile(t, cookie); got.Username != "admin" || got.DisplayName != "" {
		t.Fatalf("profile = %+v, want it unchanged", got)
	}

	spec, err := os.ReadFile("../../openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		t.Fatal(err)
	}
	op, ok := doc.Paths["/api/me/profile"]["patch"]
	if !ok {
		t.Fatal("openapi.json has no PATCH /api/me/profile")
	}
	ref := op.RequestBody.Content["application/json"].Schema.Ref
	schema, ok := doc.Components.Schemas[ref[strings.LastIndex(ref, "/")+1:]]
	if !ok {
		t.Fatalf("no schema for %q", ref)
	}
	if _, has := schema.Properties["username"]; has {
		t.Fatal("the profile body schema carries a username field")
	}
}

func TestMyProfileNeedsASessionAndACSRFTokenToSave(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	cookie, _ := f.signIn(t, "admin", false)
	p := f.getProfile(t, cookie)
	body := `{"displayName":"X","updatedAt":"` + p.UpdatedAt + `"}`
	if rec := f.call(t, authReq{method: http.MethodGet, path: "/api/me/profile"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d, want 401", rec.Code)
	}
	if rec := f.call(t, authReq{method: http.MethodPatch, path: "/api/me/profile", body: body}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous save: %d, want 401", rec.Code)
	}
	if rec := f.saveProfile(t, cookie, "", body); rec.Code != http.StatusForbidden {
		t.Fatalf("save without a CSRF token: %d, want 403", rec.Code)
	}
}
