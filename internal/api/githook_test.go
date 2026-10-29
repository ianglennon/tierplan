// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"net/http"
	"testing"
)

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
