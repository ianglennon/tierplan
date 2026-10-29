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
