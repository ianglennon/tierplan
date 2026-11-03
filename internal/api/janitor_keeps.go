// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/stacks"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Keeping anything out of cleanup (plan 11-27, JAN-07, JAN-06, D-12, D-21). A container's or an image's
// labels cannot be changed once it exists, so a keep row is the only way to protect one; it lives in the
// table the Address space page keeps its networks in, so a network kept on either page is kept on both.

// JanitorKeepTarget is one container, image or network to keep out of cleanup.
type JanitorKeepTarget struct {
	Kind   string `json:"kind" enum:"network,container,image" doc:"What is kept"`
	NodeID string `json:"nodeId,omitempty" maxLength:"128" doc:"The node the container or image is on: the nodeId of its row. For a local network, the node it is on; left out for a swarm-wide network, and when only one node has it"`
	Ref    string `json:"ref" minLength:"1" maxLength:"256" doc:"The container's ID, the image's ID, or the network's name (a keep follows the name, so it holds for a network of that name made again)"`
}

// JanitorKeepRequest is the body of PUT /api/env/{envId}/janitor/keeps.
type JanitorKeepRequest struct {
	Targets []JanitorKeepTarget `json:"targets" minItems:"1" maxItems:"100" doc:"The containers, images and networks to keep, 1 to 100"`
}

// JanitorKeepInput is the path and body of the keep route.
type JanitorKeepInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      JanitorKeepRequest
}

// JanitorKeepBody is the answer of the keep route.
type JanitorKeepBody struct {
	Kept int `json:"kept" minimum:"0" doc:"How many objects are newly kept. One that was kept already changes and records nothing and is not counted"`
}

// JanitorKeepOutput wraps JanitorKeepBody for huma.
type JanitorKeepOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorKeepBody
}

// JanitorUnkeepInput is the query of the stop-keeping route.
type JanitorUnkeepInput struct {
	EnvironmentPathInput
	Kind   string `query:"kind" required:"true" enum:"network,container,image" doc:"What is kept"`
	NodeID string `query:"nodeId" maxLength:"128" doc:"The node the keep is for: the nodeId of its row. Left out for a swarm-wide network, and when only one node has the object"`
	Ref    string `query:"ref" required:"true" minLength:"1" maxLength:"256" doc:"The container's ID, the image's ID, or the network's name"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// JanitorKeptItem is one thing kept out of cleanup.
type JanitorKeptItem struct {
	Kind       string `json:"kind" enum:"network,container,image"`
	NodeID     string `json:"nodeId" doc:"The node the keep is for: the node's ID for a container, an image or a local network, empty for a swarm-wide network"`
	Node       string `json:"node" doc:"The node's name, Cluster for a swarm-wide network, empty for a keep whose object is gone and whose node is not known"`
	Ref        string `json:"ref" doc:"The container's ID, the image's ID, or the network's name"`
	Name       string `json:"name" doc:"What to call it: the container's name, the image's first tag or short ID, the network's name"`
	KeptBy     string `json:"keptBy" enum:"label,user" doc:"label: it carries the tierplan.keep=true label, which only its owner can remove. user: a person kept it"`
	KeptByName string `json:"keptByName" doc:"The display name of the person who kept it; empty when kept by label or when that person is not known"`
	KeptAt     string `json:"keptAt,omitempty" format:"date-time" doc:"When it was kept; absent when kept by label alone"`
	Present    bool   `json:"present" doc:"True when the object exists now. A keep outlives its object, and a network of the same name made again is still kept"`
}

// JanitorKeptBody is the answer of the kept list.
type JanitorKeptBody struct {
	Items []JanitorKeptItem `json:"items" doc:"Every keep of the environment and every container, image and network that carries the keep label, by kind, name and node"`
}

// JanitorKeptOutput wraps JanitorKeptBody for huma.
type JanitorKeptOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         JanitorKeptBody
}

// registerJanitorKeeps registers the janitor's keep routes.
func registerJanitorKeeps(r *httpx.Router, d JanitorDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "keepJanitorItems", Method: http.MethodPut, Path: "/api/env/{envId}/janitor/keeps",
		Summary: "Keep containers, images and networks out of cleanup",
		Description: "Stores a keep for each target (1 to 100) in one transaction: the janitor never removes a kept container, image or network, by a schedule or by Clean now. " +
			"Each target is checked against a fresh read of the environment; 404 not_found when one is not there. " +
			"An object that is kept already changes and records nothing. Each new keep is audited as janitor.kept. " +
			"422 node_required when a container, an image or a local network is on more than one node and no nodeId is given. " +
			"The keeps are the table the Address space page keeps networks in, so a network kept here is kept there. " +
			"Needs janitor.run (Env Admin) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermJanitorRun, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorKeepInput) (*JanitorKeepOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || !d.keepsReady() {
				return nil, huma.Error500InternalServerError("internal error")
			}
			n, err := keepJanitorItems(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &JanitorKeepOutput{CacheControl: "no-store", Body: JanitorKeepBody{Kept: n}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "unkeepJanitorItem", Method: http.MethodDelete, Path: "/api/env/{envId}/janitor/keeps",
		Summary: "Stop keeping a container, image or network out of cleanup",
		Description: "Removes the keep of one object (kind, nodeId and ref in the query), audited as janitor.unkept. " +
			"404 keep_not_found when it is not kept by a person. 409 kept_by_label when it is kept by the tierplan.keep=true label on it, which only its owner can remove. " +
			"422 node_required when the same ref is kept on more than one node and no nodeId is given. " +
			"Needs janitor.run (Env Admin) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermJanitorRun, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *JanitorUnkeepInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil || !d.keepsReady() {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, unkeepJanitorItem(ctx, d, p, env.ID, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "listJanitorKeeps", Method: http.MethodGet, Path: "/api/env/{envId}/janitor/keeps",
		Summary: "List what is kept out of cleanup",
		Description: "Every keep of the environment, whether or not its object exists now, and every container, image and network that carries the tierplan.keep=true label (keptBy label). " +
			"An object that has both is one item. Needs environments.read on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermEnvironmentsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*JanitorKeptOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil || !d.keepsReady() {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := listJanitorKeeps(ctx, d, env.ID)
			if err != nil {
				return nil, err
			}
			return &JanitorKeptOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// keepsReady says whether the deps carry what the keep routes use.
func (d JanitorDeps) keepsReady() bool {
	return d.Aggregator != nil && d.Store != nil && d.Audit != nil
}

// keepInternalError logs a failure of the store and answers it as a failed request.
func (d JanitorDeps) keepInternalError(what, envID string, err error) error {
	d.log().Error("could not change what is kept out of cleanup", "step", what, "environment", envID, "error", err)
	return huma.Error500InternalServerError("internal error")
}

// keepRef is one thing kept, as the keep table keys it, with what to call it.
type keepRef struct {
	kind         janitor.Kind
	nodeID, ref  string
	name, nodeNm string
}

func (k keepRef) key() janitor.KeepKey {
	return janitor.KeepKey{Kind: k.kind, NodeID: k.nodeID, Ref: k.ref}
}

// imageName is what an image is called: its first tag, else its short ID.
func imageName(img envs.ImageRow) string {
	for _, t := range img.RepoTags {
		if !isNoneTag(t) {
			return t
		}
	}
	id := strings.TrimPrefix(img.ID, "sha256:")
	return id[:min(len(id), 12)]
}

// resolveKeep finds the object a target names in a fresh read of the environment. A missing one is a
// 404 not_found; one that more than one node has, with no node named, is a 422 node_required.
func resolveKeep(agg envs.Aggregate, t JanitorKeepTarget) (keepRef, error) {
	notThere := httpx.Problem(http.StatusNotFound, codeNotFound, "That "+t.Kind+" is not there any more.")
	var found []keepRef
	switch janitor.Kind(t.Kind) {
	case janitor.KindContainer:
		for _, c := range agg.Containers {
			if c.ID == t.Ref && (t.NodeID == "" || c.NodeID == t.NodeID) {
				found = append(found, keepRef{kind: janitor.KindContainer, nodeID: c.NodeID, ref: c.ID, name: strings.TrimPrefix(c.Name, "/"), nodeNm: c.Node})
			}
		}
	case janitor.KindImage:
		for _, img := range agg.Images {
			if img.ID == t.Ref && (t.NodeID == "" || img.NodeID == t.NodeID) {
				found = append(found, keepRef{kind: janitor.KindImage, nodeID: img.NodeID, ref: img.ID, name: imageName(img), nodeNm: img.Node})
			}
		}
	case janitor.KindNetwork:
		node, _, err := keepNode(agg, t.Ref, t.NodeID)
		if err != nil {
			return keepRef{}, err
		}
		for _, n := range agg.Networks {
			if n.Name == t.Ref && envs.KeepNodeKey(n) == node {
				return keepRef{kind: janitor.KindNetwork, nodeID: node, ref: n.Name, name: n.Name, nodeNm: n.Node}, nil
			}
		}
		return keepRef{}, notThere
	default:
		return keepRef{}, notThere
	}
	switch len(found) {
	case 0:
		return keepRef{}, notThere
	case 1:
		return found[0], nil
	}
	return keepRef{}, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
		"More than one node has that "+t.Kind+". Say which one (nodeId).")
}

// keepAuditEntry is the audit entry of a keep or an unkeep. The target is the object's kind and its ref.
func keepAuditEntry(ctx context.Context, p *rbac.Principal, envID, userAgent, action string, k keepRef, bulk bool) audit.Entry {
	details := map[string]string{"kind": string(k.kind), "name": k.name}
	if k.nodeID != "" {
		details["node"] = cmp.Or(k.nodeNm, k.nodeID)
	}
	if bulk {
		details["bulk"] = "true"
	}
	return audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: string(k.kind), TargetID: k.ref, EnvID: envID,
		Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}
}

// keepJanitorItems stores a keep for each target not kept already. The rows and their audit entries are one
// transaction: a request that changes nothing writes nothing.
func keepJanitorItems(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID string, in *JanitorKeepInput) (int, error) {
	agg, _, err := d.Aggregator.FreshSnapshot(ctx, envID)
	if err != nil {
		return 0, snapshotProblem(d.log(), envID, err)
	}
	var targets []keepRef
	seen := map[janitor.KeepKey]bool{}
	for _, t := range in.Body.Targets {
		k, err := resolveKeep(agg, t)
		if err != nil {
			return 0, err
		}
		if !seen[k.key()] {
			seen[k.key()] = true
			targets = append(targets, k)
		}
	}

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return 0, d.keepInternalError("begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	q := d.Store.Q.WithTx(tx)
	kept, networks := 0, false
	for _, k := range targets {
		n, err := q.InsertJanitorKeep(ctx, sqlc.InsertJanitorKeepParams{
			EnvID: envID, Kind: string(k.kind), NodeID: k.nodeID, Ref: k.ref, Name: k.name, KeptBy: p.ID, KeptAt: d.now().UTC().UnixNano(),
		})
		if err != nil {
			return 0, d.keepInternalError("keep", envID, err)
		}
		if n == 0 {
			continue // kept already: the first keep stands
		}
		if err := d.Audit.RecordTx(ctx, tx, keepAuditEntry(ctx, p, envID, in.UserAgent, audit.ActionJanitorKept, k, len(targets) > 1)); err != nil {
			return 0, d.keepInternalError("audit", envID, err)
		}
		kept++
		networks = networks || k.kind == janitor.KindNetwork
	}
	if kept == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, d.keepInternalError("commit", envID, err)
	}
	if networks && d.AddressSpaces != nil {
		d.AddressSpaces.ForgetOrphans(envID)
	}
	return kept, nil
}

// labelled says whether an object of the kind, ref and node carries the keep label in the read.
func labelled(agg envs.Aggregate, kind janitor.Kind, nodeID, ref string) bool {
	switch kind {
	case janitor.KindContainer:
		return slices.ContainsFunc(agg.Containers, func(c envs.ContainerRow) bool {
			return c.ID == ref && (nodeID == "" || c.NodeID == nodeID) && c.Labels[stacks.LabelKeep] == "true"
		})
	case janitor.KindImage:
		return slices.ContainsFunc(agg.Images, func(img envs.ImageRow) bool {
			return img.ID == ref && (nodeID == "" || img.NodeID == nodeID) && img.Labels[stacks.LabelKeep] == "true"
		})
	case janitor.KindNetwork:
		return slices.ContainsFunc(agg.Networks, func(n envs.NetworkRow) bool {
			return n.Name == ref && (nodeID == "" || envs.KeepNodeKey(n) == nodeID || n.Scope == dockerx.ScopeSwarm) && n.Labels[stacks.LabelKeep] == "true"
		})
	}
	return false
}

// unkeepJanitorItem removes the keep row of one object. An object kept only by its label has no row to
// remove, and is told so.
func unkeepJanitorItem(ctx context.Context, d JanitorDeps, p *rbac.Principal, envID string, in *JanitorUnkeepInput) error {
	kind := janitor.Kind(in.Kind)
	rows, err := d.Store.RQ.ListJanitorKeeps(ctx, envID)
	if err != nil {
		return d.keepInternalError("list", envID, err)
	}
	var match []sqlc.ListJanitorKeepsRow
	for _, r := range rows {
		if r.Kind == in.Kind && r.Ref == in.Ref && (in.NodeID == "" || r.NodeID == in.NodeID) {
			match = append(match, r)
		}
	}
	switch len(match) {
	case 0:
		// Not kept by a person. If its label keeps it, say whose keep that is.
		agg, _, err := d.Aggregator.Snapshot(ctx, envID)
		if err != nil {
			return snapshotProblem(d.log(), envID, err)
		}
		if labelled(agg, kind, in.NodeID, in.Ref) {
			return httpx.Problem(http.StatusConflict, problemKeptByLabel,
				"Kept by the tierplan.keep label on the "+in.Kind+". Remove the label to stop keeping it.")
		}
		return httpx.Problem(http.StatusNotFound, problemKeepNotFound, "That "+in.Kind+" is not kept.")
	case 1:
	default:
		return httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
			"More than one node keeps that "+in.Kind+". Say which one (nodeId).")
	}
	row := match[0]
	k := keepRef{kind: kind, nodeID: row.NodeID, ref: row.Ref, name: cmp.Or(row.Name, row.Ref), nodeNm: d.nodeName(ctx, envID, row.NodeID)}

	tx, err := d.Store.Write.BeginTx(ctx, nil)
	if err != nil {
		return d.keepInternalError("begin", envID, err)
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed
	n, err := d.Store.Q.WithTx(tx).DeleteJanitorKeep(ctx, sqlc.DeleteJanitorKeepParams{EnvID: envID, Kind: row.Kind, NodeID: row.NodeID, Ref: row.Ref})
	if err != nil {
		return d.keepInternalError("unkeep", envID, err)
	}
	if n == 0 {
		return httpx.Problem(http.StatusNotFound, problemKeepNotFound, "That "+in.Kind+" is not kept.") // taken back by someone else meanwhile
	}
	if err := d.Audit.RecordTx(ctx, tx, keepAuditEntry(ctx, p, envID, in.UserAgent, audit.ActionJanitorUnkept, k, false)); err != nil {
		return d.keepInternalError("audit", envID, err)
	}
	if err := tx.Commit(); err != nil {
		return d.keepInternalError("commit", envID, err)
	}
	if kind == janitor.KindNetwork && d.AddressSpaces != nil {
		d.AddressSpaces.ForgetOrphans(envID)
	}
	return nil
}

// nodeName is the name of a node of the environment as the last read has it, empty when the node is not
// known (a keep outlives its node).
func (d JanitorDeps) nodeName(ctx context.Context, envID, nodeID string) string {
	if nodeID == "" {
		return ""
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return ""
	}
	for _, n := range agg.Nodes {
		if n.ID == nodeID {
			return n.Name
		}
	}
	return ""
}

// listJanitorKeeps is the kept part of the Janitor page: every keep row, whether or not its object exists
// now, and every container, image and network that carries the keep label. An object with both is one item.
func listJanitorKeeps(ctx context.Context, d JanitorDeps, envID string) (JanitorKeptBody, error) {
	rows, err := d.Store.RQ.ListJanitorKeeps(ctx, envID)
	if err != nil {
		return JanitorKeptBody{}, d.keepInternalError("list", envID, err)
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return JanitorKeptBody{}, snapshotProblem(d.log(), envID, err)
	}
	nodes := make(map[string]string, len(agg.Nodes))
	for _, n := range agg.Nodes {
		nodes[n.ID] = n.Name
	}

	items := make([]JanitorKeptItem, 0, len(rows))
	at := make(map[janitor.KeepKey]int, len(rows))
	keeper := make(map[int]string, len(rows)) // item index -> the user ID that kept it
	for _, r := range rows {
		k := janitor.KeepKey{Kind: janitor.Kind(r.Kind), NodeID: r.NodeID, Ref: r.Ref}
		at[k] = len(items)
		items = append(items, JanitorKeptItem{
			Kind: r.Kind, NodeID: r.NodeID, Node: nodes[r.NodeID], Ref: r.Ref, Name: cmp.Or(r.Name, r.Ref), KeptBy: "user",
			KeptAt: time.Unix(0, r.KeptAt).UTC().Format(time.RFC3339),
		})
		keeper[len(items)-1] = r.KeptBy
	}
	// What exists now: present keeps get the object's current name and node, and an object that carries the
	// label is kept by it whether or not a person kept it too.
	see := func(kind janitor.Kind, nodeID, ref, name, node string, label bool) {
		k := janitor.KeepKey{Kind: kind, NodeID: nodeID, Ref: ref}
		i, have := at[k]
		if !have && !label {
			return
		}
		if !have {
			i = len(items)
			at[k] = i
			items = append(items, JanitorKeptItem{Kind: string(kind), NodeID: nodeID, Ref: ref})
		}
		it := &items[i]
		it.Name, it.Node, it.Present = name, node, true
		if label {
			it.KeptBy = "label"
		} else if it.KeptBy == "" {
			it.KeptBy = "user"
		}
	}
	for _, c := range agg.Containers {
		see(janitor.KindContainer, c.NodeID, c.ID, strings.TrimPrefix(c.Name, "/"), c.Node, c.Labels[stacks.LabelKeep] == "true")
	}
	for _, img := range agg.Images {
		see(janitor.KindImage, img.NodeID, img.ID, imageName(img), img.Node, img.Labels[stacks.LabelKeep] == "true")
	}
	seenNet := map[janitor.KeepKey]bool{}
	for _, n := range agg.Networks {
		k := janitor.KeepKey{Kind: janitor.KindNetwork, NodeID: envs.KeepNodeKey(n), Ref: n.Name}
		if seenNet[k] {
			continue
		}
		seenNet[k] = true
		see(janitor.KindNetwork, k.NodeID, n.Name, n.Name, n.Node, n.Labels[stacks.LabelKeep] == "true")
	}

	names := map[string]string{}
	for i, id := range keeper {
		if items[i].KeptBy != "label" {
			items[i].KeptByName = userDisplayName(ctx, d.Store, names, id)
		}
	}
	kindRank := func(k string) int {
		switch janitor.Kind(k) {
		case janitor.KindContainer:
			return 0
		case janitor.KindImage:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(items, func(a, b JanitorKeptItem) int {
		return cmp.Or(cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)), strings.Compare(a.Name, b.Name), strings.Compare(a.Node, b.Node), strings.Compare(a.Ref, b.Ref))
	})
	return JanitorKeptBody{Items: items}, nil
}

// userDisplayName is the display name of a user, else the username, else empty when the user is not known.
func userDisplayName(ctx context.Context, st *store.Store, cache map[string]string, userID string) string {
	if name, ok := cache[userID]; ok {
		return name
	}
	name := ""
	if st != nil {
		if u, err := st.RQ.GetUserByID(ctx, userID); err == nil {
			name = cmp.Or(u.DisplayName, u.Username)
		}
	}
	cache[userID] = name
	return name
}
