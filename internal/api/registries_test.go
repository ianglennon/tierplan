// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/config"
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
