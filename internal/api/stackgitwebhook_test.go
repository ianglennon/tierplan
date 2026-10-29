// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stackops"
)

// A git stack's webhook (plan 09-22, GIT-05, D-15): created by an Admin of the environment, shown once, and a delivery
// that proves itself does one thing: it asks for a check of the stack. The router, policy, vault, store and access
// service are the real ones; the repository is a real git server.

// hookFix is a deployed git stack, with the channels that say when a check a delivery started is over.
type hookFix struct {
	*gitStackFix
	stackID string
	// ended receives one value per check a delivery started; true: another follows at once.
	ended chan bool
	// started receives one value each time a check is begun or joined.
	started chan int
	// hold, while open, keeps every check a delivery started from beginning; closing it lets them run.
	hold chan struct{}
	once sync.Once
}

func newHookFix(t *testing.T) *hookFix {
	t.Helper()
	h := &hookFix{ended: make(chan bool, 32), started: make(chan int, 32), hold: make(chan struct{})}
	h.gitStackFix = newGitStackFixWith(t, func(f *envFix) {
		f.gitTriggered = func(_ string, _ gitsync.Outcome, _ error, more bool) { h.ended <- more }
		f.gitJoined = func(_ string, waiting int) {
			<-h.hold
			h.started <- waiting
		}
	})
	rec := h.create(t, h.admin, h.body(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var got gitStackAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	h.stackID = got.StackID
	repo := h.stackRepo()
	ctx := context.Background()
	rev, err := repo.NewRevision(ctx, h.stackID, "ada", stackops.OriginDeploy)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if err := repo.SetOutcome(ctx, rev.ID, stackops.OutcomeGood, "", ""); err != nil {
		t.Fatalf("SetOutcome: %v", err)
	}
	return h
}

// release lets the checks that deliveries started begin. It is safe to call twice.
func (h *hookFix) release() { h.once.Do(func() { close(h.hold) }) }

// open lets checks run at once, for a test that does not need to hold them.
func newOpenHookFix(t *testing.T) *hookFix {
	t.Helper()
	h := newHookFix(t)
	h.release()
	return h
}

type webhookAnswer struct {
	URL       string `json:"url"`
	Secret    string `json:"secret"`
	Flavour   string `json:"flavour"`
	Placement string `json:"placement"`
}

// token is the routing token in the address.
func (w webhookAnswer) token() string { return w.URL[strings.LastIndex(w.URL, "/")+1:] }

func (h *hookFix) webhookRoute() string { return h.gitRoute(h.stackID) + "/webhook" }

func (h *hookFix) createWebhook(t *testing.T, u credentialUser, body string) (*httptest.ResponseRecorder, webhookAnswer) {
	t.Helper()
	rec := h.f.credCall(t, u, http.MethodPost, h.webhookRoute(), body)
	var got webhookAnswer
	if rec.Code == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec, got
}

func (h *hookFix) makeWebhook(t *testing.T, flavour, placement string) webhookAnswer {
	t.Helper()
	body := `{"flavour":"` + flavour + `"`
	if placement != "" {
		body += `,"placement":"` + placement + `"`
	}
	rec, got := h.createWebhook(t, h.admin, body+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", rec.Code, rec.Body)
	}
	return got
}

// deliver posts to a webhook address as a git host would: no session, no CSRF token.
func (h *hookFix) deliver(t *testing.T, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return h.deliverFrom(t, "", token, body, headers)
}

func (h *hookFix) deliverFrom(t *testing.T, ip, token, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return h.f.call(t, authReq{method: http.MethodPost, path: "/hooks/git/" + token, body: body, headers: headers, ip: ip})
}

// githubSig is the header a GitHub delivery of body carries.
func githubSig(secret, body string) map[string]string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return map[string]string{"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(m.Sum(nil))}
}

// awaitChecks waits for the checks deliveries started to end: n runs in a row, the last with nothing after it.
func (h *hookFix) awaitChecks(t *testing.T) {
	t.Helper()
	for more := true; more; {
		more = <-h.ended
	}
}

func TestCreateStackGitWebhook(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)

	rec, got := h.createWebhook(t, h.admin, `{"flavour":"github"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if got.Flavour != "github" || got.Placement != "header" {
		t.Errorf("flavour, placement = %q, %q, want github, header", got.Flavour, got.Placement)
	}
	if !strings.HasPrefix(got.URL, "http://example.com/hooks/git/") || len(got.token()) != 43 || len(got.Secret) != 43 {
		t.Errorf("answer = %+v, want an address on the request's origin with a 256 bit token and a 256 bit secret", got)
	}

	// Only the SHA-256 of the token is stored, the secret is sealed, and the webhook is owned by its maker.
	var hash, sealed, last4, flavour, createdBy string
	if err := h.f.st.Read.QueryRow(`SELECT webhook_token_hash, webhook_secret_sealed, webhook_secret_last4, webhook_flavour, webhook_created_by
		FROM stack_git WHERE stack_id = ?`, h.stackID).Scan(&hash, &sealed, &last4, &flavour, &createdBy); err != nil {
		t.Fatal(err)
	}
	if hash != gitsync.TokenHash(got.token()) || len(hash) != 64 {
		t.Errorf("stored token hash = %q, want the SHA-256 of the token", hash)
	}
	if sealed == "" || strings.Contains(sealed, got.Secret) || last4 != got.Secret[len(got.Secret)-4:] || flavour != "github" || createdBy == "" {
		t.Errorf("stored webhook = sealed %q, last4 %q, flavour %q, createdBy %q", sealed, last4, flavour, createdBy)
	}
	var owned int
	if err := h.f.st.Read.QueryRow(`SELECT count(*) FROM owned_resources WHERE kind = ? AND resource_id = ? AND env_id = ? AND owner_id = ? AND state = 'active'`,
		gitsync.KindGitWebhook, h.stackID, h.env, createdBy).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != 1 {
		t.Errorf("owned %s rows = %d, want 1 active, owned by the maker", gitsync.KindGitWebhook, owned)
	}
	for _, col := range []string{got.token(), got.Secret} {
		var n int
		if err := h.f.st.Read.QueryRow(`SELECT (SELECT count(*) FROM stack_git WHERE webhook_secret_sealed LIKE '%'||?||'%' OR webhook_token_hash = ?)`, col, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("the token or the secret is stored in the clear")
		}
	}

	// One webhook per stack.
	if rec, _ := h.createWebhook(t, h.admin, `{"flavour":"gitlab"}`); rec.Code != http.StatusConflict || problemType(t, rec) != "webhook_exists" {
		t.Errorf("second webhook: %d %s, want 409 webhook_exists", rec.Code, rec.Body)
	}
}

func TestCreateStackGitWebhookFlavours(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body             string
		code             int
		placement        string
		secretShown      bool
		wantProblem      string
		storedSecretWant bool
	}{
		"gitlab":                    {`{"flavour":"gitlab"}`, http.StatusCreated, "header", true, "", true},
		"gitea":                     {`{"flavour":"gitea"}`, http.StatusCreated, "header", true, "", true},
		"github, header":            {`{"flavour":"github","placement":"header"}`, http.StatusCreated, "header", true, "", true},
		"generic, the default":      {`{"flavour":"generic"}`, http.StatusCreated, "path", false, "", false},
		"generic, in the address":   {`{"flavour":"generic","placement":"path"}`, http.StatusCreated, "path", false, "", false},
		"generic, a bearer header":  {`{"flavour":"generic","placement":"header"}`, http.StatusCreated, "header", true, "", true},
		"github with the path":      {`{"flavour":"github","placement":"path"}`, http.StatusUnprocessableEntity, "", false, "webhook_placement", false},
		"an unknown flavour":        {`{"flavour":"bitbucket"}`, http.StatusUnprocessableEntity, "", false, "", false},
		"an unknown placement":      {`{"flavour":"generic","placement":"cookie"}`, http.StatusUnprocessableEntity, "", false, "", false},
		"no flavour":                {`{}`, http.StatusUnprocessableEntity, "", false, "", false},
		"gitlab with a path secret": {`{"flavour":"gitlab","placement":"path"}`, http.StatusUnprocessableEntity, "", false, "webhook_placement", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newOpenHookFix(t)
			rec, got := h.createWebhook(t, h.admin, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("create: %d %s, want %d", rec.Code, rec.Body, tc.code)
			}
			if tc.wantProblem != "" && problemType(t, rec) != tc.wantProblem {
				t.Errorf("problem type = %q, want %q", problemType(t, rec), tc.wantProblem)
			}
			if tc.code != http.StatusCreated {
				return
			}
			if got.Placement != tc.placement || (got.Secret != "") != tc.secretShown {
				t.Errorf("answer = placement %q, secret shown %v, want %q, %v", got.Placement, got.Secret != "", tc.placement, tc.secretShown)
			}
			var sealed string
			if err := h.f.st.Read.QueryRow(`SELECT webhook_secret_sealed FROM stack_git WHERE stack_id = ?`, h.stackID).Scan(&sealed); err != nil {
				t.Fatal(err)
			}
			if (sealed != "") != tc.storedSecretWant {
				t.Errorf("sealed secret stored = %v, want %v (with the secret in the address the token is the secret and nothing else is kept)", sealed != "", tc.storedSecretWant)
			}
		})
	}
}

func TestCreateStackGitWebhookPermissions(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	for _, u := range []string{"operator", "viewer"} {
		h.f.addUser(t, u, false)
		h.f.addEnvRole(t, u, h.env, u)
		cookie, csrf := h.f.signIn(t, u, false)
		rec, _ := h.createWebhook(t, credentialUser{cookie, csrf}, `{"flavour":"github"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403 (git.manage is Admin)", u, rec.Code, rec.Body)
		}
	}
	// An Admin of the environment may, and owns the webhook.
	h.f.addUser(t, "envadmin", false)
	h.f.addEnvRole(t, "envadmin", h.env, "admin")
	cookie, csrf := h.f.signIn(t, "envadmin", false)
	rec, _ := h.createWebhook(t, credentialUser{cookie, csrf}, `{"flavour":"github"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("environment admin: %d %s, want 201", rec.Code, rec.Body)
	}
	var owner string
	if err := h.f.st.Read.QueryRow(`SELECT owner_id FROM owned_resources WHERE kind = ? AND resource_id = ?`, gitsync.KindGitWebhook, h.stackID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "id-envadmin" {
		t.Errorf("owner = %q, want the environment admin who made it", owner)
	}
	// No session at all is 401, and a stack that is not from git is 409.
	if rec := h.f.call(t, authReq{method: http.MethodPost, path: h.webhookRoute(), body: `{"flavour":"github"}`}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d, want 401", rec.Code)
	}
}

func TestCreateStackGitWebhookOfAStackThatIsNotGit(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	rec := h.f.credCall(t, h.admin, http.MethodPost, "/api/env/"+h.env+"/stacks", `{"name":"inline","start":"empty"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create an inline stack: %d %s", rec.Code, rec.Body)
	}
	var st struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	rec = h.f.credCall(t, h.admin, http.MethodPost, h.gitRoute(st.ID)+"/webhook", `{"flavour":"github"}`)
	if rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("webhook of an inline stack: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
	rec = h.f.credCall(t, h.admin, http.MethodPost, "/api/env/"+h.env+"/stacks/no-such-stack/git/webhook", `{"flavour":"github"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("webhook of a missing stack: %d %s, want 404", rec.Code, rec.Body)
	}
}

// rotateWebhook rotates the stack's webhook as u.
func (h *hookFix) rotateWebhook(t *testing.T, u credentialUser) (*httptest.ResponseRecorder, webhookAnswer) {
	t.Helper()
	rec := h.f.credCall(t, u, http.MethodPost, h.webhookRoute()+"/rotate", "")
	var got webhookAnswer
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", rec.Body, err)
		}
	}
	return rec, got
}

// auditAndLogDump is every audit row and every log line the server wrote, as one string to look for secrets in.
func (h *hookFix) auditAndLogDump(t *testing.T) string {
	t.Helper()
	rows, err := h.f.st.Read.Query(`SELECT action || ' ' || target_id || ' ' || COALESCE(reason, '') || ' ' || COALESCE(details, '') || ' ' || COALESCE(user_agent, '') FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var dump strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	dump.WriteString(h.f.logs.String())
	return dump.String()
}

// TestWebhookRotate is the tracer: a leaked address is replaced in one step, the new address and secret are shown once,
// the old address is a 404 like an address that never existed, and the new one makes the stack check its repository
// (D-15, T-09-97).
func TestWebhookRotate(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	old := h.makeWebhook(t, "github", "")
	body := `{"ref":"refs/heads/main"}`
	if rec := h.deliver(t, old.token(), body, githubSig(old.Secret, body)); rec.Code != http.StatusAccepted {
		t.Fatalf("delivery to the first address: %d, want 202", rec.Code)
	}
	h.awaitChecks(t)

	rec, fresh := h.rotateWebhook(t, h.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if fresh.Flavour != "github" || fresh.Placement != "header" || len(fresh.token()) != 43 || len(fresh.Secret) != 43 || !strings.HasPrefix(fresh.URL, "http://example.com/hooks/git/") {
		t.Errorf("answer = %+v, want a new address and a new secret for the same github webhook", fresh)
	}
	if fresh.token() == old.token() || fresh.Secret == old.Secret {
		t.Error("the token or the secret did not change")
	}

	// The old address, even signed with the old secret, is the same empty 404 as an address that never existed.
	unknown := h.deliver(t, strings.Repeat("Z", 43), body, githubSig(old.Secret, body))
	oldAddress := h.deliver(t, old.token(), body, githubSig(old.Secret, body))
	if oldAddress.Code != http.StatusNotFound || oldAddress.Body.Len() != 0 || unknown.Code != http.StatusNotFound {
		t.Fatalf("old address: %d %q, want 404 and no body", oldAddress.Code, oldAddress.Body)
	}
	for _, hdr := range []string{"Cache-Control", "Content-Type", "Content-Length"} {
		if a, b := oldAddress.Header().Get(hdr), unknown.Header().Get(hdr); a != b {
			t.Errorf("old address: %s = %q, an unknown token's is %q", hdr, a, b)
		}
	}
	// The new address with the old secret does not pass either.
	if rec := h.deliver(t, fresh.token(), body, githubSig(old.Secret, body)); rec.Code != http.StatusNotFound {
		t.Errorf("new address, old secret: %d, want 404", rec.Code)
	}

	// The new address with the new secret triggers a check that sees a push.
	head := h.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=after the rotation\n"}, "push after the rotation")
	if rec := h.deliver(t, fresh.token(), body, githubSig(fresh.Secret, body)); rec.Code != http.StatusAccepted {
		t.Fatalf("new address, new secret: %d %s, want 202", rec.Code, rec.Body)
	}
	h.awaitChecks(t)
	if got := h.readGit(t, h.admin, h.stackID); got.Status != "behind" || got.HeadSha != head {
		t.Errorf("after the new delivery = %+v, want behind at %s", got, head)
	}

	// Only the SHA-256 of the new token is stored, the new secret is sealed, and the old ones are gone.
	var hash, sealed, last4 string
	if err := h.f.st.Read.QueryRow(`SELECT webhook_token_hash, webhook_secret_sealed, webhook_secret_last4 FROM stack_git WHERE stack_id = ?`, h.stackID).Scan(&hash, &sealed, &last4); err != nil {
		t.Fatal(err)
	}
	if hash != gitsync.TokenHash(fresh.token()) || sealed == "" || strings.Contains(sealed, fresh.Secret) || last4 != fresh.Secret[len(fresh.Secret)-4:] {
		t.Errorf("stored = hash %q, sealed %q, last4 %q, want the new token's hash, a sealed secret and the new last four", hash, sealed, last4)
	}

	// One audit entry, naming the stack and the git host, with none of the secrets in it, in the log or in a log line.
	rotated := h.f.auditRowsOf(t, audit.ActionGitWebhookRotated)
	if len(rotated) != 1 {
		t.Fatalf("git.webhook_rotated rows = %d, want 1", len(rotated))
	}
	if r := rotated[0]; r.TargetType != "stack" || r.TargetID != h.stackID || r.EnvID != h.env || r.Result != "success" || r.Details["stack"] != "web" || r.Details["flavour"] != "github" {
		t.Errorf("audit row = %+v, want the stack, its environment, its name and the git host", r)
	}
	dump := h.auditAndLogDump(t)
	for what, secret := range map[string]string{
		"the new token": fresh.token(), "the new secret": fresh.Secret, "the new address": fresh.URL,
		"the old token": old.token(), "the old secret": old.Secret, "the old address": old.URL,
	} {
		if strings.Contains(dump, secret) {
			t.Errorf("the audit log or the logs hold %s", what)
		}
	}
}

// TestWebhookRotateKeepsTheShape: a generic webhook with the secret in the address has no separate secret before or
// after a rotation; one with a bearer header has one.
func TestWebhookRotateKeepsTheShape(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		placement   string
		secretShown bool
	}{
		"generic in the address": {"path", false},
		"generic bearer header":  {"header", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newOpenHookFix(t)
			old := h.makeWebhook(t, "generic", tc.placement)
			rec, fresh := h.rotateWebhook(t, h.admin)
			if rec.Code != http.StatusOK {
				t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
			}
			if fresh.Flavour != "generic" || fresh.Placement != tc.placement || (fresh.Secret != "") != tc.secretShown {
				t.Fatalf("answer = %+v, want a generic %s webhook with the secret shown = %v", fresh, tc.placement, tc.secretShown)
			}
			var sealed, last4 string
			if err := h.f.st.Read.QueryRow(`SELECT webhook_secret_sealed, webhook_secret_last4 FROM stack_git WHERE stack_id = ?`, h.stackID).Scan(&sealed, &last4); err != nil {
				t.Fatal(err)
			}
			wantLast4 := fresh.token()[len(fresh.token())-4:]
			if tc.secretShown {
				wantLast4 = fresh.Secret[len(fresh.Secret)-4:]
			}
			if (sealed != "") != tc.secretShown || last4 != wantLast4 {
				t.Errorf("stored = sealed %v, last4 %q, want sealed %v and last4 %q", sealed != "", last4, tc.secretShown, wantLast4)
			}
			headers := map[string]string(nil)
			oldHeaders := map[string]string(nil)
			if tc.secretShown {
				headers = map[string]string{"Authorization": "Bearer " + fresh.Secret}
				oldHeaders = map[string]string{"Authorization": "Bearer " + old.Secret}
			}
			if rec := h.deliver(t, old.token(), "{}", oldHeaders); rec.Code != http.StatusNotFound {
				t.Errorf("old address: %d, want 404", rec.Code)
			}
			if rec := h.deliver(t, fresh.token(), "{}", headers); rec.Code != http.StatusAccepted {
				t.Errorf("new address: %d, want 202", rec.Code)
			}
			h.awaitChecks(t)
		})
	}
}
