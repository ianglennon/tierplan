// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/access"
	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/auth"
	"github.com/ianglennon/tierplan/internal/gitsync"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// gitHookPath is the address a git host calls on a push. The token in it routes the request to one stack's webhook.
const gitHookPath = "/hooks/git/{token}"

// maxHookBody is the most a delivery may carry. The body is only the input of a signature; nothing in it is read.
const maxHookBody = 5 << 20

// The limits of the receiver: per client address and per webhook, each a token bucket that refills at a steady rate.
const (
	hookIPPerMinute    = 60
	hookTokenPerMinute = 10
	hookLimiterIdle    = 10 * time.Minute
	// hookRejectBurst is how many rejected signatures on one webhook are audited at once; after that one is audited a
	// minute. The address is not a secret for some git hosts, so a rejection is not evidence that the owner did anything.
	hookRejectBurst = 3
)

// maxHookUserAgent bounds the User-Agent kept in an audit row.
const maxHookUserAgent = 200

// The fixed reasons a rejected request is audited with.
const (
	hookRejectSignature = "bad_signature"
)

// GitHookDeps is what the webhook receiver needs. A zero value answers 404 to every request, so the route exists in
// every router (the exempt-route test and the OpenAPI generator build one with no dependencies).
type GitHookDeps struct {
	// Repo finds a webhook by its token and records what it saw.
	Repo stackops.Repo
	// Sync is asked for a check of the stack. Nil makes the receiver answer 404.
	Sync *gitsync.Service
	// Access rechecks the webhook's owner before anything is triggered. Nil makes the receiver answer 404.
	Access *access.Service
	// Audit records an accepted delivery and a rejected signature on a known webhook.
	Audit *audit.Writer
	Log   *slog.Logger
	// Now is the clock of the limiters and of the last delivery; nil is time.Now.
	Now func() time.Time
}

// gitHook is the receiver with its limiters.
type gitHook struct {
	GitHookDeps
	ips    *auth.IPLimiter
	tokens *auth.IPLimiter
	// rejects samples the audit rows of rejected signatures per webhook. It never refuses a request: a rejection that is
	// not audited answers exactly like one that is.
	rejects *auth.IPLimiter
}

// RegisterGitHooks mounts the webhook receiver, POST /hooks/git/{token}, outside the policy: the request proves itself
// with the token in its address and the signature or secret of the webhook it names. It can only ask for a fetch and a
// check of one stack. It decodes nothing it is sent, takes no ref and no commit from it, and deploys nothing (D-15).
func RegisterGitHooks(r *httpx.Router, d GitHookDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	h := &gitHook{
		GitHookDeps: d,
		ips:         auth.NewIPLimiter(time.Minute/hookIPPerMinute, hookIPPerMinute, hookLimiterIdle, d.Now),
		tokens:      auth.NewIPLimiter(time.Minute/hookTokenPerMinute, hookTokenPerMinute, hookLimiterIdle, d.Now),
		rejects:     auth.NewIPLimiter(time.Minute, hookRejectBurst, hookLimiterIdle, d.Now),
	}
	r.Exempt(http.MethodPost, gitHookPath, h.serve)
}

func (h *gitHook) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// answer writes a status and no body. Every refusal goes through it, so a request that names no webhook, one whose
// signature is wrong and one for a paused webhook look the same (T-09-69).
func answer(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}

func (h *gitHook) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.Sync == nil || h.Access == nil || h.Repo.Store == nil {
		answer(w, http.StatusNotFound)
		return
	}
	if !h.ips.Allow(httpx.ClientIP(ctx)) {
		answer(w, http.StatusTooManyRequests)
		return
	}
	if r.ContentLength > maxHookBody {
		answer(w, http.StatusRequestEntityTooLarge)
		return
	}

	// The token is looked up before the body is read, so a request for an address that is no webhook never makes the
	// server hold a body, and nothing below spends a webhook's budget before the sender has proved itself.
	hash := gitsync.TokenHash(chi.URLParam(r, "token"))
	hook, err := h.Repo.GitWebhookByToken(ctx, hash)
	if errors.Is(err, stackops.ErrNoWebhook) {
		answer(w, http.StatusNotFound) // an address that is no webhook: nothing recorded
		return
	}
	if err != nil {
		h.Log.Error("could not read a webhook", "path", redact.WebhookPath(r.URL.Path), "error", err)
		answer(w, http.StatusInternalServerError)
		return
	}
	// A body that cannot be read in full (too long without a length, or cut short) proves nothing, so it answers like
	// every other refusal and leaves no trace.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		answer(w, http.StatusNotFound)
		return
	}

	if !gitsync.VerifyDelivery(gitsync.Flavour(hook.Flavour), gitsync.Placement(hook.Placement), hook.Secret, r.Header, body) {
		// Only the per-address limit bounds these; the audit rows are sampled and the webhook's own state is not touched,
		// so a caller that holds only the address cannot hide a real delivery or use up the real host's budget.
		if h.rejects.Allow(hash) {
			h.audit(ctx, r, hook, audit.ActionGitWebhookRejected, hookRejectSignature)
		}
		answer(w, http.StatusNotFound)
		return
	}
	if !h.tokens.Allow(hash) {
		answer(w, http.StatusTooManyRequests)
		return
	}

	// The webhook acts on its owner's authority: if the owner no longer holds git.manage, the row is paused here and
	// the request does nothing (P3 D-14).
	if _, _, err := h.Access.RunAs(ctx, gitsync.KindGitWebhook, hook.StackID, "system:webhook"); err != nil {
		if !errors.Is(err, access.ErrOwnerLacksPermission) && !errors.Is(err, access.ErrNotFound) {
			h.Log.Error("could not recheck the owner of a webhook", "stack", hook.StackID, "error", err)
		}
		answer(w, http.StatusNotFound)
		return
	}

	h.record(ctx, r, hook, stackops.WebhookAccepted, audit.ActionGitWebhookDelivered)
	h.Sync.Trigger(hook.EnvID, hook.StackID, gitsync.TriggerWebhook)
	answer(w, http.StatusAccepted)
}

// record stores the time and result of an accepted delivery on the webhook and writes the audit row. Neither holds the
// address, the token, the secret or the signature.
func (h *gitHook) record(ctx context.Context, r *http.Request, hook stackops.GitWebhook, result, action string) {
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := h.Repo.RecordGitWebhookDelivery(wctx, hook.StackID, h.now(), result); err != nil {
		h.Log.Error("could not record a webhook delivery", "stack", hook.StackID, "error", err)
	}
	h.auditIn(wctx, r, hook, action, "")
}

// audit writes the audit row of a delivery that is not recorded on the webhook.
func (h *gitHook) audit(ctx context.Context, r *http.Request, hook stackops.GitWebhook, action, reason string) {
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	h.auditIn(wctx, r, hook, action, reason)
}

func (h *gitHook) auditIn(wctx context.Context, r *http.Request, hook stackops.GitWebhook, action, reason string) {
	if h.Audit == nil {
		return
	}
	details := map[string]string{"stack": hook.StackName, "flavour": hook.Flavour}
	entryResult := audit.ResultSuccess
	if reason != "" {
		details["reason"] = reason
		entryResult = audit.ResultFailure
	}
	ua := r.UserAgent()
	if len(ua) > maxHookUserAgent {
		// Cut on a rune boundary: the User-Agent is the sender's, and a split character would store invalid UTF-8.
		ua = strings.ToValidUTF8(ua[:maxHookUserAgent], "")
	}
	actor := rbac.System("system:webhook").AuditActor()
	if err := h.Audit.Record(wctx, audit.Entry{
		Actor: actor, Action: action, TargetType: stackTargetType, TargetID: hook.StackID, EnvID: hook.EnvID,
		Result: entryResult, Reason: reason, IP: httpx.ClientIP(wctx), UserAgent: ua, Details: details,
	}); err != nil {
		h.Log.Error("could not audit a webhook delivery", "stack", hook.StackID, "action", action, "error", err)
	}
}
