// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// codeImageInUse is the result of a remove of an image a container or a swarm service still
// references, found by the check or refused by the engine (D-28).
const codeImageInUse = "image_in_use"

// ImageRemoveTarget names one image on one node. An image ID is the same on every node that
// holds an identical copy, but a copy is removed on its own node, so a target carries both.
type ImageRemoveTarget struct {
	NodeID  string `json:"nodeId" minLength:"1" maxLength:"128" doc:"The node the copy is on: the nodeId of its row in the images list. Never the display name"`
	ImageID string `json:"imageId" pattern:"^sha256:[a-f0-9]{64}$" maxLength:"71" doc:"The full image ID: sha256: and 64 hexadecimal characters"`
}

// ImageRemoveRequest is the body of POST /api/env/{envId}/images/remove.
type ImageRemoveRequest struct {
	BulkID  string              `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request. Send a UUID to name the bulk yourself; with more than one target and none sent, the server makes one (a UUIDv7) and answers it"`
	Targets []ImageRemoveTarget `json:"targets" minItems:"1" maxItems:"25" doc:"The image copies to remove, at most 25. Each is answered separately; one that fails never stops the others"`
}

// ImageRemoveInput is the path and body of the remove route.
type ImageRemoveInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ImageRemoveRequest
}

// ImageRemoveResult is the answer for one target.
type ImageRemoveResult struct {
	NodeID  string `json:"nodeId" doc:"The node ID the request named"`
	ImageID string `json:"imageId" doc:"The image ID the request named"`
	Result  string `json:"result" enum:"done,failed" doc:"done: the image and all its names are gone from the node. failed: it was refused or could not be sent; code says why"`
	Code    string `json:"code,omitempty" enum:"image_in_use,not_found,node_unreachable,host_read_only,agent_policy_denied,action_failed" doc:"Why a target failed. image_in_use: a container on the node, running or not, or a swarm service spec references the image; nothing was removed and detail names up to 5 users. not_found: the node has no such image, so it is already gone. node_unreachable: the node is not a connected node of this environment, its engine did not answer, or on a swarm no manager could be asked for the services; nothing was removed. host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the node's agent. agent_policy_denied: the node's agent refused the call by its own policy. action_failed: the engine refused it; detail has its message"`
	Detail  string `json:"detail,omitempty" doc:"A plain sentence, or for action_failed the engine's message, at most 300 characters"`
}

// ImageRemoveResponse is the per-target outcome, in request order.
type ImageRemoveResponse struct {
	BulkID  string              `json:"bulkId,omitempty" doc:"The bulk ID recorded in the audit rows of this request: the one sent, or the one the server made for a request with more than one target. Absent for a single target with none sent"`
	Results []ImageRemoveResult `json:"results" doc:"One entry per requested target, in request order"`
}

// ImageRemoveOutput wraps ImageRemoveResponse for huma.
type ImageRemoveOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageRemoveResponse
}

// errImageGone is what a removal answers when the node does not have the image.
var errImageGone = errors.New("api: the node has no such image")

// imageBusyError is what a removal closure answers when something uses the image: the
// containers and services that do, named for the refusal.
type imageBusyError struct{ users []string }

func (e *imageBusyError) Error() string {
	return "api: image is in use by " + strings.Join(e.users, ", ")
}

// registerImageRemove registers the remove route (D-28).
func registerImageRemove(r *httpx.Router, d ImagesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "removeImages", Method: http.MethodPost, Path: "/api/env/{envId}/images/remove",
		Summary: "Remove images",
		Description: "Removes up to 25 image copies, each named by its node ID and image ID, and answers one result per target in request order. " +
			"An image with several names has each name untagged first and is then deleted by ID, so the engine's must-be-forced refusal never stops a removal; force is never sent. " +
			"An image that any container on its node, running or stopped, references is never removed: the result is failed with code image_in_use and the detail names up to 5 users. " +
			"The users are read again from the node just before each removal, and a container that starts between the check and the call makes the engine refuse and is answered image_in_use too. " +
			"An image the node does not have is failed with code not_found and writes no audit row. One that fails never stops the others. " +
			"Needs images.delete (Operator) on the environment. " +
			"With more than one target (or a bulkId sent) every audit row of the request carries the same bulk detail, a UUIDv7 the server makes when the client sends none, and the response returns it. " +
			"Every attempt on an image the node has is audited as image.removed, failures included.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusOK,
	}, rbac.Requirement{Perm: rbac.PermImagesDelete, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ImageRemoveInput) (*ImageRemoveOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return removeImages(ctx, d, p, env.ID, in)
		})
}

// removeImages removes each image copy on its own node, once and never retried, answering
// each target on its own. A target that fails never stops the others.
func removeImages(ctx context.Context, d ImagesDeps, p *rbac.Principal, envID string, in *ImageRemoveInput) (*ImageRemoveOutput, error) {
	bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	results := make([]ImageRemoveResult, len(in.Body.Targets))
	attempts := make([]imageAttempt, len(results))
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	for i, t := range in.Body.Targets {
		g.Go(func() error {
			release, err := lim.acquire(ctx, t.NodeID)
			if err != nil {
				results[i] = ImageRemoveResult{NodeID: t.NodeID, ImageID: t.ImageID, Result: actionFailed, Code: codeActionFailed, Detail: "The request was cancelled before this target was reached."}
				return nil
			}
			defer release()
			results[i], attempts[i] = removeOneImage(ctx, d, envID, t)
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: failures are results
	for i, t := range in.Body.Targets {
		if attempts[i].attempted {
			auditImageChange(ctx, d, p, envID, in.UserAgent, bulkID, audit.ActionImageRemoved, t.NodeID, t.ImageID, attempts[i].ref, results[i].Result, results[i].Code)
		}
	}
	return &ImageRemoveOutput{CacheControl: "no-store", Body: ImageRemoveResponse{BulkID: bulkID, Results: results}}, nil
}

// imageAttempt is what the audit row of one target needs, kept until every target has
// answered so the rows of one bulk read in the order the targets were sent. attempted is
// false for an image the node does not have, which there is nothing to record about.
type imageAttempt struct {
	ref       string
	attempted bool
}

// imageRef is how an image is named in a sentence and an audit row: its first tag, else the
// first 12 characters of its ID.
func imageRef(tags []string, id string) string {
	for _, t := range tags {
		if !isNoneTag(t) {
			return t
		}
	}
	return shortImageID(id)
}

// removeOneImage removes one image copy on its node and says what happened. It never
// returns an error: a failure is a result of its own. Nothing is removed on the strength of
// a list the caller saw: just before the removal the node's containers, running or not, are
// read again, and anything that was created from the image refuses the removal without a
// call to remove it being made. force is never sent, so a container that starts in between
// is refused by the engine.
func removeOneImage(ctx context.Context, d ImagesDeps, envID string, t ImageRemoveTarget) (ImageRemoveResult, imageAttempt) {
	res := ImageRemoveResult{NodeID: t.NodeID, ImageID: t.ImageID}
	at := imageAttempt{ref: shortImageID(t.ImageID), attempted: true}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err := d.Aggregator.WriteOnNode(writeCtx, envID, t.NodeID, func(c *dockerx.Client) error {
		det, _, e := c.ImageDetail(writeCtx, t.ImageID)
		if e != nil {
			if dockerx.IsNotFound(e) {
				return errImageGone
			}
			return e
		}
		at.ref = imageRef(det.RepoTags, t.ImageID)
		users, e := c.ImageUsers(writeCtx, t.ImageID)
		if e != nil {
			return e
		}
		if len(users) > 0 {
			return &imageBusyError{users: prefixed("container ", users)}
		}
		return c.ImageRemove(writeCtx, t.ImageID, append(append([]string{}, det.RepoTags...), det.RepoDigests...))
	})
	if err != nil {
		code, detail := imageRemoveError(at.ref, err)
		res.Result, res.Code, res.Detail = actionFailed, code, detail
		at.attempted = code != codeNotFound
		return res, at
	}
	res.Result = actionDone
	return res, at
}

// prefixed returns each item with the prefix in front of it.
func prefixed(prefix string, items []string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = prefix + it
	}
	return out
}

// imageRemoveError maps the error of a removal to a result code and a sentence. An engine
// that says the image is gone is not_found, and one that says something uses it, which can
// happen when a container starts between the check and the call, is image_in_use and never
// a generic failure.
func imageRemoveError(ref string, err error) (code, detail string) {
	var busy *imageBusyError
	switch {
	case errors.As(err, &busy):
		return codeImageInUse, ref + " is still in use by " + joinMore(busy.users, maxNamedUsers) + ". Remove what uses it, then try again."
	case dockerx.IsNotFound(err), errors.Is(err, errImageGone):
		return codeNotFound, ref + " is already gone."
	case dockerx.IsConflict(err):
		return codeImageInUse, ref + " is still in use. Remove what uses it, then try again."
	}
	return actionError(err)
}

// auditImageChange writes the row of one remove or untag attempt on an image that exists. A
// row that cannot be written is logged: the image has been removed or refused, and the
// caller must still be told which.
func auditImageChange(ctx context.Context, d ImagesDeps, p *rbac.Principal, envID, userAgent, bulkID, action, nodeID, imageID, ref, outcome, failCode string) {
	result, reason, code := audit.ResultSuccess, "", codeRemoved
	if action == audit.ActionImageUntagged {
		code = codeUntagged
	}
	if outcome != actionDone {
		result, reason, code = audit.ResultFailure, failCode, failCode
	}
	details := map[string]string{"ref": ref, "node": nodeID, "code": code}
	if bulkID != "" {
		details["bulk"] = bulkID
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: imageTargetTyp, TargetID: imageID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit an image action", "action", action, "image", imageID, "error", err)
	}
}

// codeUntagged is the code of the audit row of a name taken off an image.
const codeUntagged = "untagged"
