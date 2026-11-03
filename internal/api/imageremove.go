// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stacks"
)

const (
	// codeImageInUse is the result of a remove of an image a container or a swarm service still
	// references, found by the check or refused by the engine (D-28).
	codeImageInUse = "image_in_use"
	// codeLastNameInUse is the result of an untag of the last name of an image that is in use:
	// untagging it would remove the image (D-29).
	codeLastNameInUse = "last_name_in_use"
	// codeNotDangling is the result of a target that has a name in a request of more than one
	// image: a bulk removes untagged, unused images only, so it is skipped and nothing is sent
	// for it (D-17, T-05-58). One is removed alone, with the name shown in the dialog.
	codeNotDangling = "not_dangling"
)

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
	Result  string `json:"result" enum:"done,failed,skipped" doc:"done: the image and all its names are gone from the node. failed: it was refused or could not be sent; code says why. skipped: not acted on because the image has a name and the request was a bulk delete of dangling images; code is not_dangling"`
	Code    string `json:"code,omitempty" enum:"image_in_use,not_dangling,not_found,node_unreachable,host_read_only,agent_policy_denied,action_failed" doc:"Why a target failed or was skipped. not_dangling (with result skipped): the image has a name and the request was a bulk delete of dangling images; nothing was sent. image_in_use: a container on the node, running or not, or a swarm service spec references the image; nothing was removed and detail names up to 5 users. not_found: the node has no such image, so it is already gone. node_unreachable: the node is not a connected node of this environment, its engine did not answer, or on a swarm no manager could be asked for the services; nothing was removed. host_read_only: the host owner set TIERPLAN_AGENT_READ_ONLY on the node's agent. agent_policy_denied: the node's agent refused the call by its own policy. action_failed: the engine refused it; detail has its message"`
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

// ImageUntagRequest is the body of POST /api/env/{envId}/images/untag.
type ImageUntagRequest struct {
	ImageID string   `json:"imageId" pattern:"^sha256:[a-f0-9]{64}$" maxLength:"71" doc:"The full image ID: sha256: and 64 hexadecimal characters"`
	Ref     string   `json:"ref" minLength:"1" maxLength:"255" doc:"The name to take off the image, as the image's tags show it: repository:tag"`
	NodeIDs []string `json:"nodeIds,omitempty" maxItems:"25" doc:"The nodes to untag it on, by nodeId. Left out, every node whose copy of the image has that name"`
	BulkID  string   `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one request. Send a UUID to name the bulk yourself; with more than one node and none sent, the server makes one (a UUIDv7) and answers it"`
}

// ImageUntagInput is the path and body of the untag route.
type ImageUntagInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ImageUntagRequest
}

// ImageUntagResult is the answer for one node.
type ImageUntagResult struct {
	NodeID  string `json:"nodeId" doc:"The node ID the name was taken off, or was to be"`
	ImageID string `json:"imageId" doc:"The image ID the request named"`
	Ref     string `json:"ref" doc:"The name the request named"`
	Result  string `json:"result" enum:"done,failed" doc:"done: the name is gone from the node. failed: it was refused or could not be sent; code says why"`
	Code    string `json:"code,omitempty" enum:"last_name_in_use,image_in_use,not_found,node_unreachable,host_read_only,agent_policy_denied,action_failed" doc:"Why a node failed. last_name_in_use: it is the image's last name on the node and the image is in use, so untagging it would remove an image that is in use. image_in_use: the engine refused because a container uses the image. not_found: the node has no such image or no such name on it. The others are those of a remove"`
	Detail  string `json:"detail,omitempty" doc:"A plain sentence, or for action_failed the engine's message, at most 300 characters"`
	Removed bool   `json:"removed" doc:"True when it was the image's only name on the node and the image was not in use, so the image was removed with it; the audit row is then image.removed"`
}

// ImageUntagResponse is the per-node outcome, in the order of the nodes.
type ImageUntagResponse struct {
	BulkID  string             `json:"bulkId,omitempty" doc:"The bulk ID recorded in the audit rows of this request: the one sent, or the one the server made for a request with more than one node. Absent for one node with none sent"`
	Results []ImageUntagResult `json:"results" doc:"One entry per node, in the order of nodeIds, else by node name"`
}

// ImageUntagOutput wraps ImageUntagResponse for huma.
type ImageUntagOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageUntagResponse
}

// errImageGone and errNameGone are what a closure answers when the node does not have the
// image, or the image has no longer that name on it.
var (
	errImageGone = errors.New("api: the node has no such image")
	errNameGone  = errors.New("api: the image has no such name on the node")
	// errNotDangling is what a strict-dangling closure answers when the image has gained a name or
	// a keep label since the janitor looked.
	errNotDangling = errors.New("api: the image is no longer dangling")
)

// imageBusyError is what a closure answers when something uses the image: the containers and
// services that do, named for the refusal. lastName says the refused act was an untag of the
// last name.
type imageBusyError struct {
	users    []string
	lastName bool
}

func (e *imageBusyError) Error() string {
	return "api: image is in use by " + strings.Join(e.users, ", ")
}

// registerImageRemove registers the remove and untag routes (D-28, D-29).
func registerImageRemove(r *httpx.Router, d ImagesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "removeImages", Method: http.MethodPost, Path: "/api/env/{envId}/images/remove",
		Summary: "Remove images",
		Description: "Removes up to 25 image copies, each named by its node ID and image ID, and answers one result per target in request order. " +
			"An image with several names has each name untagged first and is then deleted by ID, so the engine's must-be-forced refusal never stops a removal; force is never sent. " +
			"An image that any container on its node, running or stopped, references is never removed there, and an image that any swarm service spec references, even with no task, is never removed on any node, because Swarm would pull it again: the result is failed with code image_in_use and the detail names up to 5 users. " +
			"The containers are read again from the node just before each removal and the service specs from a manager, and a container that starts between the check and the call makes the engine refuse and is answered image_in_use too. On a swarm whose services cannot be read nothing is removed and the target is node_unreachable. " +
			"An image the node does not have is failed with code not_found and writes no audit row. One that fails never stops the others. " +
			"A request that names more than one image, or sends a bulkId, is a bulk delete of dangling images: a target whose image has a name is skipped with code not_dangling, nothing is sent for it and it writes no audit row, whatever the browser sent. One image, on one or several nodes, is removed whatever its names. " +
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

	httpx.Register(r, huma.Operation{
		OperationID: "untagImage", Method: http.MethodPost, Path: "/api/env/{envId}/images/untag",
		Summary: "Take one name off an image",
		Description: "Untags one name (repository:tag) of an image on each node named in nodeIds, or on every node whose copy has that name, and answers one result per node. " +
			"Untagging one of several names is always allowed, even while the image is in use: the image stays under its other names. " +
			"The last name of an image that is in use is never untagged: the result is failed with code last_name_in_use. The last name of an image that is not in use is untagged and the image removed with it (removed is true, and the audit row is image.removed). " +
			"A name the node does not have is failed with code not_found and writes no audit row. 404 image_not_found when nodeIds is left out and no node has the image under that name. " +
			"Needs images.delete (Operator) on the environment. Every other attempt writes one image.untagged (or image.removed) row per node with a shared bulk ID.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusOK,
	}, rbac.Requirement{Perm: rbac.PermImagesDelete, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ImageUntagInput) (*ImageUntagOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return untagImage(ctx, d, p, env.ID, in)
		})
}

// imageCheck is what a removal is decided from, read once for a request just before its
// first call: every node's images from an aggregate whose caches were dropped, and on a swarm
// the service specs from a manager now. The containers of each node are read again by the
// closure that removes (D-28, T-05-54).
type imageCheck struct {
	agg   envs.Aggregate
	usage imageUsage
	swarm bool
	// svcErr is set when the specs of a swarm's services could not be read: nothing on it can
	// be told to be unused.
	svcErr error
	// specs are the service specs the check read, for the janitor's strict rule, which also asks
	// whether a spec names an image by its ID.
	specs []dockerx.ServiceRef
}

// readImageCheck reads the check of a request.
func readImageCheck(ctx context.Context, d ImagesDeps, envID string) (imageCheck, error) {
	agg, _, err := d.Aggregator.FreshSnapshot(ctx, envID)
	if err != nil {
		return imageCheck{}, snapshotProblem(d.Log, envID, err)
	}
	chk := imageCheck{agg: agg, swarm: agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker}
	var refs []dockerx.ServiceRef
	if chk.swarm {
		refs, chk.svcErr = liveServiceRefs(ctx, d, envID)
	}
	chk.usage = imageUsageFrom(agg, refs, nil)
	chk.specs = refs
	return chk, nil
}

// liveServiceRefs reads the service specs from a manager now. A spec counts even when it has
// no running task (Pitfall 7).
func liveServiceRefs(ctx context.Context, d ImagesDeps, envID string) ([]dockerx.ServiceRef, error) {
	if d.Clusters == nil {
		return nil, errServicesUnread
	}
	readCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var refs []dockerx.ServiceRef
	if err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		refs, e = c.ServiceRefs(readCtx)
		return e
	}); err != nil {
		d.Log.Warn("could not read the services before an image removal", "environment", envID, "error", err)
		return nil, errServicesUnread
	}
	return refs, nil
}

// imageOp is one image copy on one node to remove, or one name of it to take off.
type imageOp struct {
	nodeID, imageID string
	// untag is the name to take off; empty removes the image.
	untag string
	// danglingOnly refuses an image that has a name: set on every target of a bulk delete,
	// which removes untagged images only (D-17).
	danglingOnly bool
	// strictDangling is the janitor's rule (D-07): inside the write call, once the image's names
	// are read again, an image that has a real tag or any digest reference, or is labelled
	// tierplan.keep=true, is refused with not_dangling and nothing is removed. It is checked on the
	// image as the node has it at that moment, not on the list the caller saw.
	strictDangling bool
}

// imageOutcome is what happened to one op, and what its audit row needs.
type imageOutcome struct {
	ref string
	// code is empty for done; detail is the sentence of a failure.
	code, detail string
	// attempted is false for an image or a name the node does not have, which there is nothing
	// to record about.
	attempted bool
	// removed says the image was deleted. asRemove says the act was, or would have been, a
	// removal: a remove, or an untag of the last name of the image.
	removed, asRemove bool
}

// runImageOps does each op on its node, a few at once and at most perNodeCalls to one node,
// once and never retried, and answers them in the order given. A failing op never stops the
// others. The audit rows are written afterwards, in the same order, so the rows of one bulk
// read as the targets were sent.
func runImageOps(ctx context.Context, d ImagesDeps, p *rbac.Principal, envID, userAgent, bulkID string, chk imageCheck, ops []imageOp) []imageOutcome {
	out := make([]imageOutcome, len(ops))
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	for i, op := range ops {
		g.Go(func() error {
			release, err := lim.acquire(ctx, op.nodeID)
			if err != nil {
				out[i] = imageOutcome{code: codeActionFailed, detail: "The request was cancelled before this target was reached."}
				return nil
			}
			defer release()
			out[i] = runImageOp(ctx, d, envID, chk, op)
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: failures are outcomes
	for i, op := range ops {
		if !out[i].attempted {
			continue
		}
		action := audit.ActionImageUntagged
		if out[i].asRemove {
			action = audit.ActionImageRemoved
		}
		result := actionDone
		if out[i].code != "" {
			result = actionFailed
		}
		auditImageChange(ctx, d, p, envID, userAgent, bulkID, action, op.nodeID, op.imageID, out[i].ref, result, out[i].code)
	}
	return out
}

// runImageOp does one op on its node and says what happened. It never returns an error: a
// failure is an outcome of its own. Nothing is removed on the strength of a list the caller
// saw: the check was read just before, the node's containers, running or not, are read again
// inside the call, and anything that uses the image refuses the removal without a call to
// remove it being made. force is never sent, so a container that starts in between is refused
// by the engine and answered image_in_use.
func runImageOp(ctx context.Context, d ImagesDeps, envID string, chk imageCheck, op imageOp) imageOutcome {
	out := imageOutcome{ref: op.untag, attempted: true, asRemove: op.untag == ""}
	fail := func(code, detail string) imageOutcome {
		out.code, out.detail = code, detail
		return out
	}
	if !slices.ContainsFunc(chk.agg.Nodes, func(n envs.NodeRef) bool { return n.ID == op.nodeID }) {
		if out.ref == "" {
			out.ref = shortImageID(op.imageID)
		}
		return fail(codeNodeUnreachable, "The node is not connected to TierPlan.")
	}
	i := slices.IndexFunc(chk.agg.Images, func(r envs.ImageRow) bool { return r.NodeID == op.nodeID && r.ID == op.imageID })
	if i < 0 {
		out.attempted = false
		if out.ref == "" {
			out.ref = shortImageID(op.imageID)
		}
		return fail(codeNotFound, out.ref+" is already gone.")
	}
	row := chk.agg.Images[i]
	if op.untag == "" {
		out.ref = imageRef(row.RepoTags, op.imageID)
	} else if !slices.ContainsFunc(row.RepoTags, func(tag string) bool { return sameRef(tag, op.untag) }) {
		out.attempted = false
		return fail(codeNotFound, op.untag+" is already gone.")
	}
	if op.danglingOnly && op.untag == "" && slices.ContainsFunc(row.RepoTags, func(tag string) bool { return !isNoneTag(tag) }) {
		out.attempted = false
		return fail(codeNotDangling, out.ref+" has a name, so it was not deleted with the others. Open the image to remove it.")
	}
	// A cheap early exit on the list the check read; the test that decides is the one inside the
	// write call below, on the image as the node has it then.
	if op.strictDangling && op.untag == "" && !strictlyDangling(row.RepoTags, row.RepoDigests, row.Labels) {
		out.attempted = false
		return fail(codeNotDangling, out.ref+" has a name or is kept, so it is not dangling and was left alone.")
	}
	if chk.svcErr != nil {
		return fail(codeNodeUnreachable, "Can't tell whether "+out.ref+" is in use: no manager answered. Try again when one is back.")
	}
	var services []string
	for _, use := range chk.usage.servicesOf(row.ImageRow) {
		services = append(services, "service "+use.ref.Name)
	}
	if op.strictDangling {
		// An image with no name can only be named by a spec through its ID.
		for _, ref := range chk.specs {
			if janitor.ImageNamedBySpec([]string{ref.Image}, op.imageID) {
				services = append(services, "service "+ref.Name)
			}
		}
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err := d.Aggregator.WriteOnNode(writeCtx, envID, op.nodeID, func(c *dockerx.Client) error {
		det, _, e := c.ImageDetail(writeCtx, op.imageID)
		if e != nil {
			if dockerx.IsNotFound(e) {
				return errImageGone
			}
			return e
		}
		if op.strictDangling && op.untag == "" && !strictlyDangling(det.RepoTags, det.RepoDigests, det.Labels) {
			// It gained a name or a keep label since the list was read: an ImageRemove now would untag
			// that name too and delete an image somebody wants.
			return errNotDangling
		}
		names := append(slices.Clone(det.RepoTags), det.RepoDigests...)
		untag := op.untag
		if untag != "" {
			// The name as the node has it, which is what the engine is asked to untag.
			at := slices.IndexFunc(det.RepoTags, func(tag string) bool { return sameRef(tag, untag) })
			if at < 0 {
				return errNameGone
			}
			untag = det.RepoTags[at]
			if len(det.RepoTags) > 1 {
				// Another name remains, so the image stays whatever uses it (D-29).
				out.asRemove = false
				_, e := c.ImageUntag(writeCtx, untag)
				return e
			}
			out.asRemove = true
		}
		containers, e := c.ImageUsers(writeCtx, op.imageID)
		if e != nil {
			return e
		}
		if users := append(prefixed("container ", containers), services...); len(users) > 0 {
			return &imageBusyError{users: users, lastName: op.untag != ""}
		}
		if e := c.ImageRemove(writeCtx, op.imageID, names); e != nil {
			return e
		}
		out.removed = true
		return nil
	})
	if err != nil {
		code, detail := imageRemoveError(out.ref, err)
		// An image found no longer dangling was only read, never sent a removal, as the early exit above.
		out.attempted = code != codeNotFound && code != codeNotDangling
		return fail(code, detail)
	}
	return out
}

// strictlyDangling is the janitor's rule for an image it may remove (D-07): no real tag, no digest
// reference, and no tierplan.keep=true label.
func strictlyDangling(tags, digests []string, labels map[string]string) bool {
	return !slices.ContainsFunc(tags, func(tag string) bool { return !isNoneTag(tag) }) &&
		len(digests) == 0 && labels[stacks.LabelKeep] != "true"
}

// sameRef says whether two image names are the same reference once written out in full, so
// nginx:1.27 and docker.io/library/nginx:1.27 are one. Names that do not parse are equal
// only when they are the same text.
func sameRef(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := registry.ParseRef(a)
	rb, errB := registry.ParseRef(b)
	return errA == nil && errB == nil && ra.String() == rb.String()
}

// removeImages removes each image copy on its own node, once and never retried, answering
// each target on its own. A target that fails never stops the others.
func removeImages(ctx context.Context, d ImagesDeps, p *rbac.Principal, envID string, in *ImageRemoveInput) (*ImageRemoveOutput, error) {
	bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	chk, err := readImageCheck(ctx, d, envID)
	if err != nil {
		return nil, err
	}
	// A request that names more than one image, or that carries a bulk ID (the later chunks of
	// a long delete), is a bulk delete of dangling images. One image on several nodes is a
	// remove of that image from the nodes chosen, which may be a named one (D-28).
	images := map[string]bool{}
	for _, t := range in.Body.Targets {
		images[t.ImageID] = true
	}
	bulk := len(images) > 1 || in.Body.BulkID != ""
	ops := make([]imageOp, len(in.Body.Targets))
	for i, t := range in.Body.Targets {
		ops[i] = imageOp{nodeID: t.NodeID, imageID: t.ImageID, danglingOnly: bulk}
	}
	outs := runImageOps(ctx, d, p, envID, in.UserAgent, bulkID, chk, ops)
	results := make([]ImageRemoveResult, len(outs))
	for i, o := range outs {
		results[i] = ImageRemoveResult{NodeID: ops[i].nodeID, ImageID: ops[i].imageID, Result: actionDone}
		if o.code != "" {
			results[i].Result, results[i].Code, results[i].Detail = actionFailed, o.code, o.detail
			if o.code == codeNotDangling {
				results[i].Result = actionSkipped
			}
		}
	}
	return &ImageRemoveOutput{CacheControl: "no-store", Body: ImageRemoveResponse{BulkID: bulkID, Results: results}}, nil
}

// untagImage takes one name off an image on each node asked, or on each node whose copy has
// it. Untagging the last name of an image that is in use is refused; of one that is not, the
// image is removed with it (D-29).
func untagImage(ctx context.Context, d ImagesDeps, p *rbac.Principal, envID string, in *ImageUntagInput) (*ImageUntagOutput, error) {
	chk, err := readImageCheck(ctx, d, envID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	nodeIDs := slices.DeleteFunc(slices.Clone(in.Body.NodeIDs), func(id string) bool {
		dup := seen[id]
		seen[id] = true
		return dup
	})
	if len(nodeIDs) == 0 {
		for _, row := range chk.agg.Images {
			if row.ID == in.Body.ImageID && slices.ContainsFunc(row.RepoTags, func(tag string) bool { return sameRef(tag, in.Body.Ref) }) {
				nodeIDs = append(nodeIDs, row.NodeID)
			}
		}
	}
	if len(nodeIDs) == 0 {
		return nil, httpx.Problem(http.StatusNotFound, problemImageNotFound, "No node has this image under that name. It may have been removed, or its tag may have moved to another image.")
	}
	bulkID, err := bulkIDFor(in.Body.BulkID, len(nodeIDs))
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	ops := make([]imageOp, len(nodeIDs))
	for i, id := range nodeIDs {
		ops[i] = imageOp{nodeID: id, imageID: in.Body.ImageID, untag: in.Body.Ref}
	}
	outs := runImageOps(ctx, d, p, envID, in.UserAgent, bulkID, chk, ops)
	results := make([]ImageUntagResult, len(outs))
	for i, o := range outs {
		results[i] = ImageUntagResult{NodeID: ops[i].nodeID, ImageID: ops[i].imageID, Ref: in.Body.Ref, Result: actionDone, Removed: o.removed}
		if o.code != "" {
			results[i].Result, results[i].Code, results[i].Detail = actionFailed, o.code, o.detail
		}
	}
	return &ImageUntagOutput{CacheControl: "no-store", Body: ImageUntagResponse{BulkID: bulkID, Results: results}}, nil
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

// prefixed returns each item with the prefix in front of it.
func prefixed(prefix string, items []string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = prefix + it
	}
	return out
}

// imageRemoveError maps the error of a removal or an untag to a result code and a sentence. An
// engine that says the image is gone is not_found, and one that says something uses it, which
// can happen when a container starts between the check and the call, is image_in_use and never
// a generic failure.
func imageRemoveError(ref string, err error) (code, detail string) {
	var busy *imageBusyError
	switch {
	case errors.As(err, &busy) && busy.lastName:
		return codeLastNameInUse, "This is the last name of an image that is in use. Remove what uses it first."
	case errors.As(err, &busy):
		return codeImageInUse, ref + " is still in use by " + joinMore(busy.users, maxNamedUsers) + ". Remove what uses it, then try again."
	case errors.Is(err, errNotDangling):
		return codeNotDangling, ref + " has a name or is kept, so it is not dangling and was left alone."
	case dockerx.IsNotFound(err), errors.Is(err, errImageGone), errors.Is(err, errNameGone):
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
