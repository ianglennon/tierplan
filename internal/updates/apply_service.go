// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/registry"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// The update of a plain swarm service (plan 12-15, D-07, D-12, D-13). The service is read at its current version, one field of
// it changes (TaskTemplate.ContainerSpec.Image) and the whole spec is sent back, so the person's update_config and every other
// setting are kept. The image is pulled on the nodes that run the service's tasks first, so a pull that fails ends the update
// before the service is touched.

const (
	// pullTimeout is how long the pull on one node may take.
	pullTimeout = 30 * time.Minute

	// pollReadTimeout bounds one read of a service and its tasks while it is waited for.
	pollReadTimeout = 20 * time.Second

	// credentialTimeout bounds a write of a credential's bookkeeping after a pull.
	credentialTimeout = 10 * time.Second
)

// itemMessages are the sentences of the codes the update puts on a step. A code of a pull has its own, from stackops.
var itemMessages = map[string]string{
	ReasonPullFailed:     "The image could not be pulled, so nothing was changed.",
	ReasonDidntStart:     "The new version did not start.",
	ReasonTimeout:        "The service did not finish updating in time.",
	ReasonUnhealthy:      "The new version did not become healthy.",
	ReasonServiceChanged: "The service was changed by someone else while it was being updated. Nothing was changed.",
	ReasonRejected:       "The swarm refused the new version. Nothing was changed.",
	ReasonEngine:         "The swarm could not be reached.",
	ReasonCancelled:      "The update was cancelled.",
	"network_full":       "A network the service needs has no free address left.",

	// The reasons and codes of a container's update (plan 12-20).
	ReasonCreateRefused:    "The new container could not be created, so nothing was changed.",
	ReasonAgentHostPolicy:  "The node's agent doesn't let TierPlan create containers with host access. Nothing was changed.",
	ReasonContainerChanged: "The container was changed or removed by someone else while it was being updated. Nothing was changed.",
	ReasonRestoreFailed:    "The new container failed and the old one could not be put back.",
	"auto_remove":          "A container started with automatic removal is removed when it stops, so it can't be put back if the update fails. Nothing was changed.",
	"remove_old_failed":    "The new container is running. The old one could not be removed; it is stopped and keeps its temporary name.",
}

// updateService does the update of one plain service and ends the run through end. end is called once, before the last step of
// the job is finished, and frees the environment's lock.
func (a *Applier) updateService(ctx context.Context, r *jobs.Reporter, t target, end func(RunState, string)) {
	// order is the steps still to come, so a failure can end the rest as skipped.
	order := []string{ItemPull, ItemUpdate, ItemWait}
	fail := func(item, code, reason string) {
		end(RunFailed, reason)
		msg := itemMessages[code]
		if msg == "" {
			msg = itemMessages[reason]
		}
		if m := stackops.CodeMessage(code); m != "" {
			msg = m
		}
		r.Finish(item, jobs.ItemFailed, code, msg, nil)
		skip := false
		for _, id := range append(order, ItemHealth) {
			if id == item {
				skip = true
				continue
			}
			if skip {
				r.Finish(id, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
			}
		}
	}
	if ctx.Err() != nil {
		fail(ItemPull, ReasonCancelled, ReasonCancelled)
		return
	}

	// The pull, on the nodes the service's tasks run on.
	r.Update(ItemPull, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if code, reason := a.pullOnTaskNodes(ctx, r, t); code != "" {
		fail(ItemPull, code, reason)
		return
	}

	// The spec, read at its current version with only the image changed.
	if ctx.Err() != nil {
		fail(ItemPull, ReasonCancelled, ReasonCancelled)
		return
	}
	r.Update(ItemUpdate, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	var detail dockerx.ServiceDetail
	if err := a.Service.Clusters.ClusterRead(ctx, t.envID, func(c *dockerx.Client) error {
		var e error
		detail, _, e = c.ServiceInspect(ctx, t.id)
		return e
	}); err != nil {
		a.log().Warn("could not read a service to update it", "environment", t.envID, "service", t.name, "error", err)
		if errors.Is(err, dockerx.ErrServiceNotFound) {
			fail(ItemUpdate, ReasonServiceChanged, ReasonServiceChanged)
			return
		}
		fail(ItemUpdate, ReasonEngine, ReasonEngine)
		return
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(detail.Spec, &spec); err != nil || spec.TaskTemplate.ContainerSpec == nil {
		a.log().Warn("a service has no container spec to update", "environment", t.envID, "service", t.name)
		fail(ItemUpdate, ReasonRejected, ReasonRejected)
		return
	}
	spec.TaskTemplate.ContainerSpec.Image = t.ref
	hasHealth := hasHealthcheck(spec.TaskTemplate.ContainerSpec)
	if hasHealth {
		r.Add(jobs.ItemSpec{ID: ItemHealth, Label: "Wait for a healthy check", Kind: stackops.KindStep})
		order = append(order, ItemHealth)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		fail(ItemUpdate, ReasonRejected, ReasonRejected)
		return
	}

	// Fresh credentials for the nodes that pull the image: a header is never stored, and when no credential applies the
	// write says so (registryAuthFrom=spec) instead of inventing one.
	auth, used, err := a.authFor(ctx, t)
	if err != nil {
		a.log().Error("could not resolve the registry credential of a service update", "environment", t.envID, "service", t.name, "error", err)
		fail(ItemUpdate, stackops.CodeRegistryUnresolved, ReasonEngine)
		return
	}
	werr := a.Aggregator.WriteOnManager(context.WithoutCancel(ctx), t.envID, func(c *dockerx.Client) error {
		var e error
		if auth == "" {
			_, e = c.ServiceUpdateAuthFromSpec(context.WithoutCancel(ctx), detail.ID, detail.Version, raw)
		} else {
			_, e = c.ServiceUpdate(context.WithoutCancel(ctx), detail.ID, detail.Version, raw, auth)
		}
		return e
	})
	if used != nil {
		a.noteCredential(ctx, used, "")
	}
	if werr != nil {
		code := ReasonEngine
		var rejected *dockerx.SpecRejectedError
		switch {
		case errors.Is(werr, dockerx.ErrServiceChanged), errors.Is(werr, dockerx.ErrServiceNotFound):
			code = ReasonServiceChanged
		case errors.As(werr, &rejected):
			code = ReasonRejected
		}
		a.log().Warn("a service update was not accepted", "environment", t.envID, "service", t.name, "reason", code, "error", werr)
		fail(ItemUpdate, code, code)
		return
	}
	r.Output("Updated " + t.name + " to " + shortDigest(t.to))
	r.Finish(ItemUpdate, jobs.ItemDone, "", "", map[string]string{"from": t.from, "to": t.to})

	// The wait for the new tasks (and, when the image has a health check, for them to be healthy: the swarm keeps a task in
	// starting until its check passes).
	r.Update(ItemWait, func(it *jobs.Item) { it.State = jobs.ItemRunning; it.Unit = "tasks" })
	if hasHealth {
		r.Update(ItemHealth, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	}
	var last stackops.Convergence
	werr = stackops.WaitConverged(ctx, a.clock(), a.convergeTimeout(), func(ctx context.Context) (stackops.Convergence, []dockerx.TaskRow, error) {
		rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
		defer cancel()
		var (
			d    dockerx.ServiceDetail
			rows []dockerx.TaskRow
		)
		if err := a.Service.Clusters.ClusterRead(rctx, t.envID, func(c *dockerx.Client) error {
			var e error
			if d, _, e = c.ServiceInspect(rctx, detail.ID); e != nil {
				return e
			}
			rows, e = c.Tasks(rctx, dockerx.TaskFilter{ServiceIDs: []string{d.ID}})
			return e
		}); err != nil {
			return stackops.Convergence{}, nil, err
		}
		return stackops.Progress(d, rows).IgnoringUpdate(detail.UpdateStatus), rows, nil
	}, nil, func(c stackops.Convergence, _ []dockerx.TaskRow) {
		last = c
		r.Update(ItemWait, func(it *jobs.Item) { it.Current, it.Total = int64(c.Running), int64(c.Want) })
	})

	switch {
	case werr == nil:
		end(RunDone, "")
		r.Finish(ItemWait, jobs.ItemDone, "", "", map[string]string{"running": strconv.Itoa(last.Running)})
		if hasHealth {
			r.Finish(ItemHealth, jobs.ItemDone, "", "", nil)
		}
	case ctx.Err() != nil:
		fail(ItemWait, ReasonCancelled, ReasonCancelled)
	default:
		reason, code := convergeReason(werr, last)
		a.log().Warn("a service update did not settle", "environment", t.envID, "service", t.name, "reason", reason, "error", werr)
		item := ItemWait
		if reason == ReasonUnhealthy && hasHealth {
			item = ItemHealth
		}
		fail(item, code, reason)
		if text := firstText(last.DeadErr, last.Err, last.Message); text != "" {
			r.Detail(item, text)
		}
	}
}

// convergeReason words how a wait that did not settle ended: the reason the run keeps and the code of the step.
func convergeReason(err error, last stackops.Convergence) (reason, code string) {
	switch {
	case errors.Is(err, stackops.ErrConvergeTimeout):
		return ReasonTimeout, ReasonTimeout
	case errors.Is(err, stackops.ErrNetworkFull):
		return ReasonDidntStart, "network_full"
	case errors.Is(err, stackops.ErrUpdateStopped), errors.Is(err, stackops.ErrTasksExhausted):
		if strings.Contains(strings.ToLower(last.DeadErr+" "+last.Err+" "+last.Message), "unhealthy") {
			return ReasonUnhealthy, ReasonUnhealthy
		}
		return ReasonDidntStart, ReasonDidntStart
	}
	return ReasonEngine, ReasonEngine
}

// firstText is the first of the texts that says anything.
func firstText(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

// hasHealthcheck reports whether the container spec defines a health check that runs.
func hasHealthcheck(cs *swarm.ContainerSpec) bool {
	h := cs.Healthcheck
	return h != nil && len(h.Test) > 0 && h.Test[0] != "NONE"
}

// authFor is the registry header the update carries, and the credential it was made from; "" and nil when no credential
// applies.
func (a *Applier) authFor(ctx context.Context, t target) (string, *registry.Candidate, error) {
	if a.Resolver == nil {
		return "", nil, nil
	}
	ref, err := registry.ParseRef(t.ref)
	if err != nil {
		return "", nil, err
	}
	return a.Resolver.AuthFor(ctx, t.envID, ref)
}

// noteCredential keeps the credential's bookkeeping after it was used: a registry that refused the sign-in records the failure
// with its code, and a pull that worked clears it. A failed write is logged and changes nothing else.
func (a *Applier) noteCredential(ctx context.Context, used *registry.Candidate, code string) {
	if used == nil || a.Resolver == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), credentialTimeout)
	defer cancel()
	var err error
	switch code {
	case dockerx.PullCodeUnauthorized:
		err = a.Resolver.RecordFailure(wctx, used.ID, code)
	case "":
		err = a.Resolver.ClearFailure(wctx, used.ID)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		a.log().Error("could not update a credential's last failure", "credential", used.ID, "error", err)
	}
}

// pullOnTaskNodes pulls the new image on each node that holds a task the swarm means to run, one node at a time, with the
// credential the environment resolves for it. It answers the code of the step and the reason of the run when a pull failed, and
// empty strings when every pull worked or there was nothing to pull on: a node nothing reaches is left to the swarm, which pulls
// the image itself where the task lands. Nothing is retried.
func (a *Applier) pullOnTaskNodes(ctx context.Context, r *jobs.Reporter, t target) (code, reason string) {
	nodes, err := a.taskNodes(ctx, t)
	if err != nil {
		a.log().Warn("could not find the nodes a service runs on to pull its new image there", "environment", t.envID, "service", t.name, "error", err)
		// The swarm pulls the image itself where the tasks land, so the step is not run and the update goes on.
		r.Finish(ItemPull, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		return "", ""
	}
	if len(nodes) == 0 {
		r.Finish(ItemPull, jobs.ItemDone, "", "", map[string]string{"nodes": "0"})
		return "", ""
	}
	header, used, aerr := a.authFor(ctx, t)
	if aerr != nil {
		a.log().Error("could not resolve the registry credential of a service image", "environment", t.envID, "service", t.name, "error", aerr)
		return stackops.CodeRegistryUnresolved, ReasonPullFailed
	}
	index := map[string]int{}
	for _, node := range nodes {
		if code, reason := a.pullOn(ctx, r, t, node, header, used, index); code != "" {
			return code, reason
		}
	}
	a.noteCredential(ctx, used, "")
	r.Finish(ItemPull, jobs.ItemDone, "", "", map[string]string{"nodes": strconv.Itoa(len(nodes))})
	return "", ""
}

// pullOn pulls the new image on one node with the header the credential made, reporting its progress on the pull item (index maps a
// layer to its place in the item and belongs to the one step). It answers
// the code of the step and the reason of the run when the pull failed, empty strings when it worked. Nothing is retried.
func (a *Applier) pullOn(ctx context.Context, r *jobs.Reporter, t target, node taskNode, header string, used *registry.Candidate, index map[string]int) (code, reason string) {
	r.Output("Pull " + t.image + " on " + node.name)
	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	err := a.Aggregator.WriteOnNode(pctx, t.envID, node.id, func(c *dockerx.Client) error {
		_, e := c.ImagePull(pctx, t.pullRef, dockerx.PullOptions{EncodedAuth: header}, func(ev dockerx.PullEvent) {
			r.Update(ItemPull, func(it *jobs.Item) { stackops.ApplyPullEvent(it, index, ev) })
		})
		return e
	})
	if err == nil {
		return "", ""
	}
	c := stackops.PullErrorCode(err)
	if c == dockerx.PullCodeFailed || c == stackops.CodeNodeUnreachable {
		a.log().Warn("the pull of an image failed", "environment", t.envID, "workload", t.name, "node", node.id, "code", c, "error", err)
	}
	a.noteCredential(ctx, used, c)
	if c == dockerx.PullCodeCancelled || ctx.Err() != nil {
		return ReasonCancelled, ReasonCancelled
	}
	return c, ReasonPullFailed
}

// taskNode is a node the service has a task on that TierPlan can reach.
type taskNode struct{ id, name string }

// taskNodes are the reachable nodes that hold a task of the service the swarm means to run, sorted by name.
func (a *Applier) taskNodes(ctx context.Context, t target) ([]taskNode, error) {
	var rows []dockerx.TaskRow
	if err := a.Service.Clusters.ClusterRead(ctx, t.envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.Tasks(ctx, dockerx.TaskFilter{ServiceIDs: []string{t.id}, DesiredState: stackops.TaskRunning})
		return e
	}); err != nil {
		return nil, err
	}
	targets, err := a.Aggregator.SwarmNodeTargets(ctx, t.envID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []taskNode
	for _, row := range rows {
		ref, ok := targets[row.NodeID]
		if !ok || seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		name := ref.Name
		if name == "" {
			name = ref.ID
		}
		out = append(out, taskNode{id: ref.ID, name: name})
	}
	slices.SortFunc(out, func(x, y taskNode) int { return strings.Compare(x.name+"\x00"+x.id, y.name+"\x00"+y.id) })
	return out, nil
}
