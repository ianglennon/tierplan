// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The bounds the agent puts on a node's labels. They are counted in UTF-8 bytes, and the
// server's must not be looser than the agent's (plan 06-15), so a label the server accepts is
// one the agent forwards.
const (
	maxNodeLabels     = 64
	maxNodeLabelKey   = 256
	maxNodeLabelValue = 4096

	problemLabelReserved = "label_reserved"
	problemLabelInvalid  = "label_invalid"
)

// NodeLabelImpactRow is one placement constraint of one service that names a label the change
// touches.
type NodeLabelImpactRow struct {
	ServiceID  string `json:"serviceId" doc:"The service the constraint belongs to"`
	Service    string `json:"service" doc:"The service's name"`
	Constraint string `json:"constraint" doc:"The constraint as the service's spec writes it, such as node.labels.disk==ssd"`
	Before     bool   `json:"before" doc:"True when the node meets the constraint with the labels it has now"`
	After      bool   `json:"after" doc:"True when the node would meet the constraint with the labels asked for"`
}

// NodeLabelImpactBody is what a change of one node's labels will do to placement.
type NodeLabelImpactBody struct {
	Version         uint64               `json:"version" doc:"The node's object version as read for this judgement: the version the save must send back"`
	Rows            []NodeLabelImpactRow `json:"rows" doc:"Each placement constraint of a service that names a label the change adds, changes or removes, by service name then constraint. Never null"`
	NoLongerMatches int                  `json:"noLongerMatches" doc:"How many services have a constraint the node met and would no longer meet"`
	TasksMayMove    int                  `json:"tasksMayMove" doc:"How many tasks on the node belong to those services, which the swarm may move off it"`
	Added           int                  `json:"added" doc:"Labels the change adds"`
	Changed         int                  `json:"changed" doc:"Labels whose value the change changes"`
	Removed         int                  `json:"removed" doc:"Labels the change removes"`
}

// labelImpactInput is everything nodeLabelImpact judges.
type labelImpactInput struct {
	// Node is the node with the labels it has now.
	Node dockerx.NodeRow
	// Version is the node's object version at the read.
	Version uint64
	// Labels are the labels asked for, which replace the node's.
	Labels map[string]string
	// Services are the swarm's services.
	Services []dockerx.ServiceRow
	// Tasks are the node's tasks whose desired state is running.
	Tasks []dockerx.TaskRow
}

// reservedLabelPrefixes are the prefixes Docker keeps for itself. Docker accepts a node label that
// starts with one (RESEARCH E15), so TierPlan is the one that refuses it.
var reservedLabelPrefixes = []string{"com.docker.", "io.docker.", "org.dockerproject."}

// reservedPrefix is the reserved prefix a label key starts with, compared without regard to the
// case of ASCII letters, or empty when it starts with none.
func reservedPrefix(key string) string {
	for _, p := range reservedLabelPrefixes {
		if len(key) >= len(p) && strings.EqualFold(asciiOnly(key[:len(p)]), p) {
			return p
		}
	}
	return ""
}

// asciiOnly returns s when every byte is ASCII and otherwise a string that matches no prefix, so
// that the Unicode folding of EqualFold (the Kelvin sign is a K) never makes a key reserved.
func asciiOnly(s string) string {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return ""
		}
	}
	return s
}

// reservedLabel reports whether a label key is one Docker keeps for itself.
func reservedLabel(key string) bool { return reservedPrefix(key) != "" }

// labelChanges counts the labels after adds, changes the value of and drops from before. Keys
// and values are compared byte for byte.
func labelChanges(before, after map[string]string) (added, changed, removed int) {
	for k, v := range after {
		old, had := before[k]
		switch {
		case !had:
			added++
		case old != v:
			changed++
		}
	}
	for k := range before {
		if _, kept := after[k]; !kept {
			removed++
		}
	}
	return added, changed, removed
}

// checkNodeLabels refuses a label set the agent would not forward or Docker should not be given,
// before anything is read or written. Lengths are counted in UTF-8 bytes.
func checkNodeLabels(labels map[string]string) error {
	if len(labels) > maxNodeLabels {
		return httpx.Problem(http.StatusUnprocessableEntity, problemLabelInvalid, "A node can have at most "+strconv.Itoa(maxNodeLabels)+" labels.")
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		switch {
		case k == "":
			return httpx.Problem(http.StatusUnprocessableEntity, problemLabelInvalid, "A label needs a name.")
		case len(k) > maxNodeLabelKey:
			return httpx.Problem(http.StatusUnprocessableEntity, problemLabelInvalid, "A label name can be at most "+strconv.Itoa(maxNodeLabelKey)+" bytes.")
		case reservedLabel(k):
			return httpx.Problem(http.StatusUnprocessableEntity, problemLabelReserved, "Labels starting with "+reservedPrefix(k)+" are reserved.")
		case len(labels[k]) > maxNodeLabelValue:
			return httpx.Problem(http.StatusUnprocessableEntity, problemLabelInvalid, "A label value can be at most "+strconv.Itoa(maxNodeLabelValue)+" bytes.")
		}
	}
	return nil
}

// nodeLabelImpact judges a change of one node's labels against every service's placement
// constraints. A constraint is listed when it names a label (node.labels.NAME) the change adds,
// changes the value of or removes, with whether the node meets it before and after. Label names
// compare byte for byte and constraint values without regard to case, as the swarm does. A
// constraint that does not parse is left out: the swarm would refuse the service as it is.
func nodeLabelImpact(in labelImpactInput) NodeLabelImpactBody {
	b := NodeLabelImpactBody{Version: in.Version, Rows: []NodeLabelImpactRow{}}
	b.Added, b.Changed, b.Removed = labelChanges(in.Node.Labels, in.Labels)

	touched := map[string]bool{}
	for k, v := range in.Labels {
		if old, had := in.Node.Labels[k]; !had || old != v {
			touched[k] = true
		}
	}
	for k := range in.Node.Labels {
		if _, kept := in.Labels[k]; !kept {
			touched[k] = true
		}
	}
	if len(touched) == 0 {
		return b
	}

	before, after := envs.NodeFactsOf(in.Node), envs.NodeFactsOf(in.Node)
	before.Labels, after.Labels = in.Node.Labels, in.Labels
	const prefix = "node.labels."
	unmatched := map[string]bool{}
	for _, svc := range in.Services {
		for _, text := range svc.Constraints {
			c, err := envs.ParseConstraint(text)
			if err != nil || !strings.HasPrefix(c.Key, prefix) || !touched[strings.TrimPrefix(c.Key, prefix)] {
				continue
			}
			row := NodeLabelImpactRow{ServiceID: svc.ID, Service: cleanTaskText(svc.Name), Constraint: text, Before: c.Matches(before), After: c.Matches(after)}
			b.Rows = append(b.Rows, row)
			if row.Before && !row.After {
				unmatched[svc.ID] = true
			}
		}
	}
	slices.SortStableFunc(b.Rows, func(x, y NodeLabelImpactRow) int {
		return cmp.Or(cmp.Compare(x.Service, y.Service), cmp.Compare(x.Constraint, y.Constraint), cmp.Compare(x.ServiceID, y.ServiceID))
	})
	b.NoLongerMatches = len(unmatched)
	for _, tk := range in.Tasks {
		if tk.NodeID == in.Node.ID && unmatched[tk.ServiceID] {
			b.TasksMayMove++
		}
	}
	return b
}

// affectedServices is how many services have a constraint the change touches.
func affectedServices(b NodeLabelImpactBody) int {
	seen := map[string]bool{}
	for _, r := range b.Rows {
		seen[r.ServiceID] = true
	}
	return len(seen)
}

// labelImpactOf reads what nodeLabelImpact judges from a manager and judges it, and gives the
// node's host name for the audit row. The node's tasks are the ones meant to run; a node the
// swarm does not have is ErrNodeNotFound.
func labelImpactOf(ctx context.Context, d NodesDeps, envID, nodeID string, labels map[string]string) (NodeLabelImpactBody, string, error) {
	in, err := gatherImpact(ctx, d, envID, nodeID, impactLabels)
	if err != nil {
		return NodeLabelImpactBody{}, "", err
	}
	services := make([]dockerx.ServiceRow, 0, len(in.Services))
	for _, s := range in.Services {
		services = append(services, s)
	}
	return nodeLabelImpact(labelImpactInput{Node: in.Node, Version: in.Version, Labels: labels, Services: services, Tasks: in.Tasks}), cleanTaskText(in.Node.Hostname), nil
}

// NodeLabelImpactRequest is the labels a node would have.
type NodeLabelImpactRequest struct {
	Labels map[string]string `json:"labels" doc:"The node's labels as they would be saved; they replace the ones it has. At most 64, names of 1 to 256 bytes, values of at most 4096 bytes, and none starting with com.docker., io.docker. or org.dockerproject. (compared without regard to case)"`
}

// NodeLabelImpactInput is the path and body of a label impact.
type NodeLabelImpactInput struct {
	NodeInput
	Body NodeLabelImpactRequest
}

// NodeLabelImpactOutput wraps NodeLabelImpactBody for huma.
type NodeLabelImpactOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NodeLabelImpactBody
}

// NodeLabelsRequest is a save of a node's labels.
type NodeLabelsRequest struct {
	Labels  map[string]string `json:"labels" doc:"The node's labels as they are to be saved; they replace the ones it has, and an empty object removes them all. The limits are those of the impact check"`
	Version uint64            `json:"version" doc:"The node's version as the page last read it (the version of the impact); a node changed since is refused 409 node_changed"`
}

// NodeLabelsInput is the path and body of a label save.
type NodeLabelsInput struct {
	NodeInput
	Body NodeLabelsRequest
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// registerNodeLabels mounts the label impact and label save routes.
func registerNodeLabels(r *httpx.Router, d NodesDeps) {
	need := rbac.Requirement{Perm: rbac.PermNodesManage, Scope: rbac.ScopeEnvironment}
	const common = "422 label_reserved for a name starting with com.docker., io.docker. or org.dockerproject. (without regard to case; Docker itself accepts them), 422 label_invalid for a name of 0 or over 256 bytes, a value over 4096 bytes or more than 64 labels; " +
		"404 node_not_found when the swarm has no such node; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. Needs nodes.manage (Admin) on the environment."

	httpx.Register(r, huma.Operation{
		OperationID: "getNodeLabelImpact", Method: http.MethodPost, Path: "/api/env/{envId}/nodes/{nodeId}/labels/impact",
		Summary: "What a change of a node's labels will do to placement",
		Description: "Judges the labels a node would have, from a live read of the swarm, and writes nothing. Every placement constraint of a service that names a label the change adds, changes the value of or removes is listed " +
			"with whether the node meets it before and after (names compare byte for byte, values without regard to case). noLongerMatches counts the services with a constraint the node would stop meeting, and tasksMayMove the tasks on the node that belong to them. " +
			"version is the node's version, which the save must send. " + common,
		Tags: []string{"environments"},
	}, need, func(ctx context.Context, in *NodeLabelImpactInput) (*NodeLabelImpactOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if err := checkNodeLabels(in.Body.Labels); err != nil {
			return nil, err
		}
		if err := requireSwarm(ctx, d.services(), env.ID); err != nil {
			return nil, err
		}
		body, _, err := labelImpactOf(ctx, d, env.ID, in.NodeID, in.Body.Labels)
		if err != nil {
			return nil, nodeReadProblem(d, env.ID, err)
		}
		return &NodeLabelImpactOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "setNodeLabels", Method: http.MethodPut, Path: "/api/env/{envId}/nodes/{nodeId}/labels",
		Summary: "Replace a node's labels",
		Description: "Replaces the labels of one node, once. The server reads the node fresh and sends its whole spec with only the labels changed and the version sent, so nothing else about the node changes. " +
			"A node whose version is not the one sent is 409 node_changed and nothing is retried; Docker's own refusals are 502 node_update_failed with Docker's words. Whether a constraint would stop matching is the caller's to confirm: " +
			"TierPlan does not block it. Every attempt on a node that exists is audited as node.labels_changed with the name, how many labels were added, changed and removed, how many services are affected and the code. " + common,
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, need, func(ctx context.Context, in *NodeLabelsInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		return nil, setNodeLabels(ctx, d, p, env.ID, in)
	})
}

// setNodeLabels replaces the labels of one node. The labels are checked before anything is read,
// the node is read once for the audit counts, and the write is made once on a manager with the
// version it was given. Every attempt on a node that exists is audited.
func setNodeLabels(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, in *NodeLabelsInput) error {
	if err := checkNodeLabels(in.Body.Labels); err != nil {
		return err
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return err
	}
	impact, name, err := labelImpactOf(ctx, d, envID, in.NodeID, in.Body.Labels)
	rec := nodeLabelsAudit{}
	if err != nil {
		if errors.Is(err, dockerx.ErrNodeNotFound) {
			return nodeNotFound()
		}
		code, problem := nodeActionProblem(d, envID, err)
		rec.code = code
		auditNodeLabels(ctx, d, p, envID, in, rec)
		return problem
	}
	rec = nodeLabelsAudit{name: name, added: impact.Added, changed: impact.Changed, removed: impact.Removed, affected: affectedServices(impact)}

	// The write is sent once and seen through, so a browser that goes away while it runs does not
	// turn a change that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	update := dockerx.NodeChange{Version: in.Body.Version, Labels: in.Body.Labels, SetLabels: true}
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		return c.NodeUpdate(writeCtx, in.NodeID, update)
	})
	if err != nil {
		code, problem := nodeActionProblem(d, envID, err)
		rec.code = code
		auditNodeLabels(ctx, d, p, envID, in, rec)
		return problem
	}
	refreshClusters(ctx, d.services(), envID)
	rec.code = actionDone
	auditNodeLabels(ctx, d, p, envID, in, rec)
	return nil
}

// nodeLabelsAudit is what one attempt learned for its audit row.
type nodeLabelsAudit struct {
	name, code                        string
	added, changed, removed, affected int
}

// auditNodeLabels writes the row of one attempt: a success when the code is done, else a failure
// whose reason is the code. It is written on a context that outlives the request.
func auditNodeLabels(ctx context.Context, d NodesDeps, p *rbac.Principal, envID string, in *NodeLabelsInput, rec nodeLabelsAudit) {
	result, reason := audit.ResultSuccess, ""
	if rec.code != actionDone {
		result, reason = audit.ResultFailure, rec.code
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionNodeLabelsChanged, TargetType: nodeTargetType, TargetID: in.NodeID,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		Details: map[string]string{
			"name": rec.name, "added": strconv.Itoa(rec.added), "changed": strconv.Itoa(rec.changed),
			"removed": strconv.Itoa(rec.removed), "affected": strconv.Itoa(rec.affected), "code": rec.code,
		},
	}); err != nil {
		d.Log.Error("could not audit a node label change", "node", in.NodeID, "error", err)
	}
}
