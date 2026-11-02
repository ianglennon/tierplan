// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// JanitorPreviewInput is the path of the preview route.
type JanitorPreviewInput struct {
	EnvironmentPathInput
}

// JanitorSummaryBody counts what a preview lists.
type JanitorSummaryBody struct {
	Containers int    `json:"containers" minimum:"0" doc:"How many containers the janitor would remove"`
	Networks   int    `json:"networks" minimum:"0" doc:"How many networks the janitor would remove"`
	Images     int    `json:"images" minimum:"0" doc:"How many images the janitor would remove"`
	EstBytes   *int64 `json:"estBytes" minimum:"0" doc:"The space the removable items hold, the sum of the sizes that are known. Null when no size is known, which is not zero: the estimate is unavailable"`
}

// JanitorReasonBody is why an item has its class.
type JanitorReasonBody struct {
	Code             string `json:"code" enum:"task_beyond_keep,task_service_gone,stack_gone,compose_project_gone,compose_oneoff,stopped_standalone,dangling_image,network_orphaned,network_unused,opt_in_off_containers,opt_in_off_networks,lone_compose_container,kept_label,kept_row,stack_excluded,stack_declared_keep,younger_than_grace,node_not_reporting,networks_withheld,stack_unknown,inspect_capped,clock_skew,task_labels_missing,services_unread" doc:"A stable word the SPA maps to its own copy"`
	Service          string `json:"service,omitempty" doc:"The swarm service of a task container"`
	Slot             string `json:"slot,omitempty" doc:"The replica slot of a replicated service's task container. Empty for a global service"`
	Project          string `json:"project,omitempty" doc:"The compose project the object is labelled with"`
	Stack            string `json:"stack,omitempty" doc:"The swarm stack the object is labelled with"`
	Node             string `json:"node,omitempty" doc:"The node a global service's group is on"`
	Rank             int    `json:"rank,omitempty" minimum:"0" doc:"The container's place among the exited containers of its group, 1 for the oldest"`
	AgeSeconds       int64  `json:"ageSeconds,omitempty" minimum:"0" doc:"How long the object has been stopped or idle"`
	RemainingSeconds int64  `json:"remainingSeconds,omitempty" minimum:"0" doc:"How much of the grace period is left, for an item held back by grace"`
}

// JanitorItemBody is one object the preview looked at.
type JanitorItemBody struct {
	Type       string            `json:"type" enum:"container,image,network"`
	NodeID     string            `json:"nodeId" doc:"The ID of the node the object is on"`
	Node       string            `json:"node" doc:"The node's name"`
	Ref        string            `json:"ref" doc:"The engine's ID of the object"`
	Name       string            `json:"name"`
	Class      string            `json:"class" enum:"removable,reported,protected,grace,cant_tell" doc:"removable: the janitor would remove it now. reported: worth a look, not removed by the policy. protected: kept by a label or a person. grace: stopped or idle for less than the grace period. cant_tell: TierPlan cannot be sure, so it leaves it"`
	Reason     JanitorReasonBody `json:"reason"`
	AgeSeconds int64             `json:"ageSeconds" minimum:"0" doc:"How long the object has been stopped or idle, 0 when not known"`
	SizeBytes  *int64            `json:"sizeBytes" minimum:"0" doc:"The bytes removing the object frees, null when not known"`
}

// JanitorPreviewBody is the answer of POST /api/env/{envId}/janitor/preview.
type JanitorPreviewBody struct {
	PreviewedAt      string             `json:"previewedAt" format:"date-time" doc:"When the preview was made"`
	NodesChecked     int                `json:"nodesChecked" minimum:"0" doc:"How many nodes reported and were checked"`
	Summary          JanitorSummaryBody `json:"summary"`
	Items            []JanitorItemBody  `json:"items" doc:"What the janitor would remove now, ordered by type (container, network, image), then node, name and ID"`
	NotRemoved       []JanitorItemBody  `json:"notRemoved" doc:"What the janitor looked at and left, with the class and reason, in the same order"`
	WithheldNodes    []string           `json:"withheldNodes" doc:"The nodes that did not report. None of their objects is listed, and nothing is judged that depends on them"`
	NetworksWithheld bool               `json:"networksWithheld" doc:"True when no network was judged because a node that could hold an attachment did not report"`
}

// JanitorPreviewOutput wraps JanitorPreviewBody for huma.
type JanitorPreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorPreviewBody
}

// registerJanitorPreview registers the dry run.
func registerJanitorPreview(r *httpx.Router, d JanitorDeps) {
	eng := &janitorEngine{agg: d.Aggregator, clusters: d.Clusters, log: d.log()}
	httpx.Register(r, huma.Operation{
		OperationID: "previewJanitor", Method: http.MethodPost, Path: "/api/env/{envId}/janitor/preview",
		Summary: "Preview what the janitor would remove",
		Description: "Answers which objects the janitor would remove from the environment now, and why it leaves the rest. " +
			"It reads every node fresh through the agents, judges with the environment's policy, and never writes: nothing is removed, nothing is audited, " +
			"and it neither takes nor waits for the environment's operation lock, so a deploy is never blocked by a preview. " +
			"Today it lists the exited swarm task containers beyond the newest the policy keeps for their service slot (their node, for a global service) " +
			"and past the grace period, measured from when the container stopped. A container labelled tierplan.keep=true, one whose task the swarm wants running, " +
			"one the planner cannot place, and one it could not inspect are listed under notRemoved with the reason. The nodes that did not report are named in withheldNodes " +
			"and none of their objects is listed. The server decides what is removable; the client only shows it. " +
			"Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, _ *JanitorPreviewInput) (*JanitorPreviewOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Aggregator == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := previewJanitor(ctx, d, eng, env.ID)
			if err != nil {
				return nil, err
			}
			return &JanitorPreviewOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// previewJanitor reads the environment, inspects the few containers that need it and builds the
// plan. The policy is the default until the policy store arrives behind the same call.
func previewJanitor(ctx context.Context, d JanitorDeps, eng janitor.Engine, envID string) (JanitorPreviewBody, error) {
	snap, err := eng.Snapshot(ctx, envID)
	if err != nil {
		return JanitorPreviewBody{}, snapshotProblem(d.log(), envID, err)
	}
	pol := janitor.DefaultPolicy()
	ends := janitor.InspectCandidates(ctx, eng, envID, janitor.Candidates(snap, pol))
	now := d.now()
	plan := janitor.Build(janitor.Input{Snapshot: snap, Ends: ends}, pol, now)
	return previewBody(plan, now), nil
}

// previewBody is the wire form of a plan.
func previewBody(plan janitor.Plan, now time.Time) JanitorPreviewBody {
	containers, networks, images, est, known := plan.Summary()
	sum := JanitorSummaryBody{Containers: containers, Networks: networks, Images: images}
	if known {
		sum.EstBytes = &est
	}
	return JanitorPreviewBody{
		PreviewedAt: rfc3339(now), NodesChecked: plan.NodesChecked, Summary: sum,
		Items: itemBodies(plan.Items), NotRemoved: itemBodies(plan.NotRemoved),
		WithheldNodes: plan.WithheldNodes, NetworksWithheld: plan.NetworksWithheld,
	}
}

// itemBodies maps plan items to their wire form; the result is never nil.
func itemBodies(items []janitor.Item) []JanitorItemBody {
	out := make([]JanitorItemBody, 0, len(items))
	for _, it := range items {
		b := JanitorItemBody{
			Type: string(it.Type), NodeID: it.NodeID, Node: it.Node, Ref: it.Ref, Name: it.Name, Class: string(it.Class),
			AgeSeconds: int64(it.Age.Seconds()),
			Reason: JanitorReasonBody{
				Code: string(it.Reason.Code), Service: it.Reason.Service, Slot: it.Reason.Slot, Project: it.Reason.Project,
				Stack: it.Reason.Stack, Node: it.Reason.Node, Rank: it.Reason.Rank,
				AgeSeconds: int64(it.Reason.Age.Seconds()), RemainingSeconds: int64(it.Reason.Remaining.Seconds()),
			},
		}
		if it.Size >= 0 {
			size := it.Size
			b.SizeBytes = &size
		}
		out = append(out, b)
	}
	return out
}
