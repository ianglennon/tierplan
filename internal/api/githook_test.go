// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
)

func gitsyncTokenHash(token string) string { return gitsync.TokenHash(token) }

// The webhook receiver, POST /hooks/git/{token} (plan 09-22, GIT-05, D-15, T-09-66 to T-09-70). Nothing a request says
// is believed: a delivery that proves itself makes the stack check its repository, and that is all.

// snapshots counts the revisions of the stack, which a deploy would add to.
func (h *hookFix) snapshots(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.f.st.Read.QueryRow(`SELECT count(*) FROM stack_snapshots WHERE stack_id = ?`, h.stackID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pendingSnapshots counts the revisions of the stack that have no outcome yet, which a deploy in progress would add to.
func (h *hookFix) pendingSnapshots(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.f.st.Read.QueryRow(`SELECT count(*) FROM stack_snapshots WHERE stack_id = ? AND outcome = 'pending'`, h.stackID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestWebhookTriggersCheck is the tracer: a push to GitHub reaches Bayplan through a signed delivery and the stack shows
// behind, with nothing deployed by the webhook.
func TestWebhookTriggersCheck(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	wh := h.makeWebhook(t, "github", "")
	revisions := h.snapshots(t)
	if got := h.readGit(t, h.admin, h.stackID); got.Status != "unknown" {
		t.Fatalf("before any check the status is %q, want unknown", got.Status)
	}

	head := h.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=pushed\n"}, "push to web")
	body := `{"ref":"refs/heads/main","after":"` + head + `"}`
	rec := h.deliver(t, wh.token(), body, githubSig(wh.Secret, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delivery: %d %s, want 202", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("the 202 carries a body: %q", rec.Body)
	}
	h.awaitChecks(t)

	got := h.readGit(t, h.admin, h.stackID)
	if got.Status != "behind" || got.Behind != 1 || got.HeadSha != head {
		t.Errorf("after the delivery = %+v, want behind by 1 at %s", got, head)
	}
	if n := h.snapshots(t); n != revisions {
		t.Errorf("revisions = %d, want %d: a webhook never deploys", n, revisions)
	}
	var last, result string
	if err := h.f.st.Read.QueryRow(`SELECT COALESCE(webhook_last_at, 0), webhook_last_result FROM stack_git WHERE stack_id = ?`, h.stackID).Scan(&last, &result); err != nil {
		t.Fatal(err)
	}
	if last == "0" || result != "accepted" {
		t.Errorf("last delivery = %q %q, want the time and accepted", last, result)
	}
}

// TestWebhookNoOracle: an unknown token, a bad signature and a paused webhook look the same to the caller.
func TestWebhookNoOracle(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	h.f.addUser(t, "envadmin", false)
	h.f.addEnvRole(t, "envadmin", h.env, "admin")
	cookie, csrf := h.f.signIn(t, "envadmin", false)
	rec, wh := h.createWebhook(t, credentialUser{cookie, csrf}, `{"flavour":"github"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", rec.Code, rec.Body)
	}
	body := `{"zen":"x"}`

	unknown := h.deliver(t, strings.Repeat("A", 43), body, githubSig(wh.Secret, body))
	badSig := h.deliver(t, wh.token(), body, githubSig("another secret", body))
	noSig := h.deliver(t, wh.token(), body, nil)

	// The owner stops being an Admin of the environment: the next delivery pauses the webhook and answers like the rest.
	if _, err := h.f.st.Write.Exec(`DELETE FROM role_assignments WHERE id = ?`, "ra-envadmin-"+h.env); err != nil {
		t.Fatal(err)
	}
	paused := h.deliver(t, wh.token(), body, githubSig(wh.Secret, body))

	for name, got := range map[string]*httptest.ResponseRecorder{"unknown token": unknown, "bad signature": badSig, "no signature": noSig, "paused": paused} {
		if got.Code != http.StatusNotFound || got.Body.Len() != 0 {
			t.Errorf("%s: %d %q, want 404 with an empty body", name, got.Code, got.Body)
		}
		for _, hdr := range []string{"Cache-Control", "Content-Type", "Content-Length"} {
			if a, b := got.Header().Get(hdr), unknown.Header().Get(hdr); a != b {
				t.Errorf("%s: %s = %q, the unknown token's is %q", name, hdr, a, b)
			}
		}
	}
	var state string
	if err := h.f.st.Read.QueryRow(`SELECT state FROM owned_resources WHERE kind = ? AND resource_id = ?`, gitsync.KindGitWebhook, h.stackID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "paused_lacks_permission" {
		t.Errorf("owned state = %q, want paused_lacks_permission once the owner lost the permission", state)
	}
	// None of them started a check.
	if got := h.readGit(t, h.admin, h.stackID); got.Status != "unknown" || got.CheckedAt != "" {
		t.Errorf("state = %+v, want never checked: no refused request may trigger a check", got)
	}
}

// TestWebhookLimits: the body is capped at 5 MiB before any lookup, and deliveries are limited per address and per token.
func TestWebhookLimits(t *testing.T) {
	t.Parallel()
	h := newHookFix(t)
	wh := h.makeWebhook(t, "github", "")

	big := strings.Repeat("x", 6<<20)
	if rec := h.deliver(t, wh.token(), big, githubSig(wh.Secret, big)); rec.Code != http.StatusRequestEntityTooLarge || rec.Body.Len() != 0 {
		t.Errorf("6 MiB body: %d %q, want 413 and no body", rec.Code, rec.Body)
	}
	if rec := h.deliver(t, strings.Repeat("B", 43), big, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("6 MiB body to an unknown token: %d, want 413 (the size is judged before any lookup)", rec.Code)
	}
	var recorded int
	if err := h.f.st.Read.QueryRow(`SELECT count(*) FROM stack_git WHERE webhook_last_at IS NOT NULL`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Error("a request that was too big was recorded as a delivery")
	}
	almost := strings.Repeat("y", 5<<20)
	if rec := h.deliver(t, wh.token(), almost, githubSig(wh.Secret, almost)); rec.Code != http.StatusAccepted {
		t.Errorf("a body of exactly 5 MiB: %d, want 202", rec.Code)
	}

	// Per token: 10 a minute. The 5 MiB delivery above was the first; 9 more pass, the 11th is refused.
	body := `{}`
	for i := 2; i <= 10; i++ {
		if rec := h.deliver(t, wh.token(), body, githubSig(wh.Secret, body)); rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %d: %d, want 202", i, rec.Code)
		}
	}
	if rec := h.deliver(t, wh.token(), body, githubSig(wh.Secret, body)); rec.Code != http.StatusTooManyRequests || rec.Body.Len() != 0 {
		t.Errorf("11th delivery in a minute: %d %q, want 429 and no body", rec.Code, rec.Body)
	}
	h.release()
	h.awaitChecks(t)

	// Per address: 60 a minute, whatever they name. Another address is not affected.
	g := h
	for i := 1; i <= 60; i++ {
		if rec := g.deliverFrom(t, "198.51.100.7", fmt.Sprintf("%043d", i), body, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("request %d from one address: %d, want 404", i, rec.Code)
		}
	}
	if rec := g.deliverFrom(t, "198.51.100.7", strings.Repeat("C", 43), body, nil); rec.Code != http.StatusTooManyRequests {
		t.Errorf("61st request in a minute from one address: %d, want 429", rec.Code)
	}
	if rec := g.deliverFrom(t, "198.51.100.8", strings.Repeat("C", 43), body, nil); rec.Code != http.StatusNotFound {
		t.Errorf("another address: %d, want 404", rec.Code)
	}
	// The window moves on: a minute later the address has its budget back.
	g.f.clk.t = g.f.clk.t.Add(61 * time.Second)
	if rec := g.deliverFrom(t, "198.51.100.7", strings.Repeat("C", 43), body, nil); rec.Code != http.StatusNotFound {
		t.Errorf("a minute later: %d, want 404", rec.Code)
	}
}

// TestWebhookIgnoresPayload: nothing in the body chooses what is checked. A body that names another branch and a made-up
// commit checks the stack's own ref; a body that is not JSON is accepted the same way; a ping only checks.
func TestWebhookIgnoresPayload(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	wh := h.makeWebhook(t, "github", "")
	head := h.repo.Commit(t, map[string]string{"stacks/web/web.env": "A=real\n"}, "the real push")
	revisions, pending := h.snapshots(t), h.pendingSnapshots(t)

	fake := strings.Repeat("ab", 20)
	body := `{"ref":"refs/heads/some-other-branch","after":"` + fake + `","repository":{"clone_url":"https://evil.example/x.git"},"commits":[{"modified":["stacks/web/compose.yaml"]}]}`
	if rec := h.deliver(t, wh.token(), body, githubSig(wh.Secret, body)); rec.Code != http.StatusAccepted {
		t.Fatalf("a payload naming another branch: %d %s, want 202", rec.Code, rec.Body)
	}
	h.awaitChecks(t)
	got := h.readGit(t, h.admin, h.stackID)
	if got.HeadSha != head || got.HeadSha == fake || got.Status != "behind" || got.Ref != "main" {
		t.Errorf("state = %+v, want the head of the stack's own ref %s, never the SHA in the body", got, head)
	}

	// Not JSON at all, with a valid signature.
	junk := "\x00\x01 not json {{{"
	if rec := h.deliver(t, wh.token(), junk, githubSig(wh.Secret, junk)); rec.Code != http.StatusAccepted {
		t.Errorf("a body that is not JSON: %d, want 202", rec.Code)
	}
	h.awaitChecks(t)

	// A ping event: accepted, and it only checks.
	ping := `{"zen":"Keep it logically awesome.","hook_id":1}`
	hdr := githubSig(wh.Secret, ping)
	hdr["X-GitHub-Event"] = "ping"
	if rec := h.deliver(t, wh.token(), ping, hdr); rec.Code != http.StatusAccepted {
		t.Errorf("a ping: %d, want 202", rec.Code)
	}
	h.awaitChecks(t)
	if n := h.snapshots(t); n != revisions {
		t.Errorf("revisions = %d, want %d: nothing a delivery says deploys", n, revisions)
	}
	if n := h.pendingSnapshots(t); n != pending {
		t.Errorf("%d revisions pending, was %d: a webhook started a deploy", n, pending)
	}
}

// TestWebhookEveryFlavourDelivers: each git host's own proof is accepted by its own webhook and by no other.
func TestWebhookEveryFlavourDelivers(t *testing.T) {
	t.Parallel()
	body := `{"event":"push"}`
	for name, tc := range map[string]struct {
		flavour, placement string
		good, bad          func(secret string) map[string]string
	}{
		"gitlab": {"gitlab", "", func(s string) map[string]string { return map[string]string{"X-Gitlab-Token": s} },
			func(string) map[string]string { return map[string]string{"X-Gitlab-Token": "wrong"} }},
		"gitea": {"gitea", "", func(s string) map[string]string {
			m := githubSig(s, body)
			return map[string]string{"X-Gitea-Signature": strings.TrimPrefix(m["X-Hub-Signature-256"], "sha256=")}
		}, func(string) map[string]string { return githubSig("wrong", body) }},
		"generic in the address": {"generic", "path", func(string) map[string]string { return nil }, nil},
		"generic with a header": {"generic", "header", func(s string) map[string]string { return map[string]string{"Authorization": "Bearer " + s} },
			func(string) map[string]string { return map[string]string{"Authorization": "Bearer wrong"} }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newOpenHookFix(t)
			wh := h.makeWebhook(t, tc.flavour, tc.placement)
			if tc.bad != nil {
				if rec := h.deliver(t, wh.token(), body, tc.bad(wh.Secret)); rec.Code != http.StatusNotFound {
					t.Errorf("a wrong proof: %d, want 404", rec.Code)
				}
			}
			if rec := h.deliver(t, wh.token(), body, tc.good(wh.Secret)); rec.Code != http.StatusAccepted {
				t.Fatalf("a right proof: %d %s, want 202", rec.Code, rec.Body)
			}
			h.awaitChecks(t)
			if got := h.readGit(t, h.admin, h.stackID); got.CheckedAt == "" {
				t.Errorf("state = %+v, want the stack checked", got)
			}
		})
	}
}

// TestWebhookAuditAndLogs: an accepted delivery and a bad signature on a known webhook are audited, an unknown token is
// not, and neither the audit rows nor any log line hold the address, the token, the secret or a signature.
func TestWebhookAuditAndLogs(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	wh := h.makeWebhook(t, "github", "")
	body := `{"ref":"refs/heads/main"}`
	good := githubSig(wh.Secret, body)
	bad := githubSig("another secret", body)

	if rec := h.deliver(t, strings.Repeat("D", 43), body, good); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	if rec := h.deliver(t, wh.token(), body, bad); rec.Code != http.StatusNotFound {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	if rec := h.deliver(t, wh.token(), body, good); rec.Code != http.StatusAccepted {
		t.Fatalf("good delivery: %d", rec.Code)
	}
	h.awaitChecks(t)

	delivered := h.f.auditRowsOf(t, audit.ActionGitWebhookDelivered)
	rejected := h.f.auditRowsOf(t, audit.ActionGitWebhookRejected)
	if len(delivered) != 1 || len(rejected) != 1 {
		t.Fatalf("audit rows: %d delivered, %d rejected, want 1 and 1 (an unknown token writes nothing)", len(delivered), len(rejected))
	}
	for _, r := range append(delivered, rejected...) {
		if r.TargetType != "stack" || r.TargetID != h.stackID || r.EnvID != h.env || r.Details["stack"] != "web" || r.Details["flavour"] != "github" {
			t.Errorf("audit row = %+v, want the stack, its environment, its name and the flavour", r)
		}
	}
	if rejected[0].Result != "failure" || rejected[0].Details["reason"] != "bad_signature" || delivered[0].Result != "success" {
		t.Errorf("results = %q (%v) and %q, want failure bad_signature and success", rejected[0].Result, rejected[0].Details, delivered[0].Result)
	}
	if got := h.f.audits(t, audit.ActionGitWebhookDelivered)[0]; got.ActorType != "system" || got.ActorID != "system:webhook" {
		t.Errorf("actor = %s %s, want system system:webhook", got.ActorType, got.ActorID)
	}

	// The whole audit log, the logs and every answer are free of what must stay secret.
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
	dump.WriteString(h.f.logs.String())
	for what, secret := range map[string]string{
		"the token": wh.token(), "the secret": wh.Secret, "the address": wh.URL, "the signature": good["X-Hub-Signature-256"],
		"the wrong signature": bad["X-Hub-Signature-256"], "the token hash": gitsyncTokenHash(wh.token()),
	} {
		if strings.Contains(dump.String(), secret) {
			t.Errorf("the audit log or the logs hold %s", what)
		}
	}
	// After the show-once answer nothing returns them: the git route is free of them too.
	rec := h.f.credCall(t, h.admin, http.MethodGet, h.gitRoute(h.stackID), "")
	for what, secret := range map[string]string{"the token": wh.token(), "the secret": wh.Secret} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("the git summary holds %s", what)
		}
	}
}
