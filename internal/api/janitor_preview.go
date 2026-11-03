// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"maps"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// maxJanitorPreviewBody bounds the body of a preview: one candidate policy of a few short fields.
const maxJanitorPreviewBody = 64 << 10

// JanitorPreviewRequest is the optional body of the preview route.
type JanitorPreviewRequest struct {
	Policy *JanitorPolicyFields          `json:"policy,omitempty" doc:"A candidate policy: the settings this environment would set itself if it were saved, exactly as a save would store them. A field that is null or left out follows the instance default, so the preview shows what a save of these fields would remove, whatever the environment has saved now. Omit it to preview the saved policy. Nothing is saved"`
	Nodes  map[string]JanitorNodeRequest `json:"nodes,omitempty" maxProperties:"500" doc:"Candidate settings of swarm nodes, by swarm node ID, each exactly as saving that node would store it. A node named here is judged by these settings instead of the ones it has saved (a field that is null or left out follows the environment); a node not named keeps its saved settings. Use it to preview switching a node on or lowering its keep-N before saving. Nothing is saved"`
}

// JanitorPreviewInput is the path and optional body of the preview route: a pointer, so that a request
// with no body previews the saved policy.
type JanitorPreviewInput struct {
	EnvironmentPathInput
	Body *JanitorPreviewRequest
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
	Code             string `json:"code" enum:"task_beyond_keep,task_service_gone,stack_gone,compose_project_gone,compose_oneoff,stopped_standalone,dangling_image,network_orphaned,network_unused,opt_in_off_containers,opt_in_off_networks,lone_compose_container,kept_label,kept_row,stack_excluded,stack_declared_keep,younger_than_grace,node_not_reporting,networks_withheld,stack_unknown,inspect_capped,clock_skew,task_labels_missing,services_unread,node_disabled" doc:"A stable word the SPA maps to its own copy"`
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
	eng := &janitorEngine{agg: d.Aggregator, clusters: d.Clusters, facts: d.StackFacts, keeps: janitor.KeepStore{St: d.Store}, log: d.log()}
	WithStackDeclarations(d.JanitorFacts)(eng)
	httpx.Register(r, huma.Operation{
		OperationID: "previewJanitor", Method: http.MethodPost, Path: "/api/env/{envId}/janitor/preview",
		Summary: "Preview what the janitor would remove",
		Description: "Answers which objects the janitor would remove from the environment now, and why it leaves the rest. " +
			"It reads every node fresh through the agents, judges with the environment's policy, and never writes: nothing is removed, nothing is audited, " +
			"and it neither takes nor waits for the environment's operation lock, so a deploy is never blocked by a preview. " +
			"It lists the exited swarm task containers beyond the newest the policy keeps for their service slot (their node, for a global service) " +
			"and past the grace period, measured from when the container stopped, and the networks the Address space page would list as orphaned, whose stack is gone, once they have been idle for the grace period. " +
			"A network no stack ever owned is listed under notRemoved with the reason opt_in_off_networks until the environment opts in to removing unused networks. " +
			"Containers that are not swarm tasks are judged too: the leftovers of a TierPlan stack that no longer exists (stack_gone) and the old one-offs of docker compose run beyond the newest the policy keeps (compose_oneoff) are removable once stopped for the grace period, " +
			"while a lone stopped container of a compose project TierPlan does not manage (lone_compose_container) and a stopped docker run container (opt_in_off_containers) are only reported until the environment opts in to removing stopped containers. " +
			"A container of a compose project that still has another container is never listed. On a swarm, a node whose janitor is switched off (its own setting) has its containers and images listed under notRemoved as protected, with the reason node_disabled, and a node's own keep-N replaces the environment's for the containers on it. A node whose engine clock differs from the server's by more than five minutes has its containers and images listed under notRemoved as clock_skew. " +
			"While a node is not reporting, or no manager answered, no network is judged: networksWithheld is true, the nodes are named in withheldNodes, and no network is listed as removable. A container labelled tierplan.keep=true, one whose task the swarm wants running, " +
			"one the planner cannot place, and one it could not inspect are listed under notRemoved with the reason. The nodes that did not report are named in withheldNodes " +
			"and none of their objects is listed. The server decides what is removable; the client only shows it. " +
			"The body is optional. With a candidate policy in it the preview judges by that policy instead of the saved one, so the page can show what a change would remove before it is saved or before the janitor is turned on: " +
			"the fields are those of the policy save, checked the same way (422 for a value it would refuse), and nothing is saved. " +
			"Needs environments.read on the environment.",
		Tags: []string{"environments"}, MaxBodyBytes: maxJanitorPreviewBody,
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorPreviewInput) (*JanitorPreviewOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || d.Aggregator == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			var candidate *janitor.Overrides
			var candidateNodes map[string]janitor.NodeOverrides
			if in.Body != nil && len(in.Body.Nodes) > 0 {
				candidateNodes = make(map[string]janitor.NodeOverrides, len(in.Body.Nodes))
				for id, n := range in.Body.Nodes {
					o := n.overrides()
					if err := janitor.ValidateNode(o); err != nil {
						if prob := janitorPolicyValueProblem(err); prob != nil {
							return nil, prob
						}
						return nil, janitorPolicyFailure(d, env.ID, "validate a candidate node", err)
					}
					candidateNodes[id] = o
				}
			}
			if in.Body != nil && in.Body.Policy != nil {
				o := in.Body.Policy.overrides()
				if err := janitor.Validate(o); err != nil {
					if prob := janitorPolicyValueProblem(err); prob != nil {
						return nil, prob
					}
					return nil, janitorPolicyFailure(d, env.ID, "validate the candidate", err)
				}
				candidate = &o
			}
			body, err := previewJanitor(ctx, d, eng, d.networks(), env.ID, candidate, candidateNodes)
			if err != nil {
				return nil, err
			}
			return &JanitorPreviewOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// previewJanitor reads the environment, inspects the few containers that need it, judges the
// networks and builds the plan. The policy is the one the runs follow, read through Runner.Policy, so
// the preview and a run judge alike, unless candidate is given: then it is the policy that saving
// candidate would leave, the instance defaults with those overrides. nets may be nil, and then no network
// is listed. A network check that fails withholds every network, as a node that does not answer does. The
// nodes' own settings are the saved ones, with those of candidateNodes (by swarm node ID) laid over them.
func previewJanitor(ctx context.Context, d JanitorDeps, eng janitor.Engine, nets janitor.Networks, envID string, candidate *janitor.Overrides, candidateNodes map[string]janitor.NodeOverrides) (JanitorPreviewBody, error) {
	snap, err := eng.Snapshot(ctx, envID)
	if err != nil {
		return JanitorPreviewBody{}, snapshotProblem(d.log(), envID, err)
	}
	pol, err := previewPolicy(ctx, d, envID, candidate)
	if err != nil {
		return JanitorPreviewBody{}, err
	}
	nodes, err := previewNodes(ctx, d, envID, candidateNodes)
	if err != nil {
		return JanitorPreviewBody{}, err
	}
	ends := janitor.InspectCandidates(ctx, eng, envID, janitor.CandidatesFor(snap, pol, nodes))
	in := janitor.Input{Snapshot: snap, Ends: ends, Nodes: nodes}
	if nets != nil {
		rep, err := nets.Report(ctx, envID)
		if err != nil {
			d.log().Warn("the janitor could not judge the networks of an environment", "environment", envID, "error", err)
			rep = janitor.NetworkReport{}
		}
		in.Networks = &rep
	}
	now := d.now()
	plan := janitor.Build(in, pol, now)
	return previewBody(plan, now), nil
}

// previewPolicy is the policy a preview judges by: the candidate over the instance defaults when there is
// one, else the policy the runs follow.
func previewPolicy(ctx context.Context, d JanitorDeps, envID string, candidate *janitor.Overrides) (janitor.Policy, error) {
	if candidate != nil {
		def := fallbackPolicy()
		if d.Policies.St != nil {
			p, _, _, err := d.Policies.Defaults(ctx)
			if err != nil {
				return janitor.Policy{}, err
			}
			def = p
		}
		return janitor.Effective(def, *candidate), nil
	}
	if d.Runner != nil && d.Runner.Policy != nil {
		return d.Runner.Policy(ctx, envID)
	}
	return fallbackPolicy(), nil
}

// previewNodes are the settings of the swarm nodes a preview judges by: the ones the runs follow, read through
// Runner.Nodes, with each node of candidate replaced by its candidate settings.
func previewNodes(ctx context.Context, d JanitorDeps, envID string, candidate map[string]janitor.NodeOverrides) (map[string]janitor.NodeOverrides, error) {
	var saved map[string]janitor.NodeOverrides
	var err error
	switch {
	case d.Runner != nil && d.Runner.Nodes != nil:
		saved, err = d.Runner.Nodes(ctx, envID)
	case d.Policies.St != nil:
		saved, err = d.Policies.Nodes(ctx, envID)
	}
	if err != nil {
		return nil, janitorPolicyFailure(d, envID, "read the policy of the nodes", err)
	}
	if len(candidate) == 0 {
		return saved, nil
	}
	merged := make(map[string]janitor.NodeOverrides, len(saved)+len(candidate))
	maps.Copy(merged, saved)
	maps.Copy(merged, candidate)
	return merged, nil
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
