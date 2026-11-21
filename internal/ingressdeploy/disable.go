// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressdeploy

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/ingress"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
)

// DisableTaskDeadline is how long a switch-off waits, on the injected clock, for the proxy's tasks to be gone after the
// service is removed, and again for the containers of a node to be gone before its cache volume is removed. A volume is
// never removed under a running container (Pitfall 13).
const DisableTaskDeadline = 2 * time.Minute

// disableReadFailures is how many reads in a row may fail while the tasks are waited for before the step fails.
const disableReadFailures = 3

// JobKindDisable is the kind of the job that switches an environment's ingress off.
const JobKindDisable = "ingress_disable"

// The items of the disable job, in the order they run. token, network and service are the enable job's words too.
const (
	ItemSwitchOff = "switch_off"
	ItemVolumes   = "volumes"
	ItemSecret    = "secret"
	ItemNodes     = "nodes"
)

// The codes an item of the disable job ends with when it fails. Each is a stable word; CodeNotRun ends the items after the
// one that failed.
const (
	CodeDisableSwitchFailed  = "disable_switch_failed"
	CodeDisableServiceFailed = "disable_service_failed"
	CodeDisableVolumesFailed = "disable_volumes_failed"
	CodeDisableNetworkFailed = "disable_network_failed"
	CodeDisableSecretFailed  = "disable_secret_failed"
	CodeDisableTokenFailed   = "disable_token_failed"
	CodeDisableNodesFailed   = "disable_nodes_failed"
)

// CodeRoutesLive is the refusal of a switch-off while a claim with routes exists in the environment, a stopped stack's
// included (D-25).
const CodeRoutesLive = "ingress_routes_live"

// disableItems are the items of the job in order.
var disableItems = []string{ItemSwitchOff, ItemService, ItemVolumes, ItemNetwork, ItemSecret, ItemToken, ItemNodes}

// disableLabels are the lines of the job's steps.
var disableLabels = map[string]string{
	ItemSwitchOff: "Stop serving routes",
	ItemService:   "Remove the proxy service",
	ItemVolumes:   "Wipe the certificate cache on each node",
	ItemNetwork:   "Remove the ingress network",
	ItemSecret:    "Remove the join token secrets",
	ItemToken:     "Disable the join token",
	ItemNodes:     "Remove the proxy nodes",
}

// disableMessages are the sentences of the failure codes, built here and never from an engine's answer.
var disableMessages = map[string]string{
	CodeDisableSwitchFailed:  "Ingress could not be switched off.",
	CodeDisableServiceFailed: "The proxy service could not be removed.",
	CodeDisableVolumesFailed: "The certificate cache could not be wiped.",
	CodeDisableNetworkFailed: "The ingress network could not be removed.",
	CodeDisableSecretFailed:  "The join token secrets could not be removed.",
	CodeDisableTokenFailed:   "The join token could not be disabled.",
	CodeDisableNodesFailed:   "The proxy nodes could not be removed.",
	CodeNotRun:               "Not run because an earlier step failed.",
}

// NodeName is a node of the environment by the ID its agent connects as and the name it goes by.
type NodeName struct {
	ID, Name string
}

// DisableRemoves is what a switch-off would remove.
type DisableRemoves struct {
	// Service and Network are the names of the proxy service and the ingress network; empty when the swarm was read and does
	// not hold them.
	Service, Network string
	// Secrets are the names of the join token secrets the swarm holds.
	Secrets []string
	// Volumes are the nodes whose certificate cache volume is wiped: every node of the swarm. A node no agent covers has an
	// empty ID.
	Volumes []NodeName
}

// DisablePreview is what a switch-off would do and what would stop it.
type DisablePreview struct {
	// Findings are the refusals, in order: a paused environment first, then the live routes. A finding is an error; a switch-off
	// with one is refused.
	Findings []Finding
	Removes  DisableRemoves
	// Unreachable are the nodes whose agent is not connected now: their cache volume is queued and wiped when the agent
	// connects (D-27).
	Unreachable []string
}

// DisableRefusedError is a switch-off that was refused: at least one finding, each an error. Nothing was written.
type DisableRefusedError struct {
	Findings []Finding
}

// Error lists the codes of the refusals.
func (e *DisableRefusedError) Error() string {
	var codes []string
	for _, f := range e.Findings {
		codes = append(codes, f.Code)
	}
	return "ingress was not switched off: " + strings.Join(codes, ", ")
}

// checkDisable is the refusals of a switch-off: a paused environment (D-42), then live routes (D-25). The holder of each
// route is worded by ingress.DescribeHolder with the caller's canView, the same redaction the policy's clash messages use
// (Phase 18 D-20, PROH-24-13); a nil canView sees nothing.
func (s *Service) checkDisable(ctx context.Context, info EnvInfo, canView func(envID string) bool) ([]Finding, error) {
	state, err := s.d.Store.DeployState(ctx, info.ID)
	if err != nil {
		return nil, err
	}
	if state.Paused {
		return []Finding{{
			Code: CodePaused, Severity: SeverityError,
			Message: "Ingress on this environment was paused by a restore. Resume it first.",
		}}, nil
	}
	routes, err := s.d.Store.LiveRoutes(ctx, info.ID)
	if err != nil {
		return nil, err
	}
	if len(routes) == 0 {
		return nil, nil
	}
	items := make([]string, 0, len(routes))
	for _, r := range routes {
		where := r.Hostname + r.PathPrefix
		kind := ingress.OwnerStack
		if r.StackID == "" {
			kind = ingress.OwnerSystem
		}
		holder := ingress.DescribeHolder(ingress.ClaimInfo{
			Claim:   ingress.Claim{EnvID: info.ID, Hostname: r.Hostname, OwnerKind: kind, StackID: r.StackID},
			EnvName: info.Name, StackName: r.StackName,
		}, canView)
		if kind == ingress.OwnerStack && canView != nil && canView(info.ID) {
			items = append(items, where+" (stack "+r.StackName+")")
			continue
		}
		items = append(items, where+": "+holder)
	}
	return []Finding{{
		Code: CodeRoutesLive, Severity: SeverityError, Items: items,
		Message: "Ingress cannot be switched off while routes are live. Remove the labels and redeploy, or remove the stacks:",
	}}, nil
}

// wipeTargets is every node a switch-off must see to the certificate cache volume of: the nodes the environment's agents know,
// then every node of the swarm's own node list that none of them covers, then every enrolled proxy node whose row is about to
// be removed and that neither names (a node that left the swarm still holds the volume). A node that has no agent has an empty
// AgentID, so wipeNode reports it unreachable and the caller queues it, keyed by its swarm node ID, until an agent for it
// connects (WR-04). The swarm is read through a manager; when that read fails the nodes are not known and the error is returned.
func (s *Service) wipeTargets(ctx context.Context, envID string, info EnvInfo) ([]NodeInfo, error) {
	targets := slices.Clone(info.Nodes)
	var rows []dockerx.NodeRow
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		rows, err = c.SwarmNodes(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	covered := func(id, hostname string) bool {
		for _, n := range targets {
			if n.SwarmNodeID == id || (n.SwarmNodeID == "" && hostname != "" && n.Name == hostname) {
				return true
			}
		}
		return false
	}
	for _, r := range rows {
		if covered(r.ID, r.Hostname) {
			continue
		}
		name := r.Hostname
		if name == "" {
			name = r.ID
		}
		targets = append(targets, NodeInfo{SwarmNodeID: r.ID, Name: name, Role: r.Role})
	}
	if s.d.Nodes != nil {
		enrolled, err := s.d.Nodes.ListNodes(ctx, envID)
		if err != nil {
			return nil, err
		}
		for _, n := range enrolled {
			if n.Status != nodeActive || n.SwarmNodeID == "" || covered(n.SwarmNodeID, "") {
				continue
			}
			targets = append(targets, NodeInfo{SwarmNodeID: n.SwarmNodeID, Name: n.Name})
		}
	}
	return targets, nil
}

// PreviewDisable says what a switch-off would remove and what would stop it, and writes nothing. A standalone environment is
// ErrStandaloneUnavailable. The swarm is read when a manager is connected; otherwise the preview lists the names it would try.
func (s *Service) PreviewDisable(ctx context.Context, envID string, canView func(envID string) bool) (DisablePreview, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return DisablePreview{}, err
	}
	if !info.Swarm {
		return DisablePreview{}, ErrStandaloneUnavailable
	}
	findings, err := s.checkDisable(ctx, info, canView)
	if err != nil {
		return DisablePreview{}, err
	}
	res := DisablePreview{Findings: findings, Removes: DisableRemoves{Secrets: []string{}, Volumes: []NodeName{}}, Unreachable: []string{}}
	for _, n := range info.Nodes {
		res.Removes.Volumes = append(res.Removes.Volumes, NodeName{ID: n.AgentID, Name: n.Name})
		if !n.Connected {
			res.Unreachable = append(res.Unreachable, n.Name)
		}
	}
	res.Removes.Service, res.Removes.Network = ingress.ServiceName, ingress.NetworkName
	if !info.ManagerConnected {
		return res, nil
	}
	// The swarm's own node list adds the nodes no agent covers; they are named as unreachable, as the job will queue them.
	if targets, err := s.wipeTargets(ctx, envID, info); err != nil {
		s.log().Warn("could not read the swarm's nodes for a switch-off preview", "environment", envID, "error", err)
	} else {
		for _, n := range targets[len(info.Nodes):] {
			res.Removes.Volumes = append(res.Removes.Volumes, NodeName{ID: n.AgentID, Name: n.Name})
			res.Unreachable = append(res.Unreachable, n.Name)
		}
	}
	var l live
	if err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
		l, err = readLive(ctx, c)
		return err
	}); err != nil {
		s.log().Warn("could not read the ingress objects for a switch-off preview", "environment", envID, "error", err)
		return res, nil
	}
	if l.service == nil || !ingress.OwnsService(l.service.Name, l.service.Labels) {
		res.Removes.Service = ""
	}
	if row, ok := l.network(); !ok || !ingress.OwnsNetwork(row.Name, row.Labels) {
		res.Removes.Network = ""
	}
	for _, row := range l.secrets {
		if ingress.OwnsSecret(row.Name, row.Labels) {
			res.Removes.Secrets = append(res.Removes.Secrets, row.Name)
		}
	}
	slices.Sort(res.Removes.Secrets)
	return res, nil
}

// Disable switches ingress off for a swarm environment. It takes the environment's operation lock, refuses while the
// environment is paused or any claim with routes exists, and starts a job that stops delivery, removes the proxy service,
// wipes the certificate cache volume on every node it can reach, removes the network and the join token secrets, disables
// the join token and removes the node rows. A node it could not reach is queued, named in the job's result and wiped when its
// agent connects (D-27). It returns the job's ID. A standalone environment is ErrStandaloneUnavailable, a held lock a
// *LockedError and a refusal a *DisableRefusedError; in each case nothing was written. Claims and certificates are not
// touched (D-26). Every step is idempotent, so a job that failed part-way is run again by calling Disable again.
func (s *Service) Disable(ctx context.Context, actor audit.Actor, owner, envID string, canView func(envID string) bool) (string, error) {
	info, err := s.d.Envs.Info(ctx, envID)
	if err != nil {
		return "", err
	}
	if !info.Swarm {
		return "", ErrStandaloneUnavailable
	}
	release, held, ok := s.d.Locks.TryAcquire(envID, stackops.Holder{
		Kind: stackops.HolderKindIngress, By: owner, Workload: "ingress", Started: s.d.Clock.Now(),
	})
	if !ok {
		return "", &LockedError{Holder: held}
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	findings, err := s.checkDisable(ctx, info, canView)
	if err != nil {
		return "", err
	}
	if len(findings) > 0 {
		return "", &DisableRefusedError{Findings: findings}
	}
	specs := make([]jobs.ItemSpec, 0, len(disableItems))
	for _, id := range disableItems {
		specs = append(specs, jobs.ItemSpec{ID: id, Label: disableLabels[id], Kind: "step"})
	}
	attached := make(chan struct{})
	job, err := s.d.Jobs.Start(jobs.Spec{Kind: JobKindDisable, EnvID: envID, Owner: owner, Items: specs},
		func(jctx context.Context, r *jobs.Reporter) {
			defer release()
			select {
			case <-attached:
			case <-jctx.Done():
			}
			s.runDisable(jctx, r, release, actor, info)
		})
	if err != nil {
		return "", err
	}
	started = true
	s.d.Locks.Attach(envID, job.ID)
	close(attached)
	return job.ID, nil
}

// disableRun is the state the steps of one switch-off share.
type disableRun struct {
	s     *Service
	actor audit.Actor
	envID string
	r     *jobs.Reporter
	// release frees the environment's operation lock before the item that ends the job is finished, so a reader that sees the
	// job done finds the lock free; freeing is safe to repeat.
	release func()
}

// runDisable runs the steps one at a time. The first failure ends its item with a code and the items after it as not run;
// nothing is retried and nothing is rolled back.
func (s *Service) runDisable(ctx context.Context, r *jobs.Reporter, release func(), actor audit.Actor, info EnvInfo) {
	run := &disableRun{s: s, actor: actor, envID: info.ID, r: r, release: release}
	steps := []func(context.Context) error{run.switchOff, run.service, run.volumes, run.network, run.secret, run.token, run.nodes}
	for i, do := range steps {
		item := disableItems[i]
		if ctx.Err() != nil {
			run.stop(item, jobs.ItemCancelled, jobs.CodeCancelled, "")
			run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
			return
		}
		r.Update(item, func(it *jobs.Item) { it.State = jobs.ItemRunning })
		if err := do(ctx); err != nil {
			if ctx.Err() != nil {
				run.stop(item, jobs.ItemCancelled, jobs.CodeCancelled, "")
				run.skipFrom(i+1, jobs.ItemCancelled, jobs.CodeCancelled)
				return
			}
			code, detail := CodeNotRun, ""
			var se *stepError
			if errors.As(err, &se) {
				code, detail = se.code, se.detail
			}
			s.log().Warn("an ingress disable step failed", "environment", info.ID, "step", item, "code", code, "error", err)
			run.stop(item, jobs.ItemFailed, code, detail)
			run.skipFrom(i+1, jobs.ItemSkipped, CodeNotRun)
			return
		}
	}
}

// stop ends the item that failed or was cancelled.
func (e *disableRun) stop(item, state, code, detail string) {
	e.release()
	if detail != "" {
		e.r.Detail(item, detail)
	}
	msg := disableMessages[code]
	if code == jobs.CodeCancelled {
		msg = ""
	}
	e.r.Finish(item, state, code, msg, nil)
}

// skipFrom ends the items from index i on without running them.
func (e *disableRun) skipFrom(i int, state, code string) {
	for _, id := range disableItems[i:] {
		e.r.Finish(id, state, code, disableMessages[code], nil)
	}
}

// done ends an item that went well.
func (e *disableRun) done(item string, result map[string]string) {
	e.r.Finish(item, jobs.ItemDone, "", "", result)
}

// switchOff stops delivery: ingress is turned off with ingress.disabled in the same transaction, which makes the hub send no
// further bundle; then the hub closes the environment's proxy sessions, before the service goes (T-24-52).
func (e *disableRun) switchOff(ctx context.Context) error {
	s := e.s
	hook := func(ctx context.Context, tx *sql.Tx) error {
		return s.d.Audit.RecordTx(ctx, tx, audit.Entry{
			Actor: e.actor, Action: audit.ActionIngressDisabled, TargetType: "environment", TargetID: e.envID,
			EnvID: e.envID, Result: audit.ResultSuccess,
		})
	}
	if _, err := s.d.Store.SetEnabled(ctx, e.envID, false, e.actor.ID, hook); err != nil {
		return fail(CodeDisableSwitchFailed, err)
	}
	// A switch-off ends what a failed enable left to report: from here the leftovers are this job's to remove.
	s.clearEnableFailed(ctx, e.envID)
	if s.d.Kicker != nil {
		s.d.Kicker.KickEnv(e.envID, "ingress_disabled")
	}
	e.done(ItemSwitchOff, nil)
	return nil
}

// taskEnded is a task state that holds nothing: its container has stopped.
func taskEnded(state string) bool {
	switch state {
	case "complete", "failed", "shutdown", "rejected", "orphaned", "remove":
		return true
	}
	return false
}

// proxyTasksLeft counts the proxy's tasks that have not ended. A task keeps the labels its service had, so the component
// label finds the tasks of a service that is already removed; only the proxy service carries it among the swarm's tasks.
func proxyTasksLeft(ctx context.Context, c *dockerx.Client) (int, error) {
	rows, err := c.Tasks(ctx, dockerx.TaskFilter{Labels: []string{ingress.LabelComponent + "=" + ingress.ComponentIngress}})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range rows {
		if !taskEnded(t.State) {
			n++
		}
	}
	return n, nil
}

// waitTasksGone polls on the injected clock until no proxy task remains, until DisableTaskDeadline has passed, until ctx ends
// or until reads failed disableReadFailures times in a row. It reports how many remained, 0 when none. It writes nothing.
func (s *Service) waitTasksGone(ctx context.Context, envID string) (int, error) {
	clock := s.d.Clock
	deadline := clock.Now().Add(DisableTaskDeadline)
	failures := 0
	for {
		var left int
		err := s.d.Engine.ClusterRead(ctx, envID, func(c *dockerx.Client) (err error) {
			left, err = proxyTasksLeft(ctx, c)
			return err
		})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if failures++; failures >= disableReadFailures {
				return 0, err
			}
		case left == 0:
			return 0, nil
		default:
			failures = 0
			if !clock.Now().Before(deadline) {
				return left, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-clock.After(stackops.PollInterval):
		}
	}
}

// service removes the proxy service and waits for its tasks to be gone, so the cache volumes are free to wipe. A service that
// is not there is done; one that is not TierPlan's is left alone.
func (e *disableRun) service(ctx context.Context) error {
	s := e.s
	removed := false
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		cur, _, err := c.ServiceInspect(ctx, ingress.ServiceName)
		switch {
		case errors.Is(err, dockerx.ErrServiceNotFound):
			return nil
		case err != nil:
			return err
		case !ingress.OwnsService(cur.Name, cur.Labels):
			e.r.Output("Left the service " + cur.Name + " in place: it is not TierPlan's.")
			return nil
		}
		if err := c.ServiceRemove(ctx, cur.ID); err != nil && !errors.Is(err, dockerx.ErrServiceNotFound) {
			return err
		}
		removed = true
		return nil
	})
	if err != nil {
		return fail(CodeDisableServiceFailed, err, dockerx.DaemonText(err))
	}
	left, err := s.waitTasksGone(ctx, e.envID)
	if err != nil {
		return fail(CodeDisableServiceFailed, err)
	}
	if left > 0 {
		return fail(CodeDisableServiceFailed, errors.New("proxy tasks remain"),
			"The proxy service was removed, but "+strconv.Itoa(left)+" of its tasks were still running after "+DisableTaskDeadline.String()+". Switch ingress off again to continue.")
	}
	e.done(ItemService, map[string]string{"removed": strconv.FormatBool(removed)})
	return nil
}

// volumes wipes the certificate cache volume on every node of the swarm through that node's agent. A node whose agent is not
// connected, that has no agent, or whose volume could not be removed, is queued as a pending wipe and named in the item's
// result (D-27); a wipe queued by an earlier run that now succeeds is cleared.
func (e *disableRun) volumes(ctx context.Context) error {
	s := e.s
	pending, err := s.d.Store.PendingWipes(ctx, e.envID)
	if err != nil {
		return fail(CodeDisableVolumesFailed, err)
	}
	queued := make(map[string]bool, len(pending))
	for _, w := range pending {
		queued[w.SwarmNodeID] = true
	}
	// A fresh read: a node whose agent connected since the job started is wiped now.
	info, err := s.d.Envs.Info(ctx, e.envID)
	if err != nil {
		return fail(CodeDisableVolumesFailed, err)
	}
	// The swarm's own node list too: a node no agent covers holds the volume all the same (WR-04). When the list cannot be read
	// the nodes to wipe are not known, and the step fails rather than leave one out; the call is run again.
	targets, err := s.wipeTargets(ctx, e.envID, info)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail(CodeDisableVolumesFailed, err,
			"The swarm's node list could not be read, so the nodes to wipe are not known. Switch ingress off again to continue.")
	}
	wiped, unreached := 0, []string{}
	for _, n := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		out, err := s.wipeNode(ctx, e.envID, n, true)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log().Warn("an ingress cache volume could not be wiped", "environment", e.envID, "node", n.Name, "error", err)
		}
		if out == wipeDone {
			wiped++
			if queued[pendingKey(n)] {
				if err := s.clearWipe(ctx, e.actor, e.envID, n, pendingKey(n)); err != nil {
					return fail(CodeDisableVolumesFailed, err)
				}
			}
			continue
		}
		unreached = append(unreached, n.Name)
		if err := s.queueWipe(ctx, e.actor, e.envID, n, queued); err != nil {
			return fail(CodeDisableVolumesFailed, err)
		}
		e.r.Output("Could not wipe the certificate cache on " + n.Name + ". It is wiped when the node's agent is connected.")
	}
	slices.Sort(unreached)
	e.done(ItemVolumes, map[string]string{
		"wiped": strconv.Itoa(wiped), "pending": strconv.Itoa(len(unreached)), "unreachable": strings.Join(unreached, ", "),
	})
	return nil
}

// network removes the ingress network. One that is not there is done and one that is not TierPlan's is left alone.
func (e *disableRun) network(ctx context.Context) error {
	s := e.s
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		rows, err := c.Networks(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Name != ingress.NetworkName {
				continue
			}
			if !ingress.OwnsNetwork(row.Name, row.Labels) {
				e.r.Output("Left the network " + row.Name + " in place: it is not TierPlan's.")
				return nil
			}
			if err := c.NetworkRemove(ctx, row.ID); err != nil && !dockerx.IsNotFound(err) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if dockerx.IsNetworkInUse(err) {
			return fail(CodeDisableNetworkFailed, err, "A service still uses the ingress network. Redeploy it without routes or remove it, then switch ingress off again.")
		}
		return fail(CodeDisableNetworkFailed, err, dockerx.DaemonText(err))
	}
	e.done(ItemNetwork, nil)
	return nil
}

// secret removes every join token secret of TierPlan's the swarm holds.
func (e *disableRun) secret(ctx context.Context) error {
	s := e.s
	removed := 0
	err := s.d.Engine.WriteOnManager(ctx, e.envID, func(c *dockerx.Client) error {
		rows, err := c.SecretList(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !ingress.OwnsSecret(row.Name, row.Labels) {
				continue
			}
			switch err := c.SecretRemove(ctx, row.ID); {
			case err == nil:
				removed++
			case errors.Is(err, dockerx.ErrObjectNotFound):
			default:
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fail(CodeDisableSecretFailed, err)
	}
	e.done(ItemSecret, map[string]string{"removed": strconv.Itoa(removed)})
	return nil
}

// token disables the environment's join token: its hash and audit stay, so no further node can enrol with it.
func (e *disableRun) token(ctx context.Context) error {
	if err := e.s.d.Tokens.DisableToken(ctx, e.actor, e.envID, "", auditUserAgent); err != nil {
		return fail(CodeDisableTokenFailed, err)
	}
	e.done(ItemToken, nil)
	return nil
}

// nodes removes the environment's proxy node rows with ingress.node_removed for each, in one transaction.
func (e *disableRun) nodes(ctx context.Context) error {
	removed := 0
	if e.s.d.NodeRows != nil {
		rows, err := e.s.d.NodeRows.RemoveEnvNodes(ctx, e.actor, e.envID)
		if err != nil {
			return fail(CodeDisableNodesFailed, err)
		}
		removed = len(rows)
	}
	e.release()
	e.done(ItemNodes, map[string]string{"removed": strconv.Itoa(removed)})
	return nil
}
