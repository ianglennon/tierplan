// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/gitsync"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// Detaching a git stack from its repository (plan 09-31, GIT-02, D-25): it becomes an inline stack you edit, nothing that
// runs changes, and none of its git automation is left. The router, policy, vault, store and access service are the real
// ones; the repository is a real git server.

func (h *hookFix) detachRoute() string { return h.gitRoute(h.stackID) + "/detach" }

func (h *hookFix) countOf(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.f.st.Read.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *hookFix) ownedAutoDeploys(t *testing.T) int {
	t.Helper()
	return h.countOf(t, `SELECT count(*) FROM owned_resources WHERE kind = ? AND resource_id = ?`, gitsync.KindGitAutoDeploy, h.stackID)
}

// editCompose saves the draft of the stack with the compose file's content changed, as the editor would.
func (h *hookFix) editCompose(t *testing.T, c *roleCaller, content string) *httptest.ResponseRecorder {
	t.Helper()
	d := answerOf[draftAnswer](t, c.do(http.MethodGet, "stacks/"+h.stackID+"/draft", ""))
	files := make([]map[string]any, len(d.Files))
	for i, f := range d.Files {
		if f.Path == "compose.yaml" {
			f.Content = content
		}
		files[i] = map[string]any{"path": f.Path, "content": f.Content, "secret": f.Secret}
	}
	vars := make([]map[string]any, len(d.Vars))
	for i, v := range d.Vars {
		vars[i] = map[string]any{"name": v.Name, "value": v.Value, "secret": v.Secret}
	}
	body, err := json.Marshal(map[string]any{"version": d.Version, "files": files, "vars": vars})
	if err != nil {
		t.Fatal(err)
	}
	return c.saveDraft(h.stackID, string(body))
}

func TestDetachRoute(t *testing.T) {
	t.Parallel()
	h := newOpenHookFix(t)
	admin := h.f.caller(t, h.e, "envadmin-1", "admin")
	hook := h.makeWebhook(t, "github", "")
	if rec := h.f.credCall(t, h.admin, http.MethodPut, h.gitRoute(h.stackID)+"/auto-deploy", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("turn on auto-deploy: %d %s", rec.Code, rec.Body)
	}
	h.awaitChecks(t)
	if h.ownedWebhooks(t) != 1 || h.ownedAutoDeploys(t) != 1 {
		t.Fatal("the stack lacks its owned automations before the detach")
	}

	// A git stack's repository files can't be edited; that is what the detach changes.
	if rec := h.editCompose(t, admin, "services:\n  web:\n    image: nginx:1.30\n"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("edit of a git stack: %d %s, want 422 repo_file_read_only", rec.Code, rec.Body)
	}
	repo := h.stackRepo()
	polled, err := repo.PolledStacks(t.Context())
	if err != nil || len(polled) != 1 {
		t.Fatalf("PolledStacks before = %v, %v, want the stack", polled, err)
	}
	revisions := h.countOf(t, `SELECT count(*) FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision'`, h.stackID)
	deploys := h.countOf(t, `SELECT count(*) FROM stack_deploys WHERE stack_id = ?`, h.stackID)
	jobs := h.countOf(t, `SELECT count(*) FROM stack_deploys`)
	writes := len(h.f.fake.Writes())

	rec := h.f.credCall(t, h.admin, http.MethodPost, h.detachRoute(), "")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("detach: %d %q, want 204 and no body", rec.Code, rec.Body)
	}

	// The stack is inline, and nothing of git is left on it.
	if s := admin.stackOf(h.stackID); s.Source != "inline" {
		t.Errorf("source = %q, want inline", s.Source)
	}
	if rec := h.f.credCall(t, h.admin, http.MethodGet, h.gitRoute(h.stackID), ""); rec.Code != http.StatusConflict || problemType(t, rec) != "stack_not_git" {
		t.Errorf("git of a detached stack: %d %s, want 409 stack_not_git", rec.Code, rec.Body)
	}
	body := `{"ref":"refs/heads/main"}`
	unknown := h.deliver(t, strings.Repeat("Y", 43), body, githubSig(hook.Secret, body))
	gone := h.deliver(t, hook.token(), body, githubSig(hook.Secret, body))
	if gone.Code != http.StatusNotFound || gone.Body.Len() != 0 || gone.Header().Get("Cache-Control") != unknown.Header().Get("Cache-Control") {
		t.Errorf("old webhook address: %d %q, want the empty 404 of an unknown token", gone.Code, gone.Body)
	}
	if n := h.countOf(t, `SELECT count(*) FROM stack_git WHERE stack_id = ?`, h.stackID) + h.countOf(t, `SELECT count(*) FROM stack_git_state WHERE stack_id = ?`, h.stackID); n != 0 {
		t.Errorf("git rows left = %d, want 0", n)
	}
	if h.ownedWebhooks(t) != 0 || h.ownedAutoDeploys(t) != 0 {
		t.Errorf("owned rows left: webhook %d, auto-deploy %d, want none", h.ownedWebhooks(t), h.ownedAutoDeploys(t))
	}
	if polled, err := repo.PolledStacks(t.Context()); err != nil || len(polled) != 0 {
		t.Errorf("PolledStacks after = %v, %v, want none", polled, err)
	}

	// Nothing that runs changed: no deploy job, no new revision, nothing written to the engine.
	if n := h.countOf(t, `SELECT count(*) FROM stack_snapshots WHERE stack_id = ? AND kind = 'revision'`, h.stackID); n != revisions {
		t.Errorf("revisions = %d, want %d", n, revisions)
	}
	if n := h.countOf(t, `SELECT count(*) FROM stack_deploys WHERE stack_id = ?`, h.stackID); n != deploys {
		t.Errorf("deploys of the stack = %d, want %d", n, deploys)
	}
	if n := h.countOf(t, `SELECT count(*) FROM stack_deploys`); n != jobs {
		t.Errorf("deploy jobs = %d, want %d: a detach starts none", n, jobs)
	}
	if n := len(h.f.fake.Writes()); n != writes {
		t.Errorf("the detach wrote %d times to the engine, want none", n-writes)
	}

	// The draft is editable files now: no commit, inline files, and a change to the compose file is saved.
	draft, err := repo.Draft(t.Context(), h.stackID)
	if err != nil || draft.Git.Commit != "" || len(draft.Copy.Files) == 0 {
		t.Fatalf("draft = %+v, %v, want files and no commit", draft, err)
	}
	for _, f := range draft.Copy.Files {
		if f.Origin != stacks.OriginInline {
			t.Errorf("file %s origin = %q, want inline", f.Path, f.Origin)
		}
	}
	if rec := h.editCompose(t, admin, "services:\n  web:\n    image: nginx:1.30\n"); rec.Code != http.StatusOK {
		t.Errorf("edit after the detach: %d %s, want 200", rec.Code, rec.Body)
	}

	// Audited with the revision and the commit, and nothing secret.
	rows := h.f.auditRowsOf(t, audit.ActionGitStackDetached)
	if len(rows) != 1 {
		t.Fatalf("git.stack_detached rows = %d, want 1", len(rows))
	}
	if r := rows[0]; r.TargetType != "stack" || r.TargetID != h.stackID || r.EnvID != h.env || r.Result != "success" ||
		r.Details["stack"] != "web" || r.Details["revision"] != "1" || r.Details["commit"] != h.sha {
		t.Errorf("audit row = %+v, want the stack, revision 1 and commit %s", r, h.sha)
	}
	for what, secret := range map[string]string{"the token": hook.token(), "the secret": hook.Secret} {
		if strings.Contains(h.auditAndLogDump(t), secret) {
			t.Errorf("the audit log or the logs hold %s", what)
		}
	}
}
