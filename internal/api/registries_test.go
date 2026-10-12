// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Registry credentials (plan 05-05, IMG-01, D-01, D-05, D-09, D-30, D-31). The router, policy,
// CSRF guard, vault and audit writer are the real ones.

// credToken is a secret of 12 or more characters, so its last 4 are kept.
const credToken = "ghp_SECRETtoken0123456789abcdefWXYZ" //nolint:gosec // G101: a made-up token for the test

const ghcrAcme = `{"name":"ghcr-acme","type":"ghcr","prefix":"acme","username":"bot","secret":"` + credToken + `"}`

// credentialKeys is the exact key set of CredentialBody: a new key is a new thing the browser can
// learn, and it must be added here on purpose.
var credentialKeys = []string{
	"canManage", "createdAt", "expiresAt", "hubFallback", "host", "id", "last4", "lastFailure", "lastTest",
	"name", "prefix", "scope", "type", "updatedAt", "username",
}

type credentialResp struct {
	ID    string `json:"id"`
	Scope struct {
		Kind    string  `json:"kind"`
		EnvID   *string `json:"envId"`
		EnvName *string `json:"envName"`
	} `json:"scope"`
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Host        string  `json:"host"`
	Prefix      string  `json:"prefix"`
	Username    string  `json:"username"`
	Last4       string  `json:"last4"`
	HubFallback bool    `json:"hubFallback"`
	ExpiresAt   *string `json:"expiresAt"`
	CanManage   bool    `json:"canManage"`
}

type credentialUser struct {
	cookie, csrf string
}

func (f *authFixture) addEnvironmentRow(t *testing.T, id string) {
	t.Helper()
	if _, err := f.st.Write.Exec(`INSERT INTO environments (id, kind, name, socket_path, enabled, created_at, updated_at)
		VALUES (?, 'agent', ?, '', 1, 1, 1)`, id, "env "+id); err != nil {
		t.Fatal(err)
	}
}

// asRoleOn adds a user with a role on one environment and signs them in.
func (f *authFixture) asRoleOn(t *testing.T, name, envID, role string) credentialUser {
	t.Helper()
	f.addUser(t, name, false)
	f.addEnvRole(t, name, envID, role)
	cookie, csrf := f.signIn(t, name, false)
	return credentialUser{cookie, csrf}
}

func (f *authFixture) asAdmin(t *testing.T) credentialUser {
	t.Helper()
	cookie, csrf := f.signIn(t, "admin", false)
	return credentialUser{cookie, csrf}
}

func (f *authFixture) credCall(t *testing.T, u credentialUser, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.call(t, authReq{method: method, path: path, body: body, cookie: u.cookie, csrf: u.csrf})
}

func decodeCred(t *testing.T, rec *httptest.ResponseRecorder) credentialResp {
	t.Helper()
	var c credentialResp
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return c
}

func decodeCreds(t *testing.T, rec *httptest.ResponseRecorder) []credentialResp {
	t.Helper()
	var list struct {
		Items []credentialResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return list.Items
}

func TestAnInstanceAdminStoresAGHCRCredentialAndOnlyItsLastCharactersComeBack(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries", ghcrAcme)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c := decodeCred(t, rec)
	if c.ID == "" || c.Name != "ghcr-acme" || c.Type != "ghcr" || c.Host != "ghcr.io" || c.Prefix != "acme" || c.Username != "bot" ||
		c.Last4 != "WXYZ" || c.Scope.Kind != "instance" || c.Scope.EnvID != nil || c.Scope.EnvName != nil || !c.CanManage || c.HubFallback || c.ExpiresAt != nil {
		t.Errorf("credential = %+v", c)
	}

	// Every body the API gives back for the credential is free of the secret and has the key set.
	bodies := map[string]*httptest.ResponseRecorder{
		"create": rec,
		"list":   f.credCall(t, admin, http.MethodGet, "/api/registries", ""),
		"get":    f.credCall(t, admin, http.MethodGet, "/api/registries/"+c.ID, ""),
	}
	for name, r := range bodies {
		if r.Code != http.StatusOK && r.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", name, r.Code, r.Body)
		}
		if strings.Contains(r.Body.String(), credToken) || strings.Contains(r.Body.String(), "SECRETtoken") {
			t.Errorf("%s response carries the secret: %s", name, r.Body)
		}
	}
	if got := decodeCreds(t, bodies["list"]); len(got) != 1 || got[0].ID != c.ID {
		t.Errorf("list = %+v", got)
	}
	var one map[string]json.RawMessage
	if err := json.Unmarshal(bodies["get"].Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(one))
	for k := range one {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := slices.Clone(credentialKeys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Errorf("credential keys = %v, want exactly %v", keys, want)
	}
}

func TestTheSecretIsSealedAtRestUnderTheCredentialsOwnContext(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	c := decodeCred(t, f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries", ghcrAcme))

	var sealed, last4 string
	if err := f.st.Read.QueryRow(`SELECT secret_sealed, secret_last4 FROM registry_credentials WHERE id = ?`, c.ID).Scan(&sealed, &last4); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, credToken) || last4 != "WXYZ" {
		t.Fatalf("stored secret_sealed = %q, secret_last4 = %q", sealed, last4)
	}
	plain, err := f.v.Open(vault.ContextRegistryCred+c.ID, sealed)
	if err != nil || string(plain) != credToken {
		t.Fatalf("Open under its own ID = %q, %v", plain, err)
	}
	if _, err := f.v.Open(vault.ContextRegistryCred+"another-id", sealed); err == nil {
		t.Error("the sealed secret opens under another credential's ID")
	}
}

func TestAShortSecretLeavesNoLastCharactersToShow(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries",
		`{"name":"harbor","type":"generic","host":"Harbor.Example.org:5000","username":"ci","secret":"hunter2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if c := decodeCred(t, rec); c.Last4 != "" || c.Host != "harbor.example.org:5000" {
		t.Errorf("credential = %+v, want no last4 and a lower-cased host", c)
	}
	if strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("response carries the secret: %s", rec.Body)
	}
}

func TestOnlyAnInstanceAdminMayAddAnInstanceCredential(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	for name, u := range map[string]credentialUser{
		"env admin":         f.asRoleOn(t, "envadmin", "env-a", "admin"),
		"admin of all":      f.asRoleOn(t, "alladmin", "", "admin"),
		"operator":          f.asRoleOn(t, "operator", "env-a", "operator"),
		"viewer":            f.asRoleOn(t, "viewer", "env-a", "viewer"),
		"user with no role": {nobodyCookie, nobodyCSRF},
	} {
		rec := f.credCall(t, u, http.MethodPost, "/api/registries", ghcrAcme)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; a refused create stores nothing", n, err)
	}
}

func TestEveryoneWhoReadsImagesSeesTheInstanceCredentialsReadOnly(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	c := decodeCred(t, f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries", ghcrAcme))

	for name, u := range map[string]credentialUser{
		"viewer of one environment": f.asRoleOn(t, "viewer", "env-a", "viewer"),
		"env admin":                 f.asRoleOn(t, "envadmin", "env-a", "admin"),
		"viewer of everything":      f.asRoleOn(t, "viewall", "", "viewer"),
	} {
		rec := f.credCall(t, u, http.MethodGet, "/api/registries", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s list: %d %s", name, rec.Code, rec.Body)
		}
		got := decodeCreds(t, rec)
		if len(got) != 1 || got[0].ID != c.ID || got[0].CanManage || got[0].Last4 != "WXYZ" {
			t.Errorf("%s list = %+v, want the one credential with canManage false", name, got)
		}
		if strings.Contains(rec.Body.String(), "SECRETtoken") {
			t.Errorf("%s list carries the secret: %s", name, rec.Body)
		}
		one := f.credCall(t, u, http.MethodGet, "/api/registries/"+c.ID, "")
		if one.Code != http.StatusOK || decodeCred(t, one).CanManage {
			t.Errorf("%s get: %d %s", name, one.Code, one.Body)
		}
	}
}

func TestAUserWhoReadsNoImagesSeesNoCredentialsAndTheyAreNotFoundToThem(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	c := decodeCred(t, f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries", ghcrAcme))
	f.addUser(t, "nobody", false)
	cookie, csrf := f.signIn(t, "nobody", false)
	u := credentialUser{cookie, csrf}

	rec := f.credCall(t, u, http.MethodGet, "/api/registries", "")
	if rec.Code != http.StatusOK || len(decodeCreds(t, rec)) != 0 || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("list: %d %s, want an empty list", rec.Code, rec.Body)
	}
	hidden := f.credCall(t, u, http.MethodGet, "/api/registries/"+c.ID, "")
	missing := f.credCall(t, u, http.MethodGet, "/api/registries/no-such-credential", "")
	if hidden.Code != http.StatusNotFound || problemType(t, hidden) != "credential_not_found" ||
		missing.Code != hidden.Code || problemType(t, missing) != problemType(t, hidden) {
		t.Errorf("hidden: %d %s; missing: %d %s; they must be the same 404 credential_not_found", hidden.Code, hidden.Body, missing.Code, missing.Body)
	}
}

func TestAnUnknownCredentialIsNotFoundToAnInstanceAdmin(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.credCall(t, f.asAdmin(t), http.MethodGet, "/api/registries/no-such-credential", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("%d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
}

func TestACredentialIsRefusedWithACodePerFieldAtFault(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	for _, c := range []struct {
		name, body, location, code string
	}{
		{"docker hub with neither a namespace nor the fallback", `{"name":"hub","type":"dockerhub","username":"u","secret":"s"}`, "body.prefix", "hub_prefix_required"},
		{"the fallback on another registry", `{"name":"g","type":"ghcr","hubFallback":true,"username":"u","secret":"s"}`, "body.hubFallback", "hub_fallback_not_hub"},
		{"a host with a scheme", `{"name":"g","type":"generic","host":"https://x.example","username":"u","secret":"s"}`, "body.host", "host_invalid"},
		{"an upper-case prefix", `{"name":"g","type":"ghcr","prefix":"Acme","username":"u","secret":"s"}`, "body.prefix", "prefix_invalid"},
		{"an expiry in the past", `{"name":"g","type":"ghcr","username":"u","secret":"s","expires":"2025-01-01"}`, "body.expires", "expiry_past"},
		{"no secret", `{"name":"g","type":"ghcr","username":"u"}`, "body.secret", "secret_required"},
		{"markup in the name", `{"name":"<script>","type":"ghcr","username":"u","secret":"s"}`, "body.name", "name_invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := f.credCall(t, admin, http.MethodPost, "/api/registries", c.body)
			p := decodeProblem(t, rec)
			if rec.Code != http.StatusUnprocessableEntity || p.Type != "validation_failed" || len(p.Errors) != 1 ||
				p.Errors[0].Location != c.location || p.Errors[0].Message != c.code {
				t.Errorf("%d %s, want 422 validation_failed %s %s", rec.Code, rec.Body, c.location, c.code)
			}
		})
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; a refused credential stores nothing", n, err)
	}
}

func TestADockerHubCredentialNeedsANamespaceOrTheFallbackOrBoth(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	for i, body := range []string{
		`{"name":"hub-ns","type":"dockerhub","prefix":"acme","username":"u","secret":"s3cret-token-value"}`,
		`{"name":"hub-all","type":"dockerhub","hubFallback":true,"username":"u","secret":"s3cret-token-value"}`,
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("case %d: %d %s", i, rec.Code, rec.Body)
		}
		if c := decodeCred(t, rec); c.Host != "docker.io" || c.HubFallback != (i == 1) {
			t.Errorf("case %d: %+v", i, c)
		}
	}
}

func TestACredentialClashingWithAnotherOfTheLayerIsAConflict(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries", ghcrAcme); rec.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries",
		`{"name":"hub-all","type":"dockerhub","hubFallback":true,"username":"u","secret":"s3cret-token-value"}`); rec.Code != http.StatusCreated {
		t.Fatalf("hub: %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ name, body, ptype string }{
		{"same name", `{"name":"ghcr-acme","type":"ghcr","prefix":"other","username":"u","secret":"s"}`, "credential_name_taken"},
		{"same host and prefix", `{"name":"again","type":"ghcr","prefix":"acme","username":"u","secret":"s"}`, "host_prefix_taken"},
		{"a second Hub fallback", `{"name":"hub-two","type":"dockerhub","prefix":"acme","hubFallback":true,"username":"u","secret":"s"}`, "hub_fallback_taken"},
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries", c.body)
		if rec.Code != http.StatusConflict || problemType(t, rec) != c.ptype {
			t.Errorf("%s: %d %s, want 409 %s", c.name, rec.Code, rec.Body, c.ptype)
		}
	}
}

func TestACreateIsAuditedWithoutTheSecretOrItsLastCharacters(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	c := decodeCred(t, f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries",
		`{"name":"ghcr-acme","type":"ghcr","prefix":"acme","username":"bot","expires":"2199-12-31","secret":"`+credToken+`"}`))

	rows, err := f.st.Read.Query(`SELECT action, target_type, target_id, COALESCE(env_id, ''), result, actor_name, details FROM audit_log WHERE action LIKE 'registry.%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		n++
		var action, ttype, tid, env, result, actor, details string
		if err := rows.Scan(&action, &ttype, &tid, &env, &result, &actor, &details); err != nil {
			t.Fatal(err)
		}
		if action != "registry.credential_created" || ttype != "registry_credential" || tid != c.ID || env != "" || result != "success" || actor != "admin" {
			t.Errorf("audit row = %s %s %s env %q %s by %s", action, ttype, tid, env, result, actor)
		}
		var d map[string]string
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"name": "ghcr-acme", "layer": "instance", "host": "ghcr.io", "prefix": "acme", "type": "ghcr", "username": "bot", "hub_fallback": "false", "expires": "2199-12-31"}
		if len(d) != len(want) {
			t.Errorf("details = %v, want exactly %v", d, want)
		}
		for k, v := range want {
			if d[k] != v {
				t.Errorf("details[%s] = %q, want %q", k, d[k], v)
			}
		}
		for _, leak := range []string{credToken, "SECRETtoken", "WXYZ"} {
			if strings.Contains(details, leak) {
				t.Errorf("audit details %s carry %q", details, leak)
			}
		}
	}
	if err := rows.Err(); err != nil || n != 1 {
		t.Errorf("registry audit rows = %d, %v; want exactly one", n, err)
	}
}

// Editing a credential (plan 05-07, D-05, D-31).

const newCredToken = "ghp_BRANDnewTOKEN9876543210abcdefQRST" //nolint:gosec // G101: a made-up token for the test

func (f *authFixture) storeCredential(t *testing.T, u credentialUser, body string) credentialResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, "/api/registries", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return decodeCred(t, rec)
}

// registryAudit returns the details of every registry audit row, oldest first, with its action.
func (f *authFixture) registryAudit(t *testing.T) (actions []string, details []map[string]string) {
	t.Helper()
	rows, err := f.st.Read.Query(`SELECT action, details FROM audit_log WHERE action LIKE 'registry.%' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var action, raw string
		if err := rows.Scan(&action, &raw); err != nil {
			t.Fatal(err)
		}
		d := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatal(err)
		}
		actions, details = append(actions, action), append(details, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return actions, details
}

func TestAnAdminEditsAUsernameAndTheSecretStaysWhereItWas(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)
	var before string
	if err := f.st.Read.QueryRow(`SELECT secret_sealed FROM registry_credentials WHERE id = ?`, c.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"username":"bot2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	got := decodeCred(t, rec)
	if got.Username != "bot2" || got.Name != "ghcr-acme" || got.Prefix != "acme" || got.Last4 != "WXYZ" || got.Scope.Kind != "instance" {
		t.Errorf("credential = %+v", got)
	}
	if strings.Contains(rec.Body.String(), "SECRETtoken") {
		t.Errorf("response carries the secret: %s", rec.Body)
	}
	var after string
	if err := f.st.Read.QueryRow(`SELECT secret_sealed FROM registry_credentials WHERE id = ?`, c.ID).Scan(&after); err != nil || after != before {
		t.Errorf("secret_sealed changed without a new secret: %q -> %q (%v)", before, after, err)
	}
}

func TestANewSecretReplacesTheStoredOneAndNothingComesBack(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)

	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"secret":"`+newCredToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got := decodeCred(t, rec); got.Last4 != "QRST" {
		t.Errorf("last4 = %q, want QRST", got.Last4)
	}
	if strings.Contains(rec.Body.String(), newCredToken) || strings.Contains(rec.Body.String(), "BRANDnew") {
		t.Errorf("response carries the new secret: %s", rec.Body)
	}
	var sealed string
	if err := f.st.Read.QueryRow(`SELECT secret_sealed FROM registry_credentials WHERE id = ?`, c.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	plain, err := f.v.Open(vault.ContextRegistryCred+c.ID, sealed)
	if err != nil || string(plain) != newCredToken {
		t.Errorf("stored secret = %q, %v, want the new token", plain, err)
	}
}

func TestAPatchCanClearTheNamespaceAndTheExpiry(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin,
		`{"name":"harbor","type":"generic","host":"harbor.example.org","prefix":"team","username":"ci","expires":"2199-12-31","secret":"hunter2hunter2"}`)

	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"prefix":"","expires":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got := decodeCred(t, rec); got.Prefix != "" || got.ExpiresAt != nil || got.Host != "harbor.example.org" {
		t.Errorf("credential = %+v, want no prefix and no expiry", got)
	}
}

func TestAnUpdateWritesAuditRowsNamingTheChangedFieldsNeverTheirValues(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)
	if rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"username":"bot2"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"username":"bot3","secret":"`+newCredToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}

	actions, details := f.registryAudit(t)
	if !slices.Equal(actions, []string{"registry.credential_created", "registry.credential_updated", "registry.credential_updated"}) {
		t.Fatalf("actions = %v", actions)
	}
	if details[1]["changed"] != "username" || details[2]["changed"] != "secret,username" {
		t.Errorf("changed = %q and %q, want username and secret,username", details[1]["changed"], details[2]["changed"])
	}
	if details[2]["username"] != "bot3" || details[2]["name"] != "ghcr-acme" || details[2]["layer"] != "instance" {
		t.Errorf("details = %v", details[2])
	}
	for _, d := range details {
		for k, v := range d {
			for _, leak := range []string{credToken, newCredToken, "SECRETtoken", "BRANDnew", "WXYZ", "QRST"} {
				if strings.Contains(v, leak) {
					t.Errorf("audit detail %s = %q carries %q", k, v, leak)
				}
			}
		}
	}
}

func TestOnlyAnInstanceAdminMayEditAnInstanceCredential(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	c := f.storeCredential(t, f.asAdmin(t), ghcrAcme)
	for name, u := range map[string]credentialUser{
		"env admin": f.asRoleOn(t, "envadmin", "env-a", "admin"),
		"operator":  f.asRoleOn(t, "operator", "env-a", "operator"),
		"viewer":    f.asRoleOn(t, "viewer", "env-a", "viewer"),
	} {
		rec := f.credCall(t, u, http.MethodPatch, "/api/registries/"+c.ID, `{"username":"intruder"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	var username string
	if err := f.st.Read.QueryRow(`SELECT username FROM registry_credentials WHERE id = ?`, c.ID).Scan(&username); err != nil || username != "bot" {
		t.Errorf("username = %q, %v; a refused edit changes nothing", username, err)
	}
}

func TestAnEnvironmentLayerCredentialCannotBeReachedThroughTheInstanceFamily(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	if _, err := f.st.Write.Exec(`INSERT INTO registry_credentials (id, env_id, name, type, host, prefix, username, secret_sealed, created_at, updated_at, created_by)
		VALUES ('env-cred', 'env-a', 'env-one', 'ghcr', 'ghcr.io', 'acme', 'bot', 'sealed', 1, 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	rec := f.credCall(t, f.asAdmin(t), http.MethodPatch, "/api/registries/env-cred", `{"username":"bot2"}`)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("%d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
	var username string
	if err := f.st.Read.QueryRow(`SELECT username FROM registry_credentials WHERE id = 'env-cred'`).Scan(&username); err != nil || username != "bot" {
		t.Errorf("username = %q, %v", username, err)
	}
	missing := f.credCall(t, f.asAdmin(t), http.MethodPatch, "/api/registries/no-such", `{"username":"bot2"}`)
	if missing.Code != http.StatusNotFound || problemType(t, missing) != "credential_not_found" {
		t.Errorf("missing: %d %s", missing.Code, missing.Body)
	}
}

func TestAnEditIsRefusedWithACodePerFieldAtFault(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)
	for _, tc := range []struct{ name, body, location, code string }{
		{"an empty name", `{"name":""}`, "body.name", "name_required"},
		{"an upper-case prefix", `{"prefix":"Acme"}`, "body.prefix", "prefix_invalid"},
		{"an empty username", `{"username":""}`, "body.username", "username_required"},
		{"an expiry in the past", `{"expires":"2025-01-01"}`, "body.expires", "expiry_past"},
		{"the fallback on another registry", `{"hubFallback":true}`, "body.hubFallback", "hub_fallback_not_hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, tc.body)
			p := decodeProblem(t, rec)
			if rec.Code != http.StatusUnprocessableEntity || p.Type != "validation_failed" || len(p.Errors) != 1 ||
				p.Errors[0].Location != tc.location || p.Errors[0].Message != tc.code {
				t.Errorf("%d %s, want 422 validation_failed %s %s", rec.Code, rec.Body, tc.location, tc.code)
			}
		})
	}
	_, details := f.registryAudit(t)
	if len(details) != 1 {
		t.Errorf("audit rows = %d; a refused edit records nothing", len(details))
	}
}

func TestEditingOntoAnotherCredentialIsAConflictNamingIt(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, ghcrAcme)
	b := f.storeCredential(t, admin, `{"name":"second","type":"ghcr","prefix":"other","username":"u","secret":"`+credToken+`"}`)
	f.storeCredential(t, admin, `{"name":"hub-all","type":"dockerhub","hubFallback":true,"username":"u","secret":"s3cret-token-value"}`)
	hub2 := f.storeCredential(t, admin, `{"name":"hub-ns","type":"dockerhub","prefix":"ns","username":"u","secret":"s3cret-token-value"}`)

	for _, tc := range []struct{ name, id, body, ptype, with string }{
		{"same name", b.ID, `{"name":"ghcr-acme"}`, "credential_name_taken", "ghcr-acme"},
		{"same host and prefix", b.ID, `{"prefix":"acme"}`, "host_prefix_taken", "ghcr-acme"},
		{"a second Hub fallback", hub2.ID, `{"hubFallback":true}`, "hub_fallback_taken", "hub-all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+tc.id, tc.body)
			p := decodeProblem(t, rec)
			if rec.Code != http.StatusConflict || p.Type != tc.ptype || len(p.Errors) != 1 || p.Errors[0].Message != tc.with {
				t.Errorf("%d %s, want 409 %s naming %q", rec.Code, rec.Body, tc.ptype, tc.with)
			}
		})
	}
}

// Deleting a credential (plan 05-07, D-08, D-31).

func TestAnAdminDeletesACredentialAndASecondDeleteIsNotFoundAndWritesNothing(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)

	rec := f.credCall(t, admin, http.MethodDelete, "/api/registries/"+c.ID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v", n, err)
	}
	if got := f.credCall(t, admin, http.MethodGet, "/api/registries/"+c.ID, ""); got.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", got.Code)
	}

	again := f.credCall(t, admin, http.MethodDelete, "/api/registries/"+c.ID, "")
	if again.Code != http.StatusNotFound || problemType(t, again) != "credential_not_found" {
		t.Errorf("second delete: %d %s, want 404 credential_not_found", again.Code, again.Body)
	}
	actions, details := f.registryAudit(t)
	if !slices.Equal(actions, []string{"registry.credential_created", "registry.credential_deleted"}) {
		t.Fatalf("actions = %v; the repeated delete must write nothing", actions)
	}
	want := map[string]string{"name": "ghcr-acme", "layer": "instance", "host": "ghcr.io", "prefix": "acme"}
	for k, v := range want {
		if details[1][k] != v {
			t.Errorf("details[%s] = %q, want %q", k, details[1][k], v)
		}
	}
	for k, v := range details[1] {
		for _, leak := range []string{credToken, "SECRETtoken", "WXYZ"} {
			if strings.Contains(v, leak) {
				t.Errorf("audit detail %s = %q carries %q", k, v, leak)
			}
		}
	}
}

func TestOnlyAnInstanceAdminMayDeleteAnInstanceCredential(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	c := f.storeCredential(t, f.asAdmin(t), ghcrAcme)
	for name, u := range map[string]credentialUser{
		"env admin": f.asRoleOn(t, "envadmin", "env-a", "admin"),
		"operator":  f.asRoleOn(t, "operator", "env-a", "operator"),
		"viewer":    f.asRoleOn(t, "viewer", "env-a", "viewer"),
	} {
		if rec := f.credCall(t, u, http.MethodDelete, "/api/registries/"+c.ID, ""); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v; a refused delete removes nothing", n, err)
	}
}

func TestAnEnvironmentLayerCredentialCannotBeDeletedThroughTheInstanceFamily(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "env-a")
	if _, err := f.st.Write.Exec(`INSERT INTO registry_credentials (id, env_id, name, type, host, prefix, username, secret_sealed, created_at, updated_at, created_by)
		VALUES ('env-cred', 'env-a', 'env-one', 'ghcr', 'ghcr.io', 'acme', 'bot', 'sealed', 1, 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	rec := f.credCall(t, f.asAdmin(t), http.MethodDelete, "/api/registries/env-cred", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("%d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rows = %d, %v", n, err)
	}
}

func TestACreateThatClashesNamesTheOtherCredential(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, `{"name":"hub-all","type":"dockerhub","hubFallback":true,"username":"u","secret":"s3cret-token-value"}`)
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries",
		`{"name":"hub-two","type":"dockerhub","prefix":"acme","hubFallback":true,"username":"u","secret":"s3cret-token-value"}`)
	p := decodeProblem(t, rec)
	if rec.Code != http.StatusConflict || p.Type != "hub_fallback_taken" || len(p.Errors) != 1 ||
		p.Errors[0].Location != "conflict.credential" || p.Errors[0].Message != "hub-all" {
		t.Errorf("%d %s, want 409 hub_fallback_taken naming hub-all", rec.Code, rec.Body)
	}
}

// Which credential a pull would use (plan 05-08, IMG-02, D-09, D-10, D-24a). The tester asks the
// same resolver pulls use, on the instance layer and the chosen environment's layer only.

type resolveScope struct {
	Kind    string  `json:"kind"`
	EnvID   *string `json:"envId"`
	EnvName *string `json:"envName"`
}

type resolveResp struct {
	Input      string `json:"input"`
	Normalized string `json:"normalized"`
	Credential *struct {
		ID    string       `json:"id"`
		Name  string       `json:"name"`
		Scope resolveScope `json:"scope"`
	} `json:"credential"`
	MatchedOn  string `json:"matchedOn"`
	Considered []struct {
		CredentialID string       `json:"credentialId"`
		Name         string       `json:"name"`
		Scope        resolveScope `json:"scope"`
		Matches      bool         `json:"matches"`
		Result       string       `json:"result"`
	} `json:"considered"`
}

// addEnvCredential stores a credential of one environment directly: the environment family of
// credential routes is a later plan's.
func addEnvCredential(t *testing.T, f *envFix, envID string, in registry.CreateInput) {
	t.Helper()
	in.EnvID = envID
	if in.Type == "" {
		in.Type = "ghcr"
	}
	in.Username, in.Secret = "bot", credToken
	if _, err := registry.NewStore(f.st, f.v, f.clk.now).Create(context.Background(), in, "id-admin", nil); err != nil {
		t.Fatalf("store the environment credential: %v", err)
	}
}

func TestAViewerLearnsWhichCredentialAPullWouldUseAndWhy(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	e, other := f.twoEnvironments(t)
	admin := credentialUser{f.cookie, f.csrf}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries", ghcrAcme); rec.Code != http.StatusCreated {
		t.Fatalf("create the instance credential: %d %s", rec.Code, rec.Body)
	}
	addEnvCredential(t, f, e.ID, registry.CreateInput{Name: "ghcr-team", Prefix: "acme/team"})
	// Another environment's credential must never be considered for this one.
	addEnvCredential(t, f, other.ID, registry.CreateInput{Name: "ghcr-other", Prefix: "acme/team/app"})

	viewer := f.asRoleOn(t, "viewer", e.ID, "viewer")
	rec := f.credCall(t, viewer, http.MethodPost, "/api/env/"+e.ID+"/registries/resolve", `{"ref":"ghcr.io/acme/team/app:1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	var got resolveResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Input != "ghcr.io/acme/team/app:1" || got.Normalized != "ghcr.io/acme/team/app:1" || got.MatchedOn != "prefix" {
		t.Errorf("answer = %+v", got)
	}
	if got.Credential == nil || got.Credential.Name != "ghcr-team" || got.Credential.Scope.Kind != "environment" ||
		got.Credential.Scope.EnvID == nil || *got.Credential.Scope.EnvID != e.ID {
		t.Fatalf("credential = %+v, want the environment's ghcr-team", got.Credential)
	}
	if len(got.Considered) != 2 || got.Considered[0].Name != "ghcr-team" || got.Considered[0].Result != "used" ||
		got.Considered[1].Name != "ghcr-acme" || got.Considered[1].Result != "shorter_prefix" || !got.Considered[1].Matches {
		t.Errorf("considered = %+v, want ghcr-team used then ghcr-acme with a shorter prefix", got.Considered)
	}
	if strings.Contains(rec.Body.String(), "ghcr-other") || strings.Contains(rec.Body.String(), credToken) || strings.Contains(rec.Body.String(), "SECRETtoken") {
		t.Errorf("the answer leaks another environment's credential or a secret: %s", rec.Body)
	}
}

func TestAnImageNoStoredCredentialMatchesIsAnonymousWithTheReferenceNormalised(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	viewer := f.asRoleOn(t, "viewer", e.ID, "viewer")
	rec := f.credCall(t, viewer, http.MethodPost, "/api/env/"+e.ID+"/registries/resolve", `{"ref":"nginx"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	var got resolveResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Input != "nginx" || got.Normalized != "docker.io/library/nginx:latest" || got.Credential != nil || got.MatchedOn != "" || got.Considered == nil || len(got.Considered) != 0 {
		t.Errorf("answer = %+v, want anonymous and nothing considered", got)
	}
}

func TestAHubFallbackAnswersAnOfficialImageAndAGhcrCredentialNeverAnswersAnotherHost(t *testing.T) {
	f, e := dashEnv(t, dockertest.FakeConfig{Hostname: "node-1"})
	admin := credentialUser{f.cookie, f.csrf}
	for _, body := range []string{
		`{"name":"hub-all","type":"dockerhub","username":"bot","secret":"` + credToken + `","hubFallback":true}`,
		ghcrAcme,
	} {
		if rec := f.credCall(t, admin, http.MethodPost, "/api/registries", body); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}
	resolve := func(ref string) resolveResp {
		t.Helper()
		rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+e.ID+"/registries/resolve", `{"ref":"`+ref+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve %s: %d %s", ref, rec.Code, rec.Body)
		}
		var got resolveResp
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := resolve("nginx"); got.Credential == nil || got.Credential.Name != "hub-all" || got.MatchedOn != "hub_fallback" {
		t.Errorf("nginx = %+v, want the Hub fallback", got)
	}
	if got := resolve("ghcr.io.evil.example/acme/app"); got.Credential != nil {
		t.Errorf("a look-alike host used %+v, want anonymous", got.Credential)
	}
	if got := resolve("ghcr.io/acmecorp/app"); got.Credential != nil {
		t.Errorf("ghcr.io/acmecorp used %+v, want anonymous", got.Credential)
	}
}

func TestResolvingRefusesAnUnreadableReferenceAndACallerWhoCannotReadTheEnvironment(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	e, other := f.twoEnvironments(t)
	path := "/api/env/" + e.ID + "/registries/resolve"
	admin := credentialUser{f.cookie, f.csrf}

	for _, ref := range []string{"Bad Ref!", "GHCR.io/Acme/app", "   "} {
		body, _ := json.Marshal(map[string]string{"ref": ref})
		rec := f.credCall(t, admin, http.MethodPost, path, string(body))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("ref %q: %d %s, want 422", ref, rec.Code, rec.Body)
		} else if ref != "   " && problemType(t, rec) != "ref_invalid" {
			t.Errorf("ref %q: type %q, want ref_invalid", ref, problemType(t, rec))
		}
	}

	// An environment the caller cannot read is not there for them: the platform answers 404
	// environment_not_found, exactly as for one that does not exist, and nothing is resolved.
	elsewhere := f.asRoleOn(t, "elsewhere", other.ID, "viewer")
	if rec := f.credCall(t, elsewhere, http.MethodPost, path, `{"ref":"nginx"}`); rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
		t.Errorf("a Viewer of another environment: %d %s, want 404 environment_not_found", rec.Code, rec.Body)
	}
	f.addUser(t, "nobody", false)
	cookie, csrf := f.signIn(t, "nobody", false)
	if rec := f.credCall(t, credentialUser{cookie, csrf}, http.MethodPost, path, `{"ref":"nginx"}`); rec.Code != http.StatusNotFound {
		t.Errorf("a user with no role: %d %s, want 404", rec.Code, rec.Body)
	}
}

// Environment-layer credentials (plan 05-10, IMG-01, IMG-02, D-01, D-02, D-30).

const envCredBody = `{"name":"ghcr-team","type":"generic","host":"localhost:5000","prefix":"private","username":"bot","secret":"` + pullCredSecret + `"}`

func TestAnEnvAdminAddsAnEnvCredentialForTheirEnvironmentAndItIsAuditedWithThatEnvironment(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := f.asRoleOn(t, "envadmin", a.ID, "admin")

	rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/registries", envCredBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c := decodeCred(t, rec)
	if c.ID == "" || c.Scope.Kind != "environment" || c.Scope.EnvID == nil || *c.Scope.EnvID != a.ID ||
		c.Scope.EnvName == nil || *c.Scope.EnvName != a.Name || !c.CanManage || c.Last4 != "6789" {
		t.Errorf("credential = %+v, want an environment credential of %s that the caller manages", c, a.Name)
	}
	if strings.Contains(rec.Body.String(), pullCredSecret) {
		t.Errorf("the response carries the secret: %s", rec.Body)
	}

	var envID, layer string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(env_id, ''), details->>'layer' FROM audit_log WHERE action = 'registry.credential_created'`).Scan(&envID, &layer); err != nil {
		t.Fatal(err)
	}
	if envID != a.ID || layer != "environment" {
		t.Errorf("audit env_id = %q layer = %q, want %s and environment", envID, layer, a.ID)
	}
	// The Env Admin reads that row on the audit page (P3 audit scoping).
	list := f.credCall(t, admin, http.MethodGet, "/api/audit", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"registry.credential_created"`) {
		t.Errorf("audit page: %d %s, want the environment's credential row", list.Code, list.Body)
	}
}

func TestEnvCredentialRoutesRefuseWhoHoldNoAdminRoleOnTheEnvironmentAndHideOthers(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)

	for name, tc := range map[string]struct {
		u    credentialUser
		want int
	}{
		"operator of A":     {f.asRoleOn(t, "operator", a.ID, "operator"), http.StatusForbidden},
		"viewer of A":       {f.asRoleOn(t, "viewer", a.ID, "viewer"), http.StatusForbidden},
		"admin of B only":   {f.asRoleOn(t, "adminb", b.ID, "admin"), http.StatusNotFound},
		"user with no role": {credentialUser{nobodyCookie, nobodyCSRF}, http.StatusNotFound},
	} {
		rec := f.credCall(t, tc.u, http.MethodPost, "/api/env/"+a.ID+"/registries", envCredBody)
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
		if tc.want == http.StatusNotFound && problemType(t, rec) != "environment_not_found" {
			t.Errorf("%s: %s, want environment_not_found", name, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v; a refused create stores nothing", n, err)
	}
	// An instance administrator may add to any environment.
	if rec := f.credCall(t, credentialUser{f.cookie, f.csrf}, http.MethodPost, "/api/env/"+b.ID+"/registries", envCredBody); rec.Code != http.StatusCreated {
		t.Errorf("instance admin: %d %s", rec.Code, rec.Body)
	}
}

func TestEnvCredentialMayShareAnInstanceCredentialsHostAndPrefixButNotTwiceInOneEnvironment(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := credentialUser{f.cookie, f.csrf}
	f.storeCredential(t, admin, `{"name":"ghcr-team","type":"generic","host":"localhost:5000","prefix":"private","username":"inst","secret":"`+credToken+`"}`)

	// D-02: the same name, host and prefix as the instance's is allowed in an environment.
	first := f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/registries", envCredBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	// A second in the same environment for the same host and prefix is host_prefix_taken, naming the first.
	dup := f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/registries",
		`{"name":"other","type":"generic","host":"localhost:5000","prefix":"private","username":"bot","secret":"`+pullCredSecret+`"}`)
	if dup.Code != http.StatusConflict || problemType(t, dup) != "host_prefix_taken" || !strings.Contains(dup.Body.String(), "ghcr-team") {
		t.Errorf("duplicate: %d %s, want 409 host_prefix_taken naming ghcr-team", dup.Code, dup.Body)
	}
	if strings.Contains(dup.Body.String(), "instance credential") {
		t.Errorf("the conflict words the wrong layer: %s", dup.Body)
	}
	bad := f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/registries", `{"name":""}`)
	if bad.Code != http.StatusUnprocessableEntity || problemType(t, bad) != "validation_failed" {
		t.Errorf("invalid: %d %s, want 422 validation_failed", bad.Code, bad.Body)
	}
}

func TestAnEnvCredentialAnAdminAddedIsUsedByThatEnvironmentsPullsAndNoOthers(t *testing.T) {
	cfg := dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true}
	f, env, node, _ := twoNodes(t, cfg, dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	// A second environment, with one node of its own.
	other := f.srv.NewEnvironment(t, "Other")
	charlieFacts := pairFacts("charlie", false)
	charlieFacts.Swarm.ClusterID = "cluster-2"
	otherNode := f.connect(t, other, dockertest.FakeConfig{Hostname: "charlie", SwarmState: "active", SwarmControl: true}, charlieFacts)
	f.whenState(t, other.ID, "online")
	f.whenProbed(t, envs.AgentTarget(otherNode.id))

	admin := credentialUser{f.cookie, f.csrf}
	// The instance credential covers the same registry and namespace, with another user.
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries",
		`{"name":"inst","type":"generic","host":"localhost:5000","prefix":"private","username":"instbot","secret":"`+credToken+`"}`); rec.Code != http.StatusCreated {
		t.Fatalf("instance create: %d %s", rec.Code, rec.Body)
	}
	envAdmin := f.asRoleOn(t, "envadmin", env.ID, "admin")
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+env.ID+"/registries", envCredBody); rec.Code != http.StatusCreated {
		t.Fatalf("environment create: %d %s", rec.Code, rec.Body)
	}

	// The environment's own credential wins for its pulls (D-02) ...
	started := f.startPullOn(t, env.ID)
	if job := f.pullDone(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	for _, h := range node.fake.PullAuth() {
		if user, pw, _ := decodedPullAuth(t, h); user != "bot" || pw != pullCredSecret {
			t.Errorf("the environment's pull used %q, want its own credential", user)
		}
	}
	if len(node.fake.PullAuth()) != 1 {
		t.Errorf("headers = %q, want one", node.fake.PullAuth())
	}

	// ... and another environment's pulls use the instance credential, never this one.
	started = f.startPullOn(t, other.ID)
	if job := f.pullDone(t, started.JobID); job.State != "done" {
		t.Fatalf("job = %+v", job)
	}
	h := otherNode.fake.PullAuth()
	if len(h) != 1 {
		t.Fatalf("headers = %q, want one", h)
	}
	if user, pw, _ := decodedPullAuth(t, h[0]); user != "instbot" || pw != credToken {
		t.Errorf("the other environment's pull used %q, want the instance credential", user)
	}
}

// startPullOn pulls the private image on one environment as the signed-in admin.
func (f *agentFix) startPullOn(t *testing.T, envID string) pullStarted {
	t.Helper()
	rec := f.req(t, http.MethodPost, "/api/env/"+envID+"/images/pull", `{"ref":"`+privateRef+`"}`)
	if rec.code != http.StatusAccepted {
		t.Fatalf("pull: %d %s", rec.code, rec.body)
	}
	return decode[pullStarted](t, rec)
}

// A credential of one environment is managed through that environment's routes only.
func TestEnvCredentialEditAndDeleteStayInTheirEnvironmentAndLayer(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	instance, inA, inB := f.layeredCredentials(t, a, b)
	adminA := f.asRoleOn(t, "admina", a.ID, "admin")
	path := func(env, id string) string { return "/api/env/" + env + "/registries/" + id }
	username := func(id string) string {
		var u string
		if err := f.st.Read.QueryRow(`SELECT username FROM registry_credentials WHERE id = ?`, id).Scan(&u); err != nil {
			return "(" + err.Error() + ")"
		}
		return u
	}

	// A's own credential.
	rec := f.credCall(t, adminA, http.MethodPatch, path(a.ID, inA.ID), `{"username":"bot2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if c := decodeCred(t, rec); c.Username != "bot2" || c.Scope.Kind != "environment" || c.Scope.EnvID == nil || *c.Scope.EnvID != a.ID || !c.CanManage || c.Last4 != "WXYZ" {
		t.Errorf("patched = %+v", c)
	}
	if strings.Contains(rec.Body.String(), "SECRETtoken") {
		t.Errorf("the response carries the secret: %s", rec.Body)
	}

	// Another environment's credential and the instance's are 404 credential_not_found on A's routes,
	// exactly as an unknown ID is, and nothing changes.
	for name, id := range map[string]string{"B's credential": inB.ID, "the instance's": instance.ID, "an unknown one": "no-such"} {
		for method, body := range map[string]string{http.MethodPatch: `{"username":"intruder"}`, http.MethodDelete: ""} {
			rec := f.credCall(t, adminA, method, path(a.ID, id), body)
			if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
				t.Errorf("%s %s: %d %s, want 404 credential_not_found", method, name, rec.Code, rec.Body)
			}
		}
	}
	if username(inB.ID) != "bot" || username(instance.ID) != "bot" {
		t.Errorf("a refused request changed another layer: B %q, instance %q", username(inB.ID), username(instance.ID))
	}
	// An instance admin reaches B's credential through B's routes, and A's through A's, not crosswise.
	admin := credentialUser{f.cookie, f.csrf}
	if rec := f.credCall(t, admin, http.MethodPatch, path(b.ID, inA.ID), `{"username":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("A's credential through B's route: %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPatch, path(b.ID, inB.ID), `{"username":"botb"}`); rec.Code != http.StatusOK || username(inB.ID) != "botb" {
		t.Errorf("B's credential through B's route: %d %s", rec.Code, rec.Body)
	}

	// Delete A's, then again.
	if rec := f.credCall(t, adminA, http.MethodDelete, path(a.ID, inA.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, adminA, http.MethodDelete, path(a.ID, inA.ID), ""); rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("second delete: %d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}

	// The audit rows carry the environment and never the secret.
	rows, err := f.st.Read.Query(`SELECT action, COALESCE(env_id, ''), details FROM audit_log WHERE action IN ('registry.credential_updated', 'registry.credential_deleted') AND target_id = ? ORDER BY seq`, inA.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var action, env, details string
		if err := rows.Scan(&action, &env, &details); err != nil {
			t.Fatal(err)
		}
		if env != a.ID || strings.Contains(details, "SECRETtoken") || strings.Contains(details, "WXYZ") {
			t.Errorf("audit row %s env %q details %s", action, env, details)
		}
		got = append(got, action)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "registry.credential_updated,registry.credential_deleted" {
		t.Errorf("audit actions = %v, want an update and a delete", got)
	}
}

func TestEnvCredentialItemRoutesAreForTheAdminOfTheEnvironmentOnly(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	_, inA, _ := f.layeredCredentials(t, a, b)
	f.addUser(t, "nobody", false)
	nobodyCookie, nobodyCSRF := f.signIn(t, "nobody", false)
	path := "/api/env/" + a.ID + "/registries/" + inA.ID

	for name, tc := range map[string]struct {
		u    credentialUser
		want int
	}{
		"operator of A":     {f.asRoleOn(t, "operatora", a.ID, "operator"), http.StatusForbidden},
		"viewer of A":       {f.asRoleOn(t, "viewera", a.ID, "viewer"), http.StatusForbidden},
		"admin of B only":   {f.asRoleOn(t, "adminb", b.ID, "admin"), http.StatusNotFound},
		"user with no role": {credentialUser{nobodyCookie, nobodyCSRF}, http.StatusNotFound},
	} {
		for method, body := range map[string]string{http.MethodPatch: `{"username":"intruder"}`, http.MethodDelete: ""} {
			rec := f.credCall(t, tc.u, method, path, body)
			if rec.Code != tc.want {
				t.Errorf("%s %s: %d %s, want %d", name, method, rec.Code, rec.Body, tc.want)
			}
		}
	}
	var username string
	if err := f.st.Read.QueryRow(`SELECT username FROM registry_credentials WHERE id = ?`, inA.ID).Scan(&username); err != nil || username != "bot" {
		t.Errorf("username = %q, %v; refused requests change and delete nothing", username, err)
	}
}

func TestEditingAnEnvCredentialOntoAnotherOfTheEnvironmentIsAConflictNamingItButNotOntoTheInstances(t *testing.T) {
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := credentialUser{f.cookie, f.csrf}
	mk := func(path, name, prefix string) credentialResp {
		rec := f.credCall(t, admin, http.MethodPost, path,
			`{"name":"`+name+`","type":"ghcr","prefix":"`+prefix+`","username":"bot","secret":"`+credToken+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
		return decodeCred(t, rec)
	}
	mk("/api/registries", "inst", "acme")
	first := mk("/api/env/"+a.ID+"/registries", "first", "team")
	second := mk("/api/env/"+a.ID+"/registries", "second", "other")
	item := func(id string) string { return "/api/env/" + a.ID + "/registries/" + id }

	rec := f.credCall(t, admin, http.MethodPatch, item(second.ID), `{"prefix":"team"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "host_prefix_taken" || !strings.Contains(rec.Body.String(), "first") {
		t.Errorf("onto first: %d %s, want 409 host_prefix_taken naming first", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodPatch, item(second.ID), `{"name":"first"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "credential_name_taken" {
		t.Errorf("name: %d %s, want 409 credential_name_taken", rec.Code, rec.Body)
	}
	// The instance's host and prefix are free to an environment (D-02), and so is its name.
	if rec := f.credCall(t, admin, http.MethodPatch, item(second.ID), `{"prefix":"acme","name":"inst"}`); rec.Code != http.StatusOK {
		t.Errorf("onto the instance's: %d %s, want 200", rec.Code, rec.Body)
	}
	_ = first
}

// Testing a credential (plan 05-11, IMG-01, D-12, D-31). The registry is a real TLS server
// in the test that answers the way the live ones were probed to.

type fakeRegistry struct {
	srv *httptest.Server
	// refuse is the token endpoint's answer to bad credentials.
	refuse int
	// tagsStatus is the answer to the tag list of a signed-in client.
	tagsStatus int
	user, pass string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{refuse: http.StatusUnauthorized, tagsStatus: http.StatusOK, user: "bot", pass: "registry-secret-value-9876"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := func(scope string) {
			c := `Bearer realm="` + f.srv.URL + `/token",service="fake"`
			if scope != "" {
				c += `,scope="` + scope + `"`
			}
			w.Header().Set("WWW-Authenticate", c)
			w.WriteHeader(http.StatusUnauthorized)
		}
		switch {
		case r.URL.Path == "/v2/":
			if r.Header.Get("Authorization") == "Bearer good" {
				return
			}
			challenge("")
		case r.URL.Path == "/token":
			if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
				w.WriteHeader(f.refuse)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "good", "expires_in": 300})
		case strings.HasSuffix(r.URL.Path, "/tags/list"):
			repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v2/"), "/tags/list")
			switch {
			case r.Header.Get("Authorization") != "Bearer good", f.tagsStatus == http.StatusUnauthorized:
				challenge("repository:" + repo + ":pull")
			case f.tagsStatus != http.StatusOK:
				w.WriteHeader(f.tagsStatus)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": []string{"1"}})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "https://") }

// trusting makes the fixture's tests dial through a policy that trusts the registry's
// certificate, and rebuilds the router.
func (f *authFixture) trusting(t *testing.T, regs ...*fakeRegistry) {
	t.Helper()
	pool := x509.NewCertPool()
	for _, r := range regs {
		pool.AddCert(r.srv.Certificate())
	}
	f.registryEgress = registry.NewEgressPolicy(nil).WithRootCAs(pool)
	f.h = f.build(t)
}

type testStep struct {
	State string `json:"state"`
	Code  string `json:"code"`
}

type testResult struct {
	Reachable testStep `json:"reachable"`
	SignedIn  testStep `json:"signedIn"`
	CanPull   testStep `json:"canPull"`
	TestedAt  string   `json:"testedAt"`
}

func (f *fakeRegistry) body(secret, repo string) string {
	b, _ := json.Marshal(map[string]string{"type": "generic", "host": f.host(), "username": f.user, "secret": secret, "testRepository": repo})
	return string(b)
}

func decodeTest(t *testing.T, rec *httptest.ResponseRecorder) testResult {
	t.Helper()
	var r testResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

func TestAnInstanceAdminTestsUnsavedValuesAndSeesEachStepPass(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", reg.body(reg.pass, "acme/app"))
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	got := decodeTest(t, rec)
	if got.Reachable.State != "passed" || got.SignedIn.State != "passed" || got.CanPull.State != "passed" || got.TestedAt == "" {
		t.Fatalf("result = %+v, want every step passed", got)
	}
	if strings.Contains(rec.Body.String(), reg.pass) {
		t.Errorf("the answer carries the secret: %s", rec.Body)
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials = %d, %v; a test of unsaved values stores nothing", n, err)
	}
	actions, details := f.registryAudit(t)
	if !slices.Equal(actions, []string{"registry.credential_tested"}) {
		t.Fatalf("actions = %v, want one registry.credential_tested", actions)
	}
	for k, want := range map[string]string{"layer": "instance", "host": reg.host(), "type": "generic", "via": "server", "reachable": "passed", "signed_in": "passed", "can_pull": "passed", "repository": "acme/app"} {
		if details[0][k] != want {
			t.Errorf("details[%s] = %q, want %q", k, details[0][k], want)
		}
	}
	for k, v := range details[0] {
		if strings.Contains(v, reg.pass) || strings.Contains(k, "secret") {
			t.Errorf("audit detail %s = %q carries the secret", k, v)
		}
	}
}

func TestATestWithBadCredentialsNamesTheFailedStepWhetherTheRegistrySays401Or403(t *testing.T) {
	for name, status := range map[string]int{"401": http.StatusUnauthorized, "403": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			f := newAuthFixture(t, config.Config{})
			reg := newFakeRegistry(t)
			reg.refuse = status
			f.trusting(t, reg)
			rec := f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries/test", reg.body("wrong-password-123456", "acme/app"))
			if rec.Code != http.StatusOK {
				t.Fatalf("test: %d %s", rec.Code, rec.Body)
			}
			got := decodeTest(t, rec)
			if got.Reachable.State != "passed" || got.SignedIn != (testStep{"failed", "sign_in_refused"}) || got.CanPull.State != "not_checked" {
				t.Fatalf("result = %+v", got)
			}
			_, details := f.registryAudit(t)
			if details[0]["code"] != "sign_in_refused" || details[0]["signed_in"] != "failed" {
				t.Errorf("details = %v", details[0])
			}
			var result, reason string
			if err := f.st.Read.QueryRow(`SELECT result, COALESCE(reason, '') FROM audit_log WHERE action = 'registry.credential_tested'`).Scan(&result, &reason); err != nil || result != "failure" || reason != "sign_in_refused" {
				t.Errorf("audit result = %q, reason %q, %v, want failure sign_in_refused", result, reason, err)
			}
		})
	}
}

func TestATestOfTheMetadataAddressNeverConnectsAndSaysUnreachable(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	rec := f.credCall(t, f.asAdmin(t), http.MethodPost, "/api/registries/test", `{"type":"generic","host":"169.254.169.254","username":"bot","secret":"does-not-matter-1234"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	got := decodeTest(t, rec)
	if got.Reachable != (testStep{"failed", "registry_unreachable"}) || got.SignedIn.State != "not_checked" {
		t.Fatalf("result = %+v, want unreachable and the rest not checked", got)
	}
}

func TestATestNeedsTheCredentialsPermissionOfItsLayer(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	f.addEnvironmentRow(t, "e1")
	envAdmin := f.asRoleOn(t, "envadmin", "e1", "admin")
	rec := f.credCall(t, envAdmin, http.MethodPost, "/api/registries/test", `{"type":"generic","host":"169.254.169.254","username":"bot","secret":"does-not-matter-1234"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an env Admin on the instance family: %d %s, want 403", rec.Code, rec.Body)
	}
	if actions, _ := f.registryAudit(t); len(actions) != 0 {
		t.Errorf("actions = %v; a refused test records no test", actions)
	}
}

func TestATestRefusesWhatAddingWouldRefuseAndWhatCannotBeTried(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	for _, tc := range []struct{ name, body, location, code string }{
		{"no secret", `{"type":"generic","host":"r.example.org","username":"bot"}`, "body.secret", "secret_required"},
		{"no username", `{"type":"generic","host":"r.example.org","secret":"abc"}`, "body.username", "username_required"},
		{"a bad host", `{"type":"generic","host":"http://r.example.org","username":"bot","secret":"abc"}`, "body.host", "host_invalid"},
		{"no type", `{"host":"r.example.org","username":"bot","secret":"abc"}`, "body.type", "type_invalid"},
		{"a bad test repository", `{"type":"generic","host":"r.example.org","username":"bot","secret":"abc","testRepository":"A b?"}`, "body.testRepository", "test_repository_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", tc.body)
			p := decodeProblem(t, rec)
			if rec.Code != http.StatusUnprocessableEntity || p.Type != "validation_failed" || len(p.Errors) != 1 || p.Errors[0].Location != tc.location || p.Errors[0].Message != tc.code {
				t.Errorf("%d %s, want 422 validation_failed %s %s", rec.Code, rec.Body, tc.location, tc.code)
			}
		})
	}
	if actions, _ := f.registryAudit(t); len(actions) != 0 {
		t.Errorf("actions = %v; a refused test records nothing", actions)
	}
}

func TestTheEleventhTestInAMinuteIsRateLimited(t *testing.T) {
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	body := `{"type":"generic","host":"169.254.169.254","username":"bot","secret":"does-not-matter-1234"}`
	for i := 1; i <= 10; i++ {
		if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", body); rec.Code != http.StatusOK {
			t.Fatalf("test %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", body)
	if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
		t.Fatalf("test 11: %d %s, want 429 rate_limited", rec.Code, rec.Body)
	}
	if actions, _ := f.registryAudit(t); len(actions) != 10 {
		t.Errorf("audit rows = %d, want 10: the refused one writes none", len(actions))
	}
}
