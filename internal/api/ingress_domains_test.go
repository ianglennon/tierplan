// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/internal/acme/dns"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/ingress/ingressstore"
)

// The ingress domain routes (Phase 18, DOM-01 to DOM-04). The fixture's fake DNS provider holds the zone example.com and
// accepts two tokens; any other token is refused as dns_token_invalid.
const (
	ingressToken     = "cfTok-7Hq2mZ9xW4vB1nR8sKd6LpY3eA5cU0jT" //nolint:gosec // G101: a recognisable fake token, so the scans can look for it
	ingressNextToken = "cfTok-Nx3uQ6wE1yR9tB5kD2mH8vL4pZ7sC0aJ" //nolint:gosec // G101: a recognisable fake token, so the scans can look for it
	ingressWrongTok  = "cfTok-refused-by-the-provider-0000000"  //nolint:gosec // G101: a recognisable fake token, so the scans can look for it
)

// ingressFix is a fixture with two environments and an Admin of each, an Operator of the first and the instance admin.
type ingressFix struct {
	*envFix
	envA, envB                       string
	admin, adminA, adminB, operatorA credentialUser
}

func newIngressFix(t *testing.T) *ingressFix {
	t.Helper()
	ef := newEnvFix(t, dockertest.FakeConfig{Hostname: "node-1"})
	a, b := ef.twoEnvironments(t)
	return &ingressFix{
		envFix:    ef,
		envA:      a.ID,
		envB:      b.ID,
		admin:     ef.asAdmin(t),
		adminA:    ef.asRoleOn(t, "ing-admin-a", a.ID, "admin"),
		adminB:    ef.asRoleOn(t, "ing-admin-b", b.ID, "admin"),
		operatorA: ef.asRoleOn(t, "ing-operator-a", a.ID, "operator"),
	}
}

// ingressPath is a route of one environment's ingress domains.
func ingressPath(env string, rest ...string) string {
	return "/api/env/" + env + "/ingress/domains" + strings.Join(rest, "")
}

// ingressDomainResp is a domain as the API answers it.
type ingressDomainResp struct {
	ID             string     `json:"id"`
	Suffix         string     `json:"suffix"`
	Issuer         string     `json:"issuer"`
	DNSProvider    string     `json:"dnsProvider"`
	TokenSet       bool       `json:"tokenSet"`
	TokenUpdatedAt *time.Time `json:"tokenUpdatedAt"`
	CreatedBy      string     `json:"createdBy"`
	Reserved       []string   `json:"reserved"`
}

func decodeIngressDomain(t *testing.T, rec *httptest.ResponseRecorder) ingressDomainResp {
	t.Helper()
	var d ingressDomainResp
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return d
}

func ingressCreateBody(suffix, issuer, token string) string {
	m := map[string]string{"suffix": suffix}
	if issuer != "" {
		m["issuer"] = issuer
	}
	if token != "" {
		m["token"] = token
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// register registers a domain as u and requires 201.
func (f *ingressFix) register(t *testing.T, u credentialUser, env, suffix string) ingressDomainResp {
	t.Helper()
	rec := f.credCall(t, u, http.MethodPost, ingressPath(env), ingressCreateBody(suffix, "", ingressToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s in %s: %d %s", suffix, env, rec.Code, rec.Body)
	}
	return decodeIngressDomain(t, rec)
}

// ingressAuditRow is one audit row of the ingress domain routes.
type ingressAuditRow struct {
	Action, ActorID, TargetType, TargetID, Result, Reason, EnvID string
	Details                                                      map[string]string
}

// ingressAudit is every audit row of an action, oldest first.
func (f *authFixture) ingressAudit(t *testing.T, action string) []ingressAuditRow {
	t.Helper()
	rows, err := f.st.Read.QueryContext(context.Background(),
		`SELECT action, actor_id, target_type, target_id, result, reason, COALESCE(env_id, ''), details FROM audit_log WHERE action = ? ORDER BY seq`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []ingressAuditRow
	for rows.Next() {
		var r ingressAuditRow
		var raw string
		if err := rows.Scan(&r.Action, &r.ActorID, &r.TargetType, &r.TargetID, &r.Result, &r.Reason, &r.EnvID, &raw); err != nil {
			t.Fatal(err)
		}
		r.Details = map[string]string{}
		if err := json.Unmarshal([]byte(raw), &r.Details); err != nil {
			t.Fatalf("details of %s: %v: %s", action, err, raw)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// everyAuditText is every text column of every audit row, joined, for a scan.
func (f *authFixture) everyAuditText(t *testing.T) string {
	t.Helper()
	rows, err := f.st.Read.QueryContext(context.Background(),
		`SELECT actor_id || '|' || actor_name || '|' || action || '|' || target_type || '|' || target_id || '|' || result || '|' || reason || '|' || ip || '|' || user_agent || '|' || details FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestIngressDomainCreateRegistersWithProductionIssuerAndNeverReturnsTheToken(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", ingressToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s, want 201", rec.Code, rec.Body)
	}
	d := decodeIngressDomain(t, rec)
	if d.ID == "" || d.Suffix != "apps.example.com" || d.Issuer != "production" || d.DNSProvider != "cloudflare" || !d.TokenSet ||
		d.TokenUpdatedAt == nil || d.TokenUpdatedAt.IsZero() || d.CreatedBy != "id-ing-admin-a" || len(d.Reserved) != 0 {
		t.Errorf("domain = %+v", d)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if strings.Contains(rec.Body.String(), ingressToken) {
		t.Errorf("the answer holds the token: %s", rec.Body)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &keys)
	for k := range keys {
		if strings.Contains(strings.ToLower(k), "token") && k != "tokenSet" && k != "tokenUpdatedAt" {
			t.Errorf("unexpected token-like field %q in the answer", k)
		}
	}

	rows := f.ingressAudit(t, audit.ActionIngressDomainCreated)
	if len(rows) != 1 {
		t.Fatalf("ingress.domain_created rows = %+v, want one", rows)
	}
	r := rows[0]
	if r.Result != "success" || r.ActorID != "id-ing-admin-a" || r.TargetType != "ingress_domain" || r.TargetID != d.ID || r.EnvID != f.envA ||
		r.Details["suffix"] != "apps.example.com" || r.Details["issuer"] != "production" || r.Details["provider"] != "cloudflare" || r.Details["credential"] != "set" {
		t.Errorf("audit row = %+v", r)
	}
	if got, err := f.ingressStore.DomainToken(context.Background(), f.envA, d.ID); err != nil || string(got) != ingressToken {
		t.Errorf("the stored token does not open to the typed one: %v", err)
	}
}

func TestIngressDomainCreateStoresStagingAndRefusesAnUnknownIssuer(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("staging.example.com", "staging", ingressToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register staging: %d %s", rec.Code, rec.Body)
	}
	if d := decodeIngressDomain(t, rec); d.Issuer != "staging" {
		t.Errorf("issuer = %q, want staging", d.Issuer)
	}
	rec = f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("other.example.com", "letsencrypt-prod", ingressToken))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("register with an unknown issuer: %d %s, want 422", rec.Code, rec.Body)
	}
	views, err := f.ingressStore.Domains(context.Background(), f.envA)
	if err != nil || len(views) != 1 {
		t.Fatalf("domains = %+v, %v, want only the staging one", views, err)
	}
}

func TestIngressDomainCreateRefusesAPublicSuffixBeforeAnyDNSCall(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	for _, suffix := range []string{"co.uk", "github.io"} {
		rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody(suffix, "", ingressToken))
		if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != ingress.CodeDomainPublicSuffix {
			t.Errorf("%s: %d %s, want 422 ingress_domain_public_suffix", suffix, rec.Code, rec.Body)
		}
	}
	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("under_score.example.com", "", ingressToken))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != ingress.CodeDomainInvalid {
		t.Errorf("invalid suffix: %d %s, want 422 ingress_domain_invalid", rec.Code, rec.Body)
	}
	if n := len(f.ingressProv.Calls()); n != 0 {
		t.Errorf("the DNS provider saw %d calls, want none: the suffix is refused first", n)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainCreated); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none for a suffix refused before the token test", rows)
	}
}

func TestIngressDomainCreateAnswersTheClassOfAFailedTokenTestAndStoresNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(f *ingressFix)
		token  string
		status int
		class  string
	}{
		{"a token the provider refuses", func(*ingressFix) {}, ingressWrongTok, http.StatusUnprocessableEntity, dns.ClassTokenInvalid},
		{"a zone the token cannot see", func(f *ingressFix) {
			f.ingressProv.AddZone("other.org")
			f.ingressProv.HideZone("example.com")
		}, ingressToken, http.StatusUnprocessableEntity, dns.ClassZoneNotFound},
		{"a token that sees no zone at all", func(f *ingressFix) { f.ingressProv.HideZone("example.com") }, ingressToken, http.StatusUnprocessableEntity, dns.ClassTokenScope},
		{"a provider that is slowing us down", func(f *ingressFix) {
			f.ingressProv.FailZone(&dns.Error{Class: dns.ClassRateLimited, Provider: "fake", Op: "find zone"})
		}, ingressToken, http.StatusServiceUnavailable, dns.ClassRateLimited},
		{"a provider that does not answer", func(f *ingressFix) {
			f.ingressProv.FailZone(&dns.Error{Class: dns.ClassUnavailable, Provider: "fake", Op: "find zone"})
		}, ingressToken, http.StatusServiceUnavailable, dns.ClassUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newIngressFix(t)
			c.setup(f)

			rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", c.token))
			if rec.Code != c.status || problemType(t, rec) != c.class {
				t.Fatalf("register: %d %s, want %d %s", rec.Code, rec.Body, c.status, c.class)
			}
			if strings.Contains(rec.Body.String(), c.token) {
				t.Errorf("the problem holds the token: %s", rec.Body)
			}
			views, err := f.ingressStore.Domains(context.Background(), f.envA)
			if err != nil || len(views) != 0 {
				t.Errorf("domains = %+v, %v, want none stored", views, err)
			}
			rows := f.ingressAudit(t, audit.ActionIngressDomainCreated)
			if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != c.class || rows[0].TargetID != "" || rows[0].Details["suffix"] != "apps.example.com" {
				t.Errorf("audit rows = %+v, want one failure row with reason %s", rows, c.class)
			}
		})
	}
}

func TestIngressDomainCreateBoundsTheTokenWithoutEchoingIt(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), `{"suffix":"apps.example.com"}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_token_missing" {
		t.Errorf("no token: %d %s, want 422 ingress_token_missing", rec.Code, rec.Body)
	}
	rec = f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", "   "))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_token_missing" {
		t.Errorf("blank token: %d %s, want 422 ingress_token_missing", rec.Code, rec.Body)
	}
	long := strings.Repeat("Z", 5000)
	rec = f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", long))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_token_too_long" {
		t.Errorf("5000-byte token: %d, want 422 ingress_token_too_long", rec.Code)
	}
	if strings.Contains(rec.Body.String(), long[:64]) {
		t.Errorf("the problem echoes the token")
	}
	// A token with a control character cannot be a token and never reaches the provider.
	rec = f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", "abc\ndef"))
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != dns.ClassTokenInvalid {
		t.Errorf("control character: %d %s, want 422 dns_token_invalid", rec.Code, rec.Body)
	}
	if n := len(f.ingressProv.Calls()); n != 0 {
		t.Errorf("the DNS provider saw %d calls for tokens that were refused locally, want none", n)
	}
}

func TestIngressDomainCreateTwiceIsAConflictAndAnotherEnvironmentMayRegisterTheSuffix(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	first := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("Apps.Example.com.", "staging", ingressToken))
	if rec.Code != http.StatusConflict || problemType(t, rec) != "ingress_domain_exists" {
		t.Fatalf("second registration: %d %s, want 409 ingress_domain_exists", rec.Code, rec.Body)
	}
	views, err := f.ingressStore.Domains(context.Background(), f.envA)
	if err != nil || len(views) != 1 || views[0].ID != first.ID || string(views[0].Issuer) != "production" {
		t.Errorf("domains = %+v, %v, want the first row unchanged", views, err)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainCreated); len(rows) != 1 {
		t.Errorf("created rows = %d, want one: the refused registration leaves none", len(rows))
	}

	other := f.register(t, f.adminB, f.envB, "apps.example.com")
	if other.ID == first.ID {
		t.Errorf("the second environment's domain has the first one's id")
	}
}

func TestIngressDomainListShowsReservedNamesAndTokenSetNeverTheToken(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	f.register(t, f.adminB, f.envB, "other.example.com")
	if _, _, err := f.ingressStore.Reserve(context.Background(), ingressstore.ReserveInput{EnvID: f.envA, DomainID: d.ID, Hostname: "admin.apps.example.com", Actor: "x"}, nil); err != nil {
		t.Fatal(err)
	}

	rec := f.credCall(t, f.adminA, http.MethodGet, ingressPath(f.envA), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Items []ingressDomainResp `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].ID != d.ID || !list.Items[0].TokenSet || len(list.Items[0].Reserved) != 1 || list.Items[0].Reserved[0] != "admin.apps.example.com" {
		t.Errorf("list = %+v, want only this environment's domain with its reserved name", list.Items)
	}
	if strings.Contains(rec.Body.String(), ingressToken) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the list holds the token or is cacheable: %s", rec.Body)
	}
	empty := f.credCall(t, f.admin, http.MethodGet, ingressPath(f.envB), "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), "other.example.com") {
		t.Errorf("the instance admin's list of env B: %d %s", empty.Code, empty.Body)
	}
}

func TestIngressDomainTokenReplaceKeepsTheDomainAndItsClaims(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	if _, err := f.st.Write.Exec(`INSERT INTO stacks (id, env_id, name, target, created_by, created_at) VALUES ('stack-a1', ?, 'web', 'swarm', 'tester', 1)`, f.envA); err != nil {
		t.Fatal(err)
	}
	decl := ingress.Declaration{Service: "web", Host: "web.apps.example.com", Label: "tierplan.ingress.host", Port: 80}
	if _, err := f.ingressStore.ReplaceStackRoutes(context.Background(), ingressstore.ReplaceInput{EnvID: f.envA, StackID: "stack-a1", Actor: "x", Decls: []ingress.Declaration{decl}}); err != nil {
		t.Fatal(err)
	}
	f.clk.t = f.clk.t.Add(time.Hour)

	rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressNextToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace: %d %s, want 200", rec.Code, rec.Body)
	}
	got := decodeIngressDomain(t, rec)
	if got.ID != d.ID || got.Suffix != d.Suffix || got.Issuer != d.Issuer || !got.TokenSet || got.TokenUpdatedAt == nil || !got.TokenUpdatedAt.After(*d.TokenUpdatedAt) {
		t.Errorf("after = %+v, before = %+v: want the same domain with a later tokenUpdatedAt", got, d)
	}
	if strings.Contains(rec.Body.String(), ingressNextToken) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the answer holds the token or is cacheable: %s", rec.Body)
	}
	tok, err := f.ingressStore.DomainToken(context.Background(), f.envA, d.ID)
	if err != nil || string(tok) != ingressNextToken {
		t.Errorf("the saved token is not the new one: %v", err)
	}
	if world, err := f.ingressStore.World(context.Background(), f.envA, nil); err != nil || len(world.Claims) != 1 {
		t.Errorf("claims after the replacement = %+v, %v, want the one claim kept", world.Claims, err)
	}
	rows := f.ingressAudit(t, audit.ActionIngressDomainCredentialReplaced)
	if len(rows) != 1 || rows[0].Result != "success" || rows[0].TargetID != d.ID || rows[0].Details["credential"] != "replaced" || rows[0].Details["suffix"] != "apps.example.com" {
		t.Errorf("audit rows = %+v, want one ingress.domain_credential_replaced row", rows)
	}
}

func TestIngressDomainTokenReplaceWithARefusedTokenKeepsTheOldOne(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressWrongTok+`"}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != dns.ClassTokenInvalid {
		t.Fatalf("replace with a refused token: %d %s, want 422 dns_token_invalid", rec.Code, rec.Body)
	}
	tok, err := f.ingressStore.DomainToken(context.Background(), f.envA, d.ID)
	if err != nil || string(tok) != ingressToken {
		t.Errorf("the saved token changed after a refused replacement: %v", err)
	}
	rows := f.ingressAudit(t, audit.ActionIngressDomainCredentialReplaced)
	if len(rows) != 1 || rows[0].Result != "failure" || rows[0].Reason != dns.ClassTokenInvalid {
		t.Errorf("audit rows = %+v, want one failure row with reason dns_token_invalid", rows)
	}
	rec = f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{}`)
	if rec.Code != http.StatusUnprocessableEntity || problemType(t, rec) != "ingress_token_missing" {
		t.Errorf("replace with no token: %d %s, want 422 ingress_token_missing", rec.Code, rec.Body)
	}
}

func TestIngressDomainTokenNeverAppearsInAResponseTheAuditLogOrTheLogs(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")
	bodies := []*httptest.ResponseRecorder{
		f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressNextToken+`"}`),
		f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"`+ingressWrongTok+`"}`),
		f.credCall(t, f.adminA, http.MethodGet, ingressPath(f.envA), ""),
		f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("apps.example.com", "", ingressToken)),
		f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("co.uk", "", ingressToken)),
		f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("x.example.com", "", ingressWrongTok)),
	}
	f.ingressProv.FailZone(&dns.Error{Class: dns.ClassUnavailable, Provider: "fake", Op: "find zone", Err: context.DeadlineExceeded})
	bodies = append(bodies, f.credCall(t, f.adminA, http.MethodPost, ingressPath(f.envA), ingressCreateBody("y.example.com", "", ingressNextToken)))
	for i, rec := range bodies {
		for _, tok := range []string{ingressToken, ingressNextToken, ingressWrongTok} {
			if strings.Contains(rec.Body.String(), tok) {
				t.Errorf("response %d (%d) holds a token: %s", i, rec.Code, rec.Body)
			}
		}
	}
	text := f.everyAuditText(t)
	logs := f.logs.String()
	for _, tok := range []string{ingressToken, ingressNextToken, ingressWrongTok} {
		if strings.Contains(text, tok) {
			t.Errorf("the audit log holds a token")
		}
		if strings.Contains(logs, tok) {
			t.Errorf("the server log holds a token")
		}
	}
}

func TestIngressDomainCreateNeedsTheAdminOfTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	d := f.register(t, f.adminA, f.envA, "apps.example.com")

	for name, u := range map[string]credentialUser{"an operator": f.operatorA, "an admin of another environment": f.adminB} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, ingressPath(f.envA), ""},
			{http.MethodPost, ingressPath(f.envA), ingressCreateBody("new.example.com", "", ingressToken)},
			{http.MethodPut, ingressPath(f.envA, "/", d.ID, "/token"), `{"token":"` + ingressNextToken + `"}`},
		} {
			rec := f.credCall(t, u, c.method, c.path, c.body)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s %s = %d %s, want 403 or 404", name, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
	if views, err := f.ingressStore.Domains(context.Background(), f.envA); err != nil || len(views) != 1 {
		t.Errorf("domains = %+v, %v, want the one registered by the admin", views, err)
	}
	if rec := f.credCall(t, f.operatorA, http.MethodGet, ingressPath(f.envA), ""); rec.Code != http.StatusForbidden {
		t.Errorf("operator list: %d, want 403", rec.Code)
	}
}

func TestIngressDomainTokenOfAnotherEnvironmentsDomainIsNotFound(t *testing.T) {
	t.Parallel()
	f := newIngressFix(t)
	inB := f.register(t, f.adminB, f.envB, "apps.example.com")

	rec := f.credCall(t, f.adminA, http.MethodPut, ingressPath(f.envA, "/", inB.ID, "/token"), `{"token":"`+ingressNextToken+`"}`)
	if rec.Code != http.StatusNotFound || problemType(t, rec) != "ingress_domain_not_found" {
		t.Fatalf("replace under env A with env B's domain id: %d %s, want 404 ingress_domain_not_found", rec.Code, rec.Body)
	}
	if n := len(f.ingressProv.Calls()); n != 1 {
		t.Errorf("the DNS provider saw %d calls, want only the one of env B's own registration", n)
	}
	tok, err := f.ingressStore.DomainToken(context.Background(), f.envB, inB.ID)
	if err != nil || string(tok) != ingressToken {
		t.Errorf("env B's token changed: %v", err)
	}
	if rows := f.ingressAudit(t, audit.ActionIngressDomainCredentialReplaced); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want none", rows)
	}
}
