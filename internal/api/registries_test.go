// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/vault"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Registry credentials (plan 05-05, IMG-01, D-01, D-05, D-09, D-30, D-31). The router, policy,
// CSRF guard, vault and audit writer are the real ones.

// credToken is a secret of 12 or more characters, so its last 4 are kept.
const credToken = "ghp_SECRETtoken0123456789abcdefWXYZ" //nolint:gosec // G101: a made-up token for the test

const ghcrAcme = `{"name":"ghcr-acme","type":"ghcr","prefix":"acme","username":"bot","secret":"` + credToken + `"}`

// credentialKeys is the exact key set of CredentialBody: a new key is a new thing the browser can
// learn, and it must be added here on purpose.
var credentialKeys = []string{
	"caPem", "canManage", "createdAt", "expiresAt", "hasCa", "hubFallback", "host", "id", "insecure", "last4", "lastFailure", "lastTest",
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
	CAPEM       string  `json:"caPem"`
	HasCA       bool    `json:"hasCa"`
	Insecure    bool    `json:"insecure"`
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.credCall(t, f.asAdmin(t), http.MethodGet, "/api/registries/no-such-credential", "")
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("%d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
}

func TestACredentialIsRefusedWithACodePerFieldAtFault(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		want := map[string]string{"name": "ghcr-acme", "layer": "instance", "host": "ghcr.io", "prefix": "acme", "type": "ghcr", "username": "bot", "hub_fallback": "false", "expires": "2199-12-31", "insecure": "false", "custom_ca": "false"}
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

func TestAnAdminEditsANamespaceAndTheSecretStaysWhereItWas(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)
	var before string
	if err := f.st.Read.QueryRow(`SELECT secret_sealed FROM registry_credentials WHERE id = ?`, c.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"prefix":"team"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	got := decodeCred(t, rec)
	if got.Username != "bot" || got.Name != "ghcr-acme" || got.Prefix != "team" || got.Last4 != "WXYZ" || got.Scope.Kind != "instance" {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, ghcrAcme)
	if rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"prefix":"team"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"username":"bot3","secret":"`+newCredToken+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}

	actions, details := f.registryAudit(t)
	if !slices.Equal(actions, []string{"registry.credential_created", "registry.credential_updated", "registry.credential_updated"}) {
		t.Fatalf("actions = %v", actions)
	}
	if details[1]["changed"] != "prefix" || details[2]["changed"] != "secret,username" {
		t.Errorf("changed = %q and %q, want prefix and secret,username", details[1]["changed"], details[2]["changed"])
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	rec := f.credCall(t, adminA, http.MethodPatch, path(a.ID, inA.ID), `{"username":"bot2","secret":"`+newCredToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if c := decodeCred(t, rec); c.Username != "bot2" || c.Scope.Kind != "environment" || c.Scope.EnvID == nil || *c.Scope.EnvID != a.ID || !c.CanManage || c.Last4 != "QRST" {
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
	if rec := f.credCall(t, admin, http.MethodPatch, path(b.ID, inB.ID), `{"username":"botb","secret":"`+newCredToken+`"}`); rec.Code != http.StatusOK || username(inB.ID) != "botb" {
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
	t.Parallel()
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
	t.Parallel()
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
	// tags are the repository's tag names; nil answers the one tag "1". tagLists counts the
	// tag lists the registry served to a signed-in client.
	tags       []string
	tagLists   atomic.Int32
	user, pass string
	// extra, when set, answers any request the fake does not (manifests and blobs).
	extra http.HandlerFunc
	// hits counts every request the registry has been sent.
	hits atomic.Int32
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{refuse: http.StatusUnauthorized, tagsStatus: http.StatusOK, user: "bot", pass: "registry-secret-value-9876"}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
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
				f.tagLists.Add(1)
				names := f.tags
				if names == nil {
					names = []string{"1"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names})
			}
		case f.extra != nil:
			f.extra(w, r)
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Testing a stored credential and remembering the result (plan 05-11 task 2, D-12).

type storedTest struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

func (f *authFixture) storedTest(t *testing.T, u credentialUser, id string) *storedTest {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, "/api/registries/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	var c struct {
		LastTest *storedTest `json:"lastTest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c.LastTest
}

func (f *fakeRegistry) credential(name, secret string) string {
	b, _ := json.Marshal(map[string]string{"name": name, "type": "generic", "host": f.host(), "username": f.user, "secret": secret})
	return string(b)
}

func TestTestingAStoredCredentialUsesItsStoredSecretAndRemembersTheResult(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, reg.credential("mine", reg.pass))
	if f.storedTest(t, admin, c.ID) != nil {
		t.Fatal("a new credential has a last test")
	}

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", `{"credentialId":"`+c.ID+`","testRepository":"acme/app"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := decodeTest(t, rec); got.SignedIn.State != "passed" || got.CanPull.State != "passed" {
		t.Fatalf("result = %+v, want passed with the stored secret", got)
	}
	last := f.storedTest(t, admin, c.ID)
	if last == nil || !last.OK || last.Detail != "" || last.At == "" {
		t.Fatalf("lastTest = %+v, want a pass", last)
	}
	_, details := f.registryAudit(t)
	if got := details[len(details)-1]; got["name"] != "mine" || got["signed_in"] != "passed" {
		t.Errorf("audit details = %v, want the credential's name", got)
	}
	if strings.Contains(rec.Body.String(), reg.pass) {
		t.Errorf("the answer carries the stored secret: %s", rec.Body)
	}
}

func TestAFailedTestOfTheStoredValuesIsRememberedWithItsCode(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, reg.credential("stale-token", "an-old-secret-value-1234"))

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", `{"credentialId":"`+c.ID+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := decodeTest(t, rec); got.SignedIn != (testStep{"failed", "sign_in_refused"}) {
		t.Fatalf("result = %+v", got)
	}
	last := f.storedTest(t, admin, c.ID)
	if last == nil || last.OK || last.Detail != "sign_in_refused" {
		t.Fatalf("lastTest = %+v, want a failure with the code", last)
	}
	// A credential can be saved after a failed test: the failed result blocks nothing.
	if rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"name":"renamed-lab"}`); rec.Code != http.StatusOK {
		t.Errorf("save after a failed test: %d %s", rec.Code, rec.Body)
	}
}

func TestATestOfChangedValuesIsAnsweredAndNotRemembered(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, reg.credential("mine", "an-old-secret-value-1234"))

	for name, body := range map[string]string{
		"a typed secret":   `{"credentialId":"` + c.ID + `","secret":"` + reg.pass + `"}`,
		"another username": `{"credentialId":"` + c.ID + `","username":"someone-else","secret":"` + reg.pass + `"}`,
	} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if f.storedTest(t, admin, c.ID) != nil {
			t.Errorf("%s: a test of values that are not saved was remembered", name)
		}
	}
}

func TestATestNamesACredentialOfItsOwnLayerAndEnvironmentOnly(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	admin := credentialUser{f.cookie, f.csrf}
	instance := f.storeCredential(t, admin, reg.credential("layer-instance", reg.pass))
	mk := func(env string) credentialResp {
		rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+env+"/registries", reg.credential("layer-"+env, reg.pass))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		return decodeCred(t, rec)
	}
	inA, inB := mk(a.ID), mk(b.ID)

	for name, tc := range map[string]struct{ path, id string }{
		"an environment's credential through the instance family": {"/api/registries/test", inA.ID},
		"the instance's credential through an environment":        {"/api/env/" + a.ID + "/registries/test", instance.ID},
		"another environment's credential":                        {"/api/env/" + a.ID + "/registries/test", inB.ID},
		"an unknown credential":                                   {"/api/registries/test", "nope"},
	} {
		rec := f.credCall(t, admin, http.MethodPost, tc.path, `{"credentialId":"`+tc.id+`"}`)
		if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
			t.Errorf("%s: %d %s, want 404 credential_not_found", name, rec.Code, rec.Body)
		}
	}
	if f.storedTest(t, admin, inA.ID) != nil || f.storedTest(t, admin, instance.ID) != nil {
		t.Error("a refused test was remembered")
	}

	rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+a.ID+"/registries/test", `{"credentialId":"`+inA.ID+`"}`)
	if rec.Code != http.StatusOK || decodeTest(t, rec).SignedIn.State != "passed" {
		t.Fatalf("its own: %d %s", rec.Code, rec.Body)
	}
	if last := f.storedTest(t, admin, inA.ID); last == nil || !last.OK {
		t.Errorf("lastTest = %+v, want a pass", last)
	}
	var envID, layer string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(env_id, ''), details->>'layer' FROM audit_log WHERE action = 'registry.credential_tested' ORDER BY seq DESC LIMIT 1`).Scan(&envID, &layer); err != nil {
		t.Fatal(err)
	}
	if envID != a.ID || layer != "environment" {
		t.Errorf("audit env_id = %q, layer = %q, want %s and environment", envID, layer, a.ID)
	}
}

func TestAnEnvAdminTestsTheirEnvironmentAndAnOperatorCannot(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	reg := newFakeRegistry(t)
	f.trusting(t, reg)
	envAdmin := f.asRoleOn(t, "envadmin", a.ID, "admin")
	operator := f.asRoleOn(t, "operator", a.ID, "operator")

	rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+a.ID+"/registries/test", reg.body(reg.pass, "acme/app"))
	if rec.Code != http.StatusOK || decodeTest(t, rec).CanPull.State != "passed" {
		t.Fatalf("env Admin: %d %s", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, operator, http.MethodPost, "/api/env/"+a.ID+"/registries/test", reg.body(reg.pass, "")); rec.Code != http.StatusForbidden {
		t.Errorf("Operator: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+b.ID+"/registries/test", reg.body(reg.pass, "")); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("Admin of A on B: %d %s, want 403 or 404", rec.Code, rec.Body)
	}
}

func TestBothFamiliesShareOneLimitOfTenTestsAMinutePerUser(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, _ := f.twoEnvironments(t)
	admin := credentialUser{f.cookie, f.csrf}
	body := `{"type":"generic","host":"169.254.169.254","username":"bot","secret":"does-not-matter-1234"}`
	for i := range 10 {
		path := "/api/registries/test"
		if i%2 == 1 {
			path = "/api/env/" + a.ID + "/registries/test"
		}
		if rec := f.credCall(t, admin, http.MethodPost, path, body); rec.Code != http.StatusOK {
			t.Fatalf("test %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/api/registries/test", "/api/env/" + a.ID + "/registries/test"} {
		rec := f.credCall(t, admin, http.MethodPost, path, body)
		if rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
			t.Errorf("%s: %d %s, want 429 rate_limited", path, rec.Code, rec.Body)
		}
	}
	// Another user has a limit of their own.
	other := f.asRoleOn(t, "envadmin", a.ID, "admin")
	if rec := f.credCall(t, other, http.MethodPost, "/api/env/"+a.ID+"/registries/test", body); rec.Code != http.StatusOK {
		t.Errorf("another user: %d %s, want 200", rec.Code, rec.Body)
	}
}

// Self-hosted registries (plan 05-12, D-14): a credential's own CA and the insecure switch.

// caOf is the registry's certificate in PEM, the CA a person would paste for a self-signed one.
func (f *fakeRegistry) caOf() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

// withOptions is the credential body with the self-hosted options added.
func (f *fakeRegistry) withOptions(name, secret string, extra map[string]any) string {
	m := map[string]any{"name": name, "type": "generic", "host": f.host(), "username": f.user, "secret": secret}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestACredentialKeepsItsCAAndSwitchAndOnlyManagersReadTheCA(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	admin := f.asAdmin(t)
	f.addEnvironmentRow(t, "env-a")
	viewer := f.asRoleOn(t, "viewer", "env-a", "viewer")

	c := f.storeCredential(t, admin, reg.withOptions("lab", reg.pass, map[string]any{"caPem": "\n" + reg.caOf() + "\n", "insecure": true}))
	if c.CAPEM != strings.TrimSpace(reg.caOf()) || !c.HasCA || !c.Insecure {
		t.Fatalf("created = %+v, want the trimmed CA, hasCa and insecure for a manager", c)
	}
	rec := f.credCall(t, viewer, http.MethodGet, "/api/registries/"+c.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer get: %d %s", rec.Code, rec.Body)
	}
	seen := decodeCred(t, rec)
	if seen.CAPEM != "" || !seen.HasCA || !seen.Insecure || seen.CanManage {
		t.Errorf("viewer sees %+v, want no CA text but hasCa and insecure", seen)
	}

	_, details := f.registryAudit(t)
	if details[0]["insecure"] != "true" || details[0]["custom_ca"] != "true" {
		t.Errorf("audit details = %v, want insecure and custom_ca recorded", details[0])
	}
	for k, v := range details[0] {
		if strings.Contains(v, "BEGIN CERTIFICATE") {
			t.Errorf("audit detail %s carries the certificate", k)
		}
	}

	// An edit changes them, clears the CA with an empty string, and keeps both when left out.
	rec = f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"caPem":"","insecure":false,"secret":`+jsonString(reg.pass)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got := decodeCred(t, rec); got.HasCA || got.CAPEM != "" || got.Insecure {
		t.Errorf("after clearing = %+v, want no CA and no switch", got)
	}
	actions, details := f.registryAudit(t)
	last := details[len(details)-1]
	if actions[len(actions)-1] != "registry.credential_updated" || last["changed"] != "ca_pem,insecure,secret" {
		t.Errorf("update audit = %v %v, want changed ca_pem,insecure,secret", actions[len(actions)-1], last)
	}
	rec = f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"insecure":true,"caPem":`+jsonString(reg.caOf())+`,"secret":`+jsonString(reg.pass)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"name":"lab2"}`)
	if got := decodeCred(t, rec); !got.HasCA || !got.Insecure || got.Name != "lab2" {
		t.Errorf("a patch that leaves them out changed them: %+v", got)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestACAThatIsTooLargeOrHoldsNoCertificateIsRefusedWithCaInvalid(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	admin := f.asAdmin(t)
	big := reg.caOf() + strings.Repeat("#", 64<<10)
	for name, ca := range map[string]string{"not a certificate": "hello", "too large": big} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries", reg.withOptions("lab", reg.pass, map[string]any{"caPem": ca}))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "body.caPem") || !strings.Contains(rec.Body.String(), "ca_invalid") {
			t.Errorf("create with %s: %d %s, want 422 ca_invalid at body.caPem", name, rec.Code, rec.Body)
		}
	}
	var n int
	if err := f.st.Read.QueryRow(`SELECT count(*) FROM registry_credentials`).Scan(&n); err != nil || n != 0 {
		t.Errorf("credentials = %d, %v; a refused CA stores nothing", n, err)
	}
	c := f.storeCredential(t, admin, reg.credential("lab", reg.pass))
	rec := f.credCall(t, admin, http.MethodPatch, "/api/registries/"+c.ID, `{"caPem":"hello"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ca_invalid") {
		t.Errorf("patch with a bad CA: %d %s, want 422 ca_invalid", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodPost, "/api/registries/test", reg.withOptionsTest(map[string]any{"caPem": "hello"}))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "ca_invalid") {
		t.Errorf("test with a bad CA: %d %s, want 422 ca_invalid", rec.Code, rec.Body)
	}
}

func (f *fakeRegistry) withOptionsTest(extra map[string]any) string {
	m := map[string]any{"type": "generic", "host": f.host(), "username": f.user, "secret": f.pass, "testRepository": "acme/app"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// The server's own default trust does not include the fixture's certificate; the credential's
// CA, or the switch, is what lets the test through.
func TestATestTrustsTheCredentialsCAOrTheSwitchAndNothingElse(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	admin := f.asAdmin(t)

	call := func(extra map[string]any) testResult {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", reg.withOptionsTest(extra))
		if rec.Code != http.StatusOK {
			t.Fatalf("test: %d %s", rec.Code, rec.Body)
		}
		return decodeTest(t, rec)
	}
	if got := call(nil); got.Reachable != (testStep{"failed", "registry_tls"}) {
		t.Errorf("without a CA = %+v, want failed registry_tls", got)
	}
	if got := call(map[string]any{"caPem": reg.caOf()}); got.Reachable.State != "passed" || got.SignedIn.State != "passed" || got.CanPull.State != "passed" {
		t.Errorf("with the CA = %+v, want every step passed", got)
	}
	if got := call(map[string]any{"insecure": true}); got.Reachable.State != "passed" || got.SignedIn.State != "passed" || got.CanPull.State != "passed" {
		t.Errorf("with the switch = %+v, want every step passed", got)
	}
	// The CA of one test does not leak into the next.
	if got := call(nil); got.Reachable != (testStep{"failed", "registry_tls"}) {
		t.Errorf("after a test with the CA, without it = %+v, want failed registry_tls", got)
	}
	_, details := f.registryAudit(t)
	if details[1]["custom_ca"] != "true" || details[2]["insecure"] != "true" || details[0]["insecure"] != "false" {
		t.Errorf("audit details = %v, want custom_ca and insecure recorded per test", details)
	}
}

func TestATestOfAStoredCredentialLendsItsCAAndSwitchUnlessTheTestSendsOthers(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	reg := newFakeRegistry(t)
	admin := f.asAdmin(t)
	c := f.storeCredential(t, admin, reg.withOptions("lab", reg.pass, map[string]any{"caPem": reg.caOf()}))

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", `{"credentialId":"`+c.ID+`","testRepository":"acme/app"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := decodeTest(t, rec); got.Reachable.State != "passed" || got.CanPull.State != "passed" {
		t.Fatalf("with the stored CA = %+v, want passed", got)
	}
	if last := f.storedTest(t, admin, c.ID); last == nil || !last.OK {
		t.Fatalf("lastTest = %+v, want the pass remembered", last)
	}

	// A test that sends another CA is of values that are not saved: answered, not remembered.
	rec = f.credCall(t, admin, http.MethodPost, "/api/registries/test", `{"credentialId":"`+c.ID+`","caPem":"","secret":`+jsonString(reg.pass)+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := decodeTest(t, rec); got.Reachable != (testStep{"failed", "registry_tls"}) {
		t.Fatalf("with the CA cleared = %+v, want failed registry_tls", got)
	}
	if last := f.storedTest(t, admin, c.ID); last == nil || !last.OK {
		t.Errorf("lastTest = %+v, want the earlier pass kept: the changed test was not saved values", last)
	}
}

// Test from an environment (plan 05-13, IMG-01, D-13): a node's engine reads the manifest.

type fromEnvResult struct {
	NodeID     string   `json:"nodeId"`
	Node       string   `json:"node"`
	State      string   `json:"state"`
	Code       string   `json:"code"`
	Digest     string   `json:"digest"`
	Platforms  []string `json:"platforms"`
	Repository string   `json:"repository"`
	Tag        string   `json:"tag"`
	Ref        string   `json:"ref"`
}

func fromEnvBody(envID, repo, tag string) string {
	m := map[string]string{"type": "generic", "host": "localhost:5000", "username": "bot", "secret": pullCredSecret, "repository": repo}
	if envID != "" {
		m["envId"] = envID
	}
	if tag != "" {
		m["tag"] = tag
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func decodeFromEnv(t *testing.T, rec *httptest.ResponseRecorder) fromEnvResult {
	t.Helper()
	var r fromEnvResult
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return r
}

func TestATestFromAnEnvironmentRunsOnOneManagerNamesItAndSendsTheCredentialOnlyThere(t *testing.T) {
	t.Parallel()
	f, env, alpha, bravo := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("test-from-env: %d %s", rec.Code, rec.Body)
	}
	res := decodeFromEnv(t, rec)
	if res.State != "passed" || res.Code != "" || res.Node != "alpha" || res.NodeID != alpha.id ||
		!strings.HasPrefix(res.Digest, "sha256:") || res.Repository != "localhost:5000/private/app" ||
		res.Tag != "latest" || res.Ref != "localhost:5000/private/app:latest" ||
		!slices.Equal(res.Platforms, []string{"linux/amd64", "linux/arm64/v8"}) {
		t.Errorf("result = %+v, want passed on alpha with the digest, the tag and the real platforms", res)
	}
	// One node was asked, once, for the tag that defaults to latest, with the credential.
	if got := bravo.fake.DistributionAuth(); len(got) != 0 {
		t.Errorf("bravo was asked too: %q", got)
	}
	refs, auths := alpha.fake.DistributionRefs(), alpha.fake.DistributionAuth()
	if !slices.Equal(refs, []string{"localhost:5000/private/app:latest"}) || len(auths) != 1 {
		t.Fatalf("alpha saw %q with %d headers, want one check of private/app:latest", refs, len(auths))
	}
	if user, pw, server := decodedPullAuth(t, auths[0]); user != "bot" || pw != pullCredSecret || server != "localhost:5000" {
		t.Errorf("header = %q %q %q, want the credential for localhost:5000", user, pw, server)
	}
	if w := alpha.fake.Writes(); len(w) != 0 {
		t.Errorf("the check wrote %v", w)
	}
	if strings.Contains(rec.Body.String(), pullCredSecret) {
		t.Errorf("the response carries the secret: %s", rec.Body)
	}

	// Audited with the environment, the node and via env, never the secret.
	var envID, via, node, repo, result string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(env_id, ''), details->>'via', details->>'node', details->>'repository', result
		FROM audit_log WHERE action = 'registry.credential_tested'`).Scan(&envID, &via, &node, &repo, &result); err != nil {
		t.Fatal(err)
	}
	if envID != env.ID || via != "env" || node != alpha.id || repo != "localhost:5000/private/app" || result != "success" {
		t.Errorf("audit = %q %q %q %q %q, want the environment, via env, the node, the repository and success", envID, via, node, repo, result)
	}
	var raw string
	if err := f.st.Read.QueryRow(`SELECT COALESCE(details, '') FROM audit_log WHERE action = 'registry.credential_tested'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, pullCredSecret) {
		t.Errorf("the audit row carries the secret: %s", raw)
	}
}

// fromEnvBodyFor is a from-env request for a stored registry fake: its host, user and password.
func fromEnvBodyFor(reg *fakeRegistry, envID, repo, tag string) string {
	m := map[string]string{"type": "generic", "host": reg.host(), "username": reg.user, "secret": reg.pass, "repository": repo, "envId": envID}
	if tag != "" {
		m["tag"] = tag
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestATestFromAnEnvironmentReadsATagTheRepositoryHasWhenNoneIsAsked(t *testing.T) {
	t.Parallel()
	f, env, alpha, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	reg := newFakeRegistry(t)
	reg.tags = []string{"1.36.1"}
	f.trusting(t, reg)

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBodyFor(reg, env.ID, "team/busybox", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("test-from-env: %d %s", rec.Code, rec.Body)
	}
	want := reg.host() + "/team/busybox:1.36.1"
	res := decodeFromEnv(t, rec)
	if res.State != "passed" || res.Tag != "1.36.1" || res.Ref != want || res.Repository != reg.host()+"/team/busybox" {
		t.Errorf("result = %+v, want passed on %s", res, want)
	}
	if refs := alpha.fake.DistributionRefs(); !slices.Equal(refs, []string{want}) {
		t.Errorf("the node was asked for %q, want %q", refs, want)
	}
	if n := reg.tagLists.Load(); n != 1 {
		t.Errorf("tag lists = %d, want one", n)
	}
	var tag string
	if err := f.st.Read.QueryRow(`SELECT details->>'tag' FROM audit_log WHERE action = 'registry.credential_tested'`).Scan(&tag); err != nil || tag != "1.36.1" {
		t.Errorf("audited tag = %q, %v, want the tag that was read", tag, err)
	}
	if strings.Contains(rec.Body.String(), pullCredSecret) || strings.Contains(rec.Body.String(), reg.pass) {
		t.Errorf("the response carries the secret: %s", rec.Body)
	}

	// latest wins when the repository lists it.
	reg.tags = []string{"2.0", "latest"}
	rec = f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBodyFor(reg, env.ID, "team/busybox", ""))
	if res := decodeFromEnv(t, rec); res.Tag != "latest" || res.Ref != reg.host()+"/team/busybox:latest" {
		t.Errorf("result = %+v, want latest when it is listed", res)
	}
}

func TestATestFromAnEnvironmentWithATagListsNothingAndAHostThePolicyRefusesIsNeverListed(t *testing.T) {
	t.Parallel()
	f, env, alpha, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	reg := newFakeRegistry(t)
	reg.tags = []string{"1.36.1"}
	f.trusting(t, reg)

	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBodyFor(reg, env.ID, "team/busybox", "2.0"))
	if res := decodeFromEnv(t, rec); rec.Code != http.StatusOK || res.Tag != "2.0" || res.Ref != reg.host()+"/team/busybox:2.0" {
		t.Errorf("explicit tag: %d %+v, want 2.0", rec.Code, res)
	}
	if refs := alpha.fake.DistributionRefs(); !slices.Equal(refs, []string{reg.host() + "/team/busybox:2.0"}) {
		t.Errorf("the node was asked for %q, want :2.0", refs)
	}
	if n := reg.hits.Load(); n != 0 {
		t.Errorf("the registry was sent %d requests for a check with a tag, want none from the server", n)
	}

	// A host the egress policy refuses reads as unreachable and the server makes no request.
	pool := x509.NewCertPool()
	pool.AddCert(reg.srv.Certificate())
	f.registryEgress = registry.NewEgressPolicy([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}).WithRootCAs(pool)
	f.h = f.build(t)
	rec = f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBodyFor(reg, env.ID, "team/busybox", ""))
	res := decodeFromEnv(t, rec)
	if rec.Code != http.StatusOK || res.State != "failed" || res.Code != "registry_unreachable" || res.Tag != "latest" {
		t.Errorf("refused host: %d %+v, want failed registry_unreachable with latest", rec.Code, res)
	}
	if n := reg.hits.Load(); n != 0 {
		t.Errorf("the registry was sent %d requests for a refused host, want none", n)
	}
}

func TestATestFromAnEnvironmentKeepsLatestWhenTheServerCannotListTheTags(t *testing.T) {
	t.Parallel()
	f, env, alpha, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	reg := newFakeRegistry(t)
	reg.tagsStatus = http.StatusForbidden
	f.trusting(t, reg)
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBodyFor(reg, env.ID, "team/busybox", ""))
	if res := decodeFromEnv(t, rec); rec.Code != http.StatusOK || res.Tag != "latest" || res.Ref != reg.host()+"/team/busybox:latest" {
		t.Errorf("result = %d %+v, want latest", rec.Code, res)
	}
	if refs := alpha.fake.DistributionRefs(); !slices.Equal(refs, []string{reg.host() + "/team/busybox:latest"}) {
		t.Errorf("the node was asked for %q, want :latest", refs)
	}
}

func TestATestFromAnEnvironmentOfASwarmPrefersAManagerOverAnEarlierWorker(t *testing.T) {
	t.Parallel()
	worker := &wire.Facts{EngineID: "ENG:alpha", Hostname: "alpha", EngineReachable: true,
		Swarm: &wire.SwarmFacts{State: "active", NodeID: "node-alpha", ClusterID: "cluster-1"}}
	f, env, w, m := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active"},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		worker, pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", "2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("test-from-env: %d %s", rec.Code, rec.Body)
	}
	if res := decodeFromEnv(t, rec); res.Node != "bravo" || res.NodeID != m.id || res.State != "passed" {
		t.Errorf("result = %+v, want it run on the manager bravo", res)
	}
	if len(w.fake.DistributionAuth()) != 0 || !slices.Equal(m.fake.DistributionRefs(), []string{"localhost:5000/private/app:2"}) {
		t.Errorf("worker saw %q, manager saw %q", w.fake.DistributionRefs(), m.fake.DistributionRefs())
	}
}

func TestATestFromAnEnvironmentNamesTheNodeAndTheCodeWhenItCannotPull(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status dockertest.FakePullStatus
		want   string
	}{
		{"denied", dockertest.FakePullStatus{Code: 403, Message: "Head \"https://localhost:5000/v2/private/app/manifests/latest\": denied"}, "pull_denied"},
		{"unauthorized", dockertest.FakePullStatus{Code: 401, Message: "unauthorized: authentication required"}, "pull_unauthorized"},
		{"missing", dockertest.FakePullStatus{Code: 404, Message: "manifest unknown"}, "image_not_found"},
		{"unreachable", dockertest.FakePullStatus{Code: 500, Message: "lookup localhost: no such host"}, "registry_unreachable"},
		{"certificate", dockertest.FakePullStatus{Code: 500, Message: "x509: certificate signed by unknown authority"}, "registry_tls"},
		{"plain-HTTP retry hid the answer", dockertest.FakePullStatus{Code: 500, Message: "unknown: Client sent an HTTP request to an HTTPS server."}, "registry_http_fallback"},
		{"an agent too old for the route", dockertest.FakePullStatus{Code: 403, Message: "agent_policy_denied: GET /distribution/localhost:5000/private/app:latest/json"}, "agent_policy_denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, env, node, _ := twoNodes(t,
				dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true, PullStatus: tc.status},
				dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
				pairFacts("alpha", false), pairFacts("bravo", false))
			admin := credentialUser{f.cookie, f.csrf}
			rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("test-from-env: %d %s", rec.Code, rec.Body)
			}
			res := decodeFromEnv(t, rec)
			if res.State != "failed" || res.Code != tc.want || res.Node != "alpha" || res.NodeID != node.id || res.Digest != "" {
				t.Errorf("result = %+v, want failed %s on alpha", res, tc.want)
			}
			if strings.Contains(rec.Body.String(), "manifests/latest") || strings.Contains(rec.Body.String(), "x509") ||
				strings.Contains(rec.Body.String(), "HTTPS server") {
				t.Errorf("the answer carries text from the engine: %s", rec.Body)
			}
			var reason, result string
			if err := f.st.Read.QueryRow(`SELECT COALESCE(reason, ''), result FROM audit_log WHERE action = 'registry.credential_tested'`).Scan(&reason, &result); err != nil {
				t.Fatal(err)
			}
			if reason != tc.want || result != "failure" {
				t.Errorf("audit = %q %q, want failure %s", result, reason, tc.want)
			}
		})
	}
}

func TestATestFromAnEnvironmentNeedsARepositoryAndAUsableTag(t *testing.T) {
	t.Parallel()
	f, env, node, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "", "")); rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "repository_required" {
		t.Errorf("no repository: %d %s, want 422 repository_required", rec.Code, rec.Body)
	}
	for _, tag := range []string{"bad tag", "1@sha256:" + strings.Repeat("a", 64), "-x"} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", tag))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "validation_failed" || !strings.Contains(rec.Body.String(), "tag_invalid") {
			t.Errorf("tag %q: %d %s, want 422 tag_invalid", tag, rec.Code, rec.Body)
		}
	}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "Private/App", "")); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "test_repository_invalid") {
		t.Errorf("upper-case repository: %d %s, want 422 test_repository_invalid", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody("no-such-env", "private/app", "")); rec.Code != http.StatusNotFound || problemType(t, rec) != "environment_not_found" {
		t.Errorf("unknown environment: %d %s, want 404 environment_not_found", rec.Code, rec.Body)
	}
	if got := node.fake.DistributionAuth(); len(got) != 0 {
		t.Errorf("a refused request reached the node: %q", got)
	}
}

func TestATestFromAnEnvironmentOfAStoredCredentialUsesItsSecretAndOnlyThatLayers(t *testing.T) {
	t.Parallel()
	f, env, node, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	inst := f.addPullCredential(t, "inst", "")
	envCred := f.addPullCredential(t, "own", env.ID)

	body := func(id string) string {
		return `{"credentialId":"` + id + `","repository":"private/app"}`
	}
	// The instance family tests an instance credential with its stored secret ...
	rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env",
		`{"envId":"`+env.ID+`","credentialId":"`+inst.ID+`","repository":"private/app"}`)
	if rec.Code != http.StatusOK || decodeFromEnv(t, rec).State != "passed" {
		t.Fatalf("instance credential: %d %s", rec.Code, rec.Body)
	}
	if got := node.fake.DistributionAuth(); len(got) != 1 {
		t.Fatalf("headers = %q, want one", got)
	} else if user, pw, _ := decodedPullAuth(t, got[0]); user != "bot" || pw != pullCredSecret {
		t.Errorf("header = %q %q, want the stored credential", user, pw)
	}
	// ... but not an environment's.
	rec = f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env",
		`{"envId":"`+env.ID+`","credentialId":"`+envCred.ID+`","repository":"private/app"}`)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("environment credential through the instance family: %d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
	// The environment family tests the environment's own, and not the instance's.
	rec = f.credCall(t, admin, http.MethodPost, "/api/env/"+env.ID+"/registries/test-from-env", body(envCred.ID))
	if rec.Code != http.StatusOK || decodeFromEnv(t, rec).Node != "alpha" {
		t.Errorf("own credential: %d %s, want it tested on alpha", rec.Code, rec.Body)
	}
	rec = f.credCall(t, admin, http.MethodPost, "/api/env/"+env.ID+"/registries/test-from-env", body(inst.ID))
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "credential_not_found" {
		t.Errorf("instance credential through the environment family: %d %s, want 404 credential_not_found", rec.Code, rec.Body)
	}
	// Nothing is remembered on the credential by a check from a node.
	if c := decodeCred(t, f.credCall(t, admin, http.MethodGet, "/api/registries/"+inst.ID, "")); c.ID == "" {
		t.Errorf("credential read back empty")
	}
	var tested sql.NullInt64
	if err := f.st.Read.QueryRow(`SELECT last_test_at FROM registry_credentials WHERE id = ?`, inst.ID).Scan(&tested); err != nil || tested.Valid {
		t.Errorf("last_test_at = %v, %v, want none: only the server's test on the saved values is remembered", tested, err)
	}
}

func TestATestFromAnEnvironmentNeedsTheCredentialsPermissionOfItsFamily(t *testing.T) {
	t.Parallel()
	f, env, node, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	other := f.srv.NewEnvironment(t, "Other")
	envAdmin := f.asRoleOn(t, "envadmin", env.ID, "admin")
	operator := f.asRoleOn(t, "operator", env.ID, "operator")

	path := "/api/env/" + env.ID + "/registries/test-from-env"
	if rec := f.credCall(t, envAdmin, http.MethodPost, path, fromEnvBody("", "private/app", "")); rec.Code != http.StatusOK || decodeFromEnv(t, rec).State != "passed" {
		t.Errorf("Env Admin on their environment: %d %s, want 200 passed", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, operator, http.MethodPost, path, fromEnvBody("", "private/app", "")); rec.Code != http.StatusForbidden {
		t.Errorf("Operator: %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/env/"+other.ID+"/registries/test-from-env", fromEnvBody("", "private/app", "")); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("Env Admin on another environment: %d %s, want 403 or 404", rec.Code, rec.Body)
	}
	// The instance family is for instance admins: an Env Admin cannot aim it at any environment.
	if rec := f.credCall(t, envAdmin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", "")); rec.Code != http.StatusForbidden {
		t.Errorf("Env Admin through the instance family: %d %s, want 403", rec.Code, rec.Body)
	}
	if got := node.fake.DistributionAuth(); len(got) != 1 {
		t.Errorf("headers = %q, want only the Env Admin's one check", got)
	}
}

func TestATestFromAnEnvironmentSharesTheTestLimitAndAnEnvironmentWithNoNodeIs503(t *testing.T) {
	t.Parallel()
	f, env, _, _ := twoNodes(t,
		dockertest.FakeConfig{Hostname: "alpha", SwarmState: "active", SwarmControl: true},
		dockertest.FakeConfig{Hostname: "bravo", SwarmState: "active", SwarmControl: true},
		pairFacts("alpha", false), pairFacts("bravo", false))
	admin := credentialUser{f.cookie, f.csrf}
	empty := f.srv.NewEnvironment(t, "Empty")
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(empty.ID, "private/app", "")); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("environment with no agent: %d %s, want 503", rec.Code, rec.Body)
	}
	serverTest := `{"type":"generic","host":"169.254.169.254","username":"bot","secret":"does-not-matter-1234"}`
	// The request above counted as the first, so eight server tests and one check make ten.
	for i := range 8 {
		if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test", serverTest); rec.Code != http.StatusOK {
			t.Fatalf("server test %d: %d %s", i+1, rec.Code, rec.Body)
		}
	}
	if rec := f.credCall(t, admin, http.MethodPost, "/api/registries/test-from-env", fromEnvBody(env.ID, "private/app", "")); rec.Code != http.StatusOK {
		t.Fatalf("tenth test: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/api/registries/test-from-env", "/api/env/" + env.ID + "/registries/test-from-env"} {
		body := fromEnvBody(env.ID, "private/app", "")
		if strings.Contains(path, "/env/") {
			body = fromEnvBody("", "private/app", "")
		}
		if rec := f.credCall(t, admin, http.MethodPost, path, body); rec.Code != http.StatusTooManyRequests || problemType(t, rec) != "rate_limited" {
			t.Errorf("%s: %d %s, want 429 rate_limited", path, rec.Code, rec.Body)
		}
	}
}

// Credential alerts (plan 05-21, D-07, T-05-69). The bell shows a credential that expires within
// 7 days, has expired or was last refused by its registry, and only to the people who manage it.

type alertResp struct {
	Kind         string `json:"kind"`
	CredentialID string `json:"credentialId"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Scope        struct {
		Kind    string  `json:"kind"`
		EnvID   *string `json:"envId"`
		EnvName *string `json:"envName"`
	} `json:"scope"`
	Days int    `json:"days"`
	At   string `json:"at"`
}

func (f *authFixture) alertsFor(t *testing.T, u credentialUser) []alertResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodGet, "/api/registries/alerts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("alerts: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Items []alertResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if out.Items == nil {
		t.Errorf("items is null in %s, want an empty list", rec.Body)
	}
	return out.Items
}

func alertNames(items []alertResp) []string {
	names := make([]string, len(items))
	for i, a := range items {
		names[i] = a.Kind + ":" + a.Name
	}
	return names
}

func credWithExpiry(name, host, expires string) string {
	return `{"name":"` + name + `","type":"generic","host":"` + host + `","username":"bot","secret":"` + pullCredSecret + `","expires":"` + expires + `"}`
}

func TestACredentialExpiringInThreeDaysIsInTheInstanceAdminsAlerts(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	// The fixture clock stands at 2026-01-01 00:00 UTC, so the end of the 3rd is 2 days 23:59:59 away.
	c := f.storeCredential(t, admin, credWithExpiry("ghcr-acme", "ghcr.example.org", "2026-01-03"))
	f.storeCredential(t, admin, credWithExpiry("far-away", "far.example.org", "2027-01-03"))

	got := f.alertsFor(t, admin)
	if len(got) != 1 {
		t.Fatalf("alerts = %v, want only the credential that expires soon", alertNames(got))
	}
	a := got[0]
	if a.Kind != "expiring" || a.Days != 3 || a.CredentialID != c.ID || a.Name != "ghcr-acme" || a.Host != "ghcr.example.org" ||
		a.Scope.Kind != "instance" || a.Scope.EnvID != nil || a.At != "2026-01-03T23:59:59Z" {
		t.Errorf("alert = %+v, want expiring in 3 days for the instance credential, at the end of the day", a)
	}
}

func TestExpiredAndRefusedCredentialsAreAlertsToo(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	admin := f.asAdmin(t)
	old := f.storeCredential(t, admin, credWithExpiry("old", "old.example.org", "2026-01-03"))
	refused := f.storeCredential(t, admin, credWithExpiry("refused", "refused.example.org", "2027-01-03"))
	both := f.storeCredential(t, admin, credWithExpiry("both", "both.example.org", "2026-01-03"))
	store := registry.NewStore(f.st, f.v, f.clk.now)
	for _, id := range []string{refused.ID, both.ID} {
		if err := store.RecordFailure(context.Background(), id, "unauthorized"); err != nil {
			t.Fatal(err)
		}
	}
	// A create refuses a day that has gone, and moving the clock would end the admin's session, so
	// the two early expiries are moved back to the end of 2025-12-31 in the table.
	gone, _ := registry.ExpiryEnd("2025-12-31")
	for _, id := range []string{old.ID, both.ID} {
		if _, err := f.st.Write.Exec(`UPDATE registry_credentials SET expires_at = ? WHERE id = ?`, gone.UnixNano(), id); err != nil {
			t.Fatal(err)
		}
	}

	got := f.alertsFor(t, admin)
	want := []string{"expired:both", "expired:old", "auth_failed:refused"}
	if !slices.Equal(alertNames(got), want) {
		t.Errorf("alerts = %v, want %v (most severe first, one item per credential)", alertNames(got), want)
	}

	// The refusal clears when a later pull works, and the alert with it.
	if err := store.ClearFailure(context.Background(), refused.ID); err != nil {
		t.Fatal(err)
	}
	if got := alertNames(f.alertsFor(t, admin)); !slices.Equal(got, []string{"expired:both", "expired:old"}) {
		t.Errorf("after a later success alerts = %v, want only the expired ones", got)
	}
}

func TestAnEnvAdminHearsOnlyAboutTheCredentialsOfTheirEnvironments(t *testing.T) {
	t.Parallel()
	f := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := f.twoEnvironments(t)
	admin := f.asAdmin(t)
	f.storeCredential(t, admin, credWithExpiry("instance-cred", "inst.example.org", "2026-01-03"))
	for envID, name := range map[string]string{a.ID: "cred-a", b.ID: "cred-b"} {
		rec := f.credCall(t, admin, http.MethodPost, "/api/env/"+envID+"/registries", credWithExpiry(name, name+".example.org", "2026-01-05"))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body)
		}
	}

	// The same kind sorts by name.
	if got := alertNames(f.alertsFor(t, admin)); !slices.Equal(got, []string{"expiring:cred-a", "expiring:cred-b", "expiring:instance-cred"}) {
		t.Errorf("instance admin alerts = %v, want all three", got)
	}

	envAdmin := f.asRoleOn(t, "envadmin", a.ID, "admin")
	got := f.alertsFor(t, envAdmin)
	if len(got) != 1 || got[0].Name != "cred-a" || got[0].Scope.Kind != "environment" || got[0].Scope.EnvID == nil || *got[0].Scope.EnvID != a.ID ||
		got[0].Scope.EnvName == nil || *got[0].Scope.EnvName != a.Name {
		t.Errorf("env admin of A alerts = %+v, want only cred-a, with its environment", got)
	}

	for name, u := range map[string]credentialUser{
		"operator":      f.asRoleOn(t, "operator", a.ID, "operator"),
		"viewer":        f.asRoleOn(t, "viewer", a.ID, "viewer"),
		"viewer of all": f.asRoleOn(t, "viewall", "", "viewer"),
	} {
		if got := f.alertsFor(t, u); len(got) != 0 {
			t.Errorf("%s alerts = %v, want none: they cannot fix a credential", name, alertNames(got))
		}
	}
}

func TestCredentialAlertsNeedSignIn(t *testing.T) {
	t.Parallel()
	f := newAuthFixture(t, config.Config{})
	rec := f.call(t, authReq{method: http.MethodGet, path: "/api/registries/alerts"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d %s, want 401", rec.Code, rec.Body)
	}
}
