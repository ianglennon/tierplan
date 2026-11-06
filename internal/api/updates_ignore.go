// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Telling TierPlan to stop offering an update (plan 12-13, IMG-08, D-15, D-16, D-10). A skip passes over the one
// version a workload is offered and lifts itself when the tag moves again; an ignore hides every update of an
// image in this environment or of one workload. Both hide future updates, so setting or removing one needs
// updates.manage (an Env Admin). The digest a skip passes over is read from the stored check on the server and
// never taken from the request.

const (
	problemNothingToSkip       = "nothing_to_skip"
	problemIgnoredByLabel      = "ignored_by_label"
	problemSkipNotFound        = "skip_not_found"
	problemIgnoreNotFound      = "ignore_not_found"
	problemInvalidIgnoreTarget = "invalid_ignore_target"
	problemInvalidImage        = "invalid_image"
)

// UpdateIgnoreBody says why a workload's update is skipped or ignored, and by whom.
type UpdateIgnoreBody struct {
	Mode   string `json:"mode" enum:"skip,ignore" doc:"skip passes over one version; ignore hides every update until it is removed"`
	Scope  string `json:"scope" enum:"image,workload,label" doc:"What the choice covers: image (this image in this environment, every service and container using it here), workload (this service or container alone) or label (the tierplan.updates=ignore label on it, which only its owner can remove). A skip is always workload"`
	SetBy  string `json:"setBy" doc:"The display name of the person who chose it; empty for the label, or when that person is gone"`
	SetAt  string `json:"setAt" format:"date-time" doc:"When it was chosen; empty for the label"`
	Digest string `json:"digest" doc:"For a skip, the version it passes over; empty for an ignore"`
}

// updateIgnoreOf turns the skip or ignore a verdict rests on into its answer.
func updateIgnoreOf(ig *updates.Ignore) *UpdateIgnoreBody {
	if ig == nil {
		return nil
	}
	return &UpdateIgnoreBody{Mode: ig.Mode, Scope: ig.Scope, SetBy: ig.SetByName, SetAt: rfc3339(ig.SetAt), Digest: ig.Digest}
}

// UpdateSkipRequest names the workload whose offered version is skipped, or whose skip is taken back.
type UpdateSkipRequest struct {
	Kind string `json:"kind" enum:"service,container" doc:"service for a swarm service, container for a container"`
	Name string `json:"name" minLength:"1" maxLength:"255" doc:"The service's or container's name"`
	Bulk string `json:"bulk,omitempty" enum:"true" doc:"Set to true when this is one of several workloads chosen together; it is only noted in the audit log"`
}

// UpdateSkipInput is the path and body of the skip routes.
type UpdateSkipInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      UpdateSkipRequest
}

// UpdateIgnoreRequest names what is ignored or taken back: an image in this environment, or one workload.
type UpdateIgnoreRequest struct {
	Scope string `json:"scope" enum:"image,workload" doc:"image: the image and tag named by image, every service and container using it in this environment. workload: the service or container named by kind and name"`
	Kind  string `json:"kind,omitempty" enum:"service,container" doc:"For scope workload: service for a swarm service, container for a container"`
	Name  string `json:"name,omitempty" maxLength:"255" doc:"For scope workload: the service's or container's name"`
	Image string `json:"image,omitempty" maxLength:"512" doc:"For scope image: an image reference with a tag, such as nginx:1.27. The registry, repository and tag are matched exactly"`
	Bulk  string `json:"bulk,omitempty" enum:"true" doc:"Set to true when this is one of several choices made together; it is only noted in the audit log"`
}

// UpdateIgnoreInput is the path and body of the ignore routes.
type UpdateIgnoreInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      UpdateIgnoreRequest
}

// UpdateHeldBody is the answer of a skip or an ignore.
type UpdateHeldBody struct {
	Update *UpdateInfoBody `json:"update,omitempty" doc:"What is known about the workload's image now. Absent for an ignore of an image, which covers many workloads"`
}

// UpdateHeldOutput wraps UpdateHeldBody for huma.
type UpdateHeldOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         UpdateHeldBody
}

// registerUpdateIgnores registers the skip and ignore routes.
func registerUpdateIgnores(r *httpx.Router, d UpdatesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "skipImageUpdate", Method: http.MethodPost, Path: "/api/env/{envId}/updates/skip",
		Summary: "Skip the offered version of a service or container",
		Description: "Hides the version of its image the service or container is offered now, until the tag points at a newer one. The version is the one the last check found, read on the server. " +
			"409 nothing_to_skip when the workload has no update available; 404 not_found when it is not there. Skipping a version that is skipped already changes nothing. " +
			"Audited as image.update_skipped. Needs updates.manage (an Env Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermUpdatesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateSkipInput) (*UpdateHeldOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := skipImageUpdate(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &UpdateHeldOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "unskipImageUpdate", Method: http.MethodDelete, Path: "/api/env/{envId}/updates/skip",
		Summary: "Stop skipping a version of a service or container",
		Description: "Takes back the skip of a service or container, so the update is offered again. 404 skip_not_found when it is not skipped. " +
			"Audited as image.update_unskipped. Needs updates.manage (an Env Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermUpdatesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateSkipInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, unskipImageUpdate(ctx, d, p, env.ID, in)
		})
	httpx.Register(r, huma.Operation{
		OperationID: "ignoreImageUpdates", Method: http.MethodPost, Path: "/api/env/{envId}/updates/ignore",
		Summary: "Ignore the updates of an image or of a service or container",
		Description: "Hides every update until the choice is removed. scope image covers an image and tag (named by image) for every service and container using it in this environment and no other; " +
			"scope workload covers one service or container (kind and name, 404 not_found when it is not there). " +
			"422 invalid_image when the image is not a reference with a tag, 422 invalid_ignore_target when the scope's fields are missing. Choosing what is ignored already changes nothing. " +
			"Audited as image.updates_ignored. Needs updates.manage (an Env Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermUpdatesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateIgnoreInput) (*UpdateHeldOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := ignoreImageUpdates(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &UpdateHeldOutput{CacheControl: "no-store", Body: body}, nil
		})
	httpx.Register(r, huma.Operation{
		OperationID: "unignoreImageUpdates", Method: http.MethodDelete, Path: "/api/env/{envId}/updates/ignore",
		Summary: "Stop ignoring the updates of an image or of a service or container",
		Description: "Takes back an ignore chosen through this API, so updates are offered again. 404 ignore_not_found when there is none. " +
			"409 ignored_by_label when the workload is ignored by the tierplan.updates=ignore label in its compose file, which only its owner can remove. " +
			"Audited as image.updates_unignored. Needs updates.manage (an Env Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermUpdatesManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *UpdateIgnoreInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, unignoreImageUpdates(ctx, d, p, env.ID, in)
		})
}

// ignoresReady answers the problem to give when the routes cannot run: update checks are not wired (503), or the
// store they save through is missing (500).
func (d UpdatesDeps) ignoresReady() error {
	if d.Updates == nil || d.Updates.Ignores == nil {
		return httpx.Problem(http.StatusServiceUnavailable, "", "Update checks are not available on this server.")
	}
	if d.Store == nil || d.Audit == nil {
		return huma.Error500InternalServerError("internal error")
	}
	return nil
}

// ignoreFailure logs a failed read or write of the skips and ignores and answers it as an internal error.
func ignoreFailure(d UpdatesDeps, envID, what string, err error) error {
	d.log().Error("image update skip or ignore request failed", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// workloadGone is the 404 of a service or container that is not there.
func workloadGone(kind string) error {
	return httpx.Problem(http.StatusNotFound, codeNotFound, "That "+kind+" is not there any more.")
}

// imageText is the image a workload follows, as the audit log names it; empty when it follows none.
func imageText(w updates.Workload) string {
	if !w.Checkable() {
		return ""
	}
	return w.Ref.Repo() + ":" + w.Ref.Tag
}

// holdAudit is the audit entry of a skip or an ignore, or of taking one back.
func holdAudit(ctx context.Context, p *rbac.Principal, envID, userAgent, action, targetType, targetID string, details map[string]string) audit.Entry {
	return audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: targetType, TargetID: targetID, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}
}

// skipImageUpdate skips the version a workload is offered now.
func skipImageUpdate(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdateSkipInput) (UpdateHeldBody, error) {
	if err := d.ignoresReady(); err != nil {
		return UpdateHeldBody{}, err
	}
	sv, found, err := d.Updates.Find(ctx, envID, in.Body.Kind, in.Body.Name)
	if err != nil {
		return UpdateHeldBody{}, snapshotProblem(d.log(), envID, err)
	}
	if !found {
		return UpdateHeldBody{}, workloadGone(in.Body.Kind)
	}
	switch sv.Verdict.State {
	case updates.StateSkipped:
		return UpdateHeldBody{Update: updateInfoOf(sv.Verdict, sv.Workload)}, nil // skipped already: the first skip stands
	case updates.StateUpdateAvailable:
	case updates.StateBusy, updates.StateUpdating:
		// Another operation holds the environment: the update is there, but nothing is changed on the environment until that ends.
		if sv.Verdict.Holder != nil {
			name := ""
			if env := httpx.EnvFrom(ctx); env != nil {
				name = env.Name
			}
			return UpdateHeldBody{}, operationLocked(name, *sv.Verdict.Holder)
		}
		return UpdateHeldBody{}, httpx.Problem(http.StatusConflict, problemNothingToSkip, "An update of this is running.")
	default:
		return UpdateHeldBody{}, httpx.Problem(http.StatusConflict, problemNothingToSkip, "There is no update to skip.")
	}
	w, digest := sv.Workload, sv.Verdict.RemoteDigest
	if digest == "" {
		return UpdateHeldBody{}, httpx.Problem(http.StatusConflict, problemNothingToSkip, "There is no update to skip.")
	}
	details := map[string]string{"kind": w.Kind, "workload": w.Name, "image": imageText(w), "digest": digest}
	if in.Body.Bulk != "" {
		details["bulk"] = in.Body.Bulk
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	store := updates.IgnoreStore{Q: d.Store.Q.WithTx(tx), W: d.Store.Write}
	if err := store.Add(ctx, updates.Ignore{
		EnvID: envID, Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: w.Kind, Name: w.Name, Digest: digest,
		SetBy: p.ID, SetAt: d.now().UTC(),
	}); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "save the skip", err)
	}
	if err := d.Audit.RecordTx(ctx, tx, holdAudit(ctx, p, envID, in.UserAgent, audit.ActionImageUpdateSkipped, w.Kind, w.ID, details)); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "commit", err)
	}
	return d.heldAfter(ctx, envID, w), nil
}

// heldAfter is the answer for a workload once its skip or ignore is saved: what is known about it now. A
// workload that cannot be judged again answers none; the lists carry the outcome.
func (d UpdatesDeps) heldAfter(ctx context.Context, envID string, w updates.Workload) UpdateHeldBody {
	sv, found, err := d.Updates.Find(ctx, envID, w.Kind, w.Name)
	if err != nil {
		d.log().Warn("could not judge a workload after its skip or ignore", "environment", envID, "error", err)
		return UpdateHeldBody{}
	}
	if !found {
		return UpdateHeldBody{}
	}
	return UpdateHeldBody{Update: updateInfoOf(sv.Verdict, sv.Workload)}
}

// unskipImageUpdate takes back the skip of a workload.
func unskipImageUpdate(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdateSkipInput) error {
	if err := d.ignoresReady(); err != nil {
		return err
	}
	name := strings.TrimPrefix(in.Body.Name, "/")
	// The workload is read before the transaction starts: the verdicts are read on the connection it holds.
	image := ""
	if sv, found, err := d.Updates.Find(ctx, envID, in.Body.Kind, name); err == nil && found {
		image = imageText(sv.Workload)
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return ignoreFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	store := updates.IgnoreStore{Q: d.Store.Q.WithTx(tx), W: d.Store.Write}
	ok, err := store.Remove(ctx, envID, updates.Ignore{Mode: updates.ModeSkip, Scope: updates.ScopeWorkload, Kind: in.Body.Kind, Name: name})
	if err != nil {
		return ignoreFailure(d, envID, "remove the skip", err)
	}
	if !ok {
		return httpx.Problem(http.StatusNotFound, problemSkipNotFound, "That version is not skipped.")
	}
	details := map[string]string{"kind": in.Body.Kind, "workload": name}
	if image != "" {
		details["image"] = image
	}
	if in.Body.Bulk != "" {
		details["bulk"] = in.Body.Bulk
	}
	if err := d.Audit.RecordTx(ctx, tx, holdAudit(ctx, p, envID, in.UserAgent, audit.ActionImageUpdateUnskipped, in.Body.Kind, name, details)); err != nil {
		return ignoreFailure(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return ignoreFailure(d, envID, "commit", err)
	}
	return nil
}

// ignoreKey is what an ignore request names, as the store keys it, with the words the audit log uses.
type ignoreKey struct {
	updates.Ignore
	// image is the image text of an image-scoped key, else of the workload when it is known.
	image string
}

// ignoreKeyOf reads the target of a request: a reference with a tag for the image scope, a kind and a name for the
// workload scope. envID is set on the key.
func ignoreKeyOf(envID string, b UpdateIgnoreRequest) (ignoreKey, error) {
	k := ignoreKey{Ignore: updates.Ignore{EnvID: envID, Mode: updates.ModeIgnore, Scope: b.Scope}}
	switch b.Scope {
	case updates.ScopeImage:
		ref, err := registry.ParseRef(b.Image)
		if err != nil || ref.Tag == "" || !registry.ValidTag(ref.Tag) {
			return ignoreKey{}, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidImage,
				"Name an image with a tag, such as nginx:1.27. An image named by digest alone is never checked, so there is nothing to ignore.")
		}
		k.Host, k.Repo, k.Tag = ref.Domain, ref.Path, ref.Tag
		k.image = ref.Repo() + ":" + ref.Tag
	case updates.ScopeWorkload:
		name := strings.TrimPrefix(b.Name, "/")
		if b.Kind == "" || name == "" {
			return ignoreKey{}, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidIgnoreTarget,
				"Name the service or container: its kind and its name.")
		}
		k.Kind, k.Name = b.Kind, name
	default:
		return ignoreKey{}, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidIgnoreTarget, "Choose an image or a service or container.")
	}
	return k, nil
}

// details are the audit details of the key; workload is the workload's kind, name and image when known.
func (k ignoreKey) details(bulk string) map[string]string {
	out := map[string]string{"scope": k.Scope}
	if k.Scope == updates.ScopeWorkload {
		out["kind"], out["workload"] = k.Kind, k.Name
	}
	if k.image != "" {
		out["image"] = k.image
	}
	if bulk != "" {
		out["bulk"] = bulk
	}
	return out
}

// target is the audit target of the key: the environment for an image, the workload for a workload.
func (k ignoreKey) target(envID string) (typ, id string) {
	if k.Scope == updates.ScopeWorkload {
		return k.Kind, k.Name
	}
	return auditTargetEnvironment, envID
}

// ignoreImageUpdates ignores the updates of an image or of a workload.
func ignoreImageUpdates(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdateIgnoreInput) (UpdateHeldBody, error) {
	if err := d.ignoresReady(); err != nil {
		return UpdateHeldBody{}, err
	}
	k, err := ignoreKeyOf(envID, in.Body)
	if err != nil {
		return UpdateHeldBody{}, err
	}
	var w updates.Workload
	if k.Scope == updates.ScopeWorkload {
		sv, found, err := d.Updates.Find(ctx, envID, k.Kind, k.Name)
		if err != nil {
			return UpdateHeldBody{}, snapshotProblem(d.log(), envID, err)
		}
		if !found {
			return UpdateHeldBody{}, workloadGone(k.Kind)
		}
		w = sv.Workload
		k.image = imageText(w)
	}
	held, err := d.Updates.Ignores.List(ctx, envID)
	if err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "read the ignores", err)
	}
	for _, ig := range held {
		if ig.Mode == updates.ModeIgnore && ig.Scope == k.Scope && ig.Host == k.Host && ig.Repo == k.Repo && ig.Tag == k.Tag &&
			ig.Kind == k.Kind && ig.Name == k.Name {
			return d.heldBody(ctx, envID, k, w), nil // ignored already: the first choice stands
		}
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	store := updates.IgnoreStore{Q: d.Store.Q.WithTx(tx), W: d.Store.Write}
	k.SetBy, k.SetAt = p.ID, d.now().UTC()
	if err := store.Add(ctx, k.Ignore); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "save the ignore", err)
	}
	typ, id := k.target(envID)
	if k.Scope == updates.ScopeWorkload {
		id = w.ID
	}
	if err := d.Audit.RecordTx(ctx, tx, holdAudit(ctx, p, envID, in.UserAgent, audit.ActionImageUpdatesIgnored, typ, id, k.details(in.Body.Bulk))); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return UpdateHeldBody{}, ignoreFailure(d, envID, "commit", err)
	}
	return d.heldBody(ctx, envID, k, w), nil
}

// heldBody is the answer of an ignore: the workload's update object for a workload, none for an image.
func (d UpdatesDeps) heldBody(ctx context.Context, envID string, k ignoreKey, w updates.Workload) UpdateHeldBody {
	if k.Scope != updates.ScopeWorkload {
		return UpdateHeldBody{}
	}
	return d.heldAfter(ctx, envID, w)
}

// unignoreImageUpdates takes back an ignore chosen through the API. A workload ignored by its label cannot be
// freed here.
func unignoreImageUpdates(ctx context.Context, d UpdatesDeps, p *rbac.Principal, envID string, in *UpdateIgnoreInput) error {
	if err := d.ignoresReady(); err != nil {
		return err
	}
	k, err := ignoreKeyOf(envID, in.Body)
	if err != nil {
		return err
	}
	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return ignoreFailure(d, envID, "begin", err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	store := updates.IgnoreStore{Q: d.Store.Q.WithTx(tx), W: d.Store.Write}
	ok, err := store.Remove(ctx, envID, k.Ignore)
	if err != nil {
		return ignoreFailure(d, envID, "remove the ignore", err)
	}
	if !ok {
		_ = tx.Rollback() // the verdict below is read on the connection the transaction holds
		if k.Scope == updates.ScopeWorkload {
			if sv, found, err := d.Updates.Find(ctx, envID, k.Kind, k.Name); err == nil && found && sv.Verdict.Ignore != nil && sv.Verdict.Ignore.ByLabel {
				return httpx.Problem(http.StatusConflict, problemIgnoredByLabel,
					"This is ignored by the tierplan.updates=ignore label in its compose file. Remove the label there and deploy again.")
			}
		}
		return httpx.Problem(http.StatusNotFound, problemIgnoreNotFound, "That is not ignored.")
	}
	typ, id := k.target(envID)
	if err := d.Audit.RecordTx(ctx, tx, holdAudit(ctx, p, envID, in.UserAgent, audit.ActionImageUpdatesUnignored, typ, id, k.details(in.Body.Bulk))); err != nil {
		return ignoreFailure(d, envID, "audit", err)
	}
	if err := tx.Commit(); err != nil {
		return ignoreFailure(d, envID, "commit", err)
	}
	return nil
}
