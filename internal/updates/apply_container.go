// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/stackops"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// The update of a plain container (plan 12-20, IMG-06, D-07, D-12, D-13, RESEARCH Pitfall 11). The replacement is made from the
// whole inspect document of the old container with only the image changed, and it is created before anything is stopped, so a
// refusal at create (the agent's host-access policy, a bad configuration) changes nothing. Then the old container is stopped
// and renamed out of the way, the replacement takes its name and starts, and only when it runs (and is healthy when it has a
// health check) is the old container removed, without its volumes. A replacement that does not come up is removed and the old
// container is put back and started again.

const (
	// HealthWindow is how long a replacement has to run, and to be healthy when it has a health check. A health check that needs
	// longer (its start period plus its interval and timeout for each try) extends it, never beyond the applier's converge
	// timeout.
	HealthWindow = 5 * time.Minute

	// runningSettle is how long a replacement without a health check must be seen running before it counts as up, so a command
	// that fails at once is not taken for a container that started.
	runningSettle = 2 * time.Second

	// containerWriteTimeout bounds one create, rename, connect, start or remove on the node.
	containerWriteTimeout = 60 * time.Second

	// stopSlack is what a stop is given beyond the container's own stop timeout.
	stopSlack = 30 * time.Second

	// restoreTimeout bounds the whole of putting the old container back.
	restoreTimeout = 2 * time.Minute

	// containerPollErrors is how many reads of the replacement in a row may fail before the wait gives up.
	containerPollErrors = 3

	// defaultStopSeconds is the stop timeout of a container that sets none.
	defaultStopSeconds = 10

	// Health check defaults of the engine, for a field a health check leaves at zero.
	defaultHealthInterval = 30 * time.Second
	defaultHealthTimeout  = 30 * time.Second
	defaultHealthRetries  = 3
)

// subject is the workload an update is about, as the pieces of Start that differ between a service and a container need it.
type subject struct {
	kind, name, id string
	// specImage is the image the workload is written with: a service's spec image. A container takes the check's own text.
	specImage string
	// nodeID is the node a container runs on.
	nodeID string
}

func serviceSubject(row dockerx.ServiceRow) subject {
	return subject{kind: "service", name: row.Name, id: row.ID, specImage: row.Image}
}

func containerSubject(w Workload) subject {
	s := subject{kind: "container", name: strings.TrimPrefix(w.Name, "/"), id: w.ID, specImage: w.Written}
	if len(w.NodeIDs) > 0 {
		s.nodeID = w.NodeIDs[0]
	}
	return s
}

// containerWorkload finds the container of that name among the environment's workloads. A container is named without its leading
// slash.
func (a *Applier) containerWorkload(ctx context.Context, envID, name string) (Workload, bool, error) {
	ws, err := a.Service.Workloads(ctx, envID)
	if err != nil {
		return Workload{}, false, err
	}
	name = strings.TrimPrefix(name, "/")
	for _, w := range ws {
		if w.Kind == "container" && strings.TrimPrefix(w.Name, "/") == name {
			return w, true, nil
		}
	}
	return Workload{}, false, nil
}

// Container is a container of an environment as an update sees it.
type Container struct {
	Workload Workload
	// StackID is the ID of the stack TierPlan manages that the container belongs to, empty when it belongs to none.
	StackID string
	// Service is the container's service in its Compose file, empty for a container made on its own.
	Service string
}

// FindContainer answers the container of that name, and the managed stack it belongs to if it does. ErrWorkloadGone is returned
// when the environment has no such container.
func (a *Applier) FindContainer(ctx context.Context, envID, name string) (Container, error) {
	w, found, err := a.containerWorkload(ctx, envID, name)
	if err != nil {
		return Container{}, err
	}
	if !found {
		return Container{}, ErrWorkloadGone
	}
	c := Container{Workload: w, Service: w.Labels[stacks.LabelComposeService]}
	if w.Labels[stacks.LabelComposeProject] != "" {
		if c.StackID, err = a.managedStackIDOf(ctx, envID, w.Name, w.Labels); err != nil {
			return Container{}, err
		}
	}
	return c, nil
}

// containerUpdatable refuses the containers this path does not update: TierPlan's own server, a task container of a swarm
// service (updated through its service), a container of a stack TierPlan manages (updated through a revision of that stack)
// and a container of a Compose project TierPlan does not manage, which a recreate would leave with a stale config hash (D-21).
func (a *Applier) containerUpdatable(ctx context.Context, envID string, w Workload) error {
	switch {
	case a.isSelf(w.ID):
		return &NotUpdatableError{Code: NotUpdatableAgent}
	case w.Task:
		return &NotUpdatableError{Code: NotUpdatableTaskContainer}
	case len(w.NodeIDs) == 0 || w.NodeIDs[0] == "":
		return ErrWorkloadGone
	case w.Labels[stacks.LabelComposeProject] == "":
		return nil
	}
	id, err := a.managedStackIDOf(ctx, envID, w.Name, w.Labels)
	if err != nil {
		return err
	}
	if id != "" {
		return &NotUpdatableError{Code: NotUpdatableManaged}
	}
	return &NotUpdatableError{Code: NotUpdatableComposeUnmanaged}
}

// minSelfIDLen is the shortest prefix of the server's own container ID that identifies it.
const minSelfIDLen = 12

// isSelf says whether id is the container TierPlan's server runs in.
func (a *Applier) isSelf(id string) bool {
	return len(a.SelfContainerID) >= minSelfIDLen && strings.HasPrefix(id, a.SelfContainerID)
}

// updateContainer does the update of one plain container and ends the run through end. end is called once, before the last
// step of the job is finished, and frees the environment's lock.
func (a *Applier) updateContainer(ctx context.Context, r *jobs.Reporter, run Run, t target, end endFunc) {
	// order is the steps that exist and are still to come, so a failure can end the rest as skipped.
	order := []string{ItemPull, ItemCreate, ItemSwap, ItemStart}
	endState := RunFailed
	// cause is why the update failed when the run ends rollback_failed, which keeps restore_failed as its reason.
	cause := ""
	fail := func(item, code, reason string) {
		if cause == "" {
			cause = reason
		}
		end(endState, reason, cause)
		msg := itemMessages[code]
		if msg == "" {
			msg = itemMessages[reason]
		}
		if m := stackops.CodeMessage(code); m != "" {
			msg = m
		}
		r.Finish(item, jobs.ItemFailed, code, msg, nil)
		skip := false
		for _, id := range order {
			if id == item {
				skip = true
				continue
			}
			if skip {
				r.Finish(id, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
			}
		}
	}
	read := func(rctx context.Context, fn func(*dockerx.Client) error) error {
		return a.Aggregator.OnNode(rctx, t.envID, t.nodeID, fn)
	}
	write := func(wctx context.Context, fn func(*dockerx.Client) error) error {
		return a.Aggregator.WriteOnNode(wctx, t.envID, t.nodeID, fn)
	}
	timed := func(d time.Duration, fn func(context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return fn(cctx)
	}
	// why is the reason and code of a write that failed: a cancelled update says so, whatever the engine answered.
	why := func(err error, fallback string) string {
		if ctx.Err() != nil {
			return ReasonCancelled
		}
		return fallback
	}

	if ctx.Err() != nil {
		fail(ItemPull, ReasonCancelled, ReasonCancelled)
		return
	}

	// The pull, on the container's own node.
	r.Update(ItemPull, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	header, used, aerr := a.authFor(ctx, t)
	if aerr != nil {
		a.log().Error("could not resolve the registry credential of a container image", "environment", t.envID, "container", t.name, "error", aerr)
		fail(ItemPull, stackops.CodeRegistryUnresolved, ReasonPullFailed)
		return
	}
	if code, reason := a.pullOn(ctx, r, t, taskNode{id: t.nodeID, name: "its node"}, header, used, map[string]int{}); code != "" {
		fail(ItemPull, code, reason)
		return
	}
	a.noteCredential(ctx, used, "")
	r.Finish(ItemPull, jobs.ItemDone, "", "", map[string]string{"nodes": "1"})

	// The old container as it is now, and the config of its image and of the new one.
	if ctx.Err() != nil {
		fail(ItemCreate, ReasonCancelled, ReasonCancelled)
		return
	}
	r.Update(ItemCreate, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	var old container.InspectResponse
	if err := timed(pollReadTimeout, func(cctx context.Context) error {
		return read(cctx, func(c *dockerx.Client) error {
			var e error
			old, e = c.ContainerInspectFull(cctx, t.id)
			return e
		})
	}); err != nil {
		a.log().Warn("could not read a container to update it", "environment", t.envID, "container", t.name, "error", err)
		if dockerx.IsNotFound(err) {
			fail(ItemCreate, ReasonContainerChanged, ReasonContainerChanged)
			return
		}
		fail(ItemCreate, ReasonEngine, why(err, ReasonEngine))
		return
	}
	name := strings.TrimPrefix(old.Name, "/")
	if old.Config == nil || old.HostConfig == nil || name == "" {
		fail(ItemCreate, ReasonContainerChanged, ReasonContainerChanged)
		return
	}
	if old.HostConfig.AutoRemove {
		fail(ItemCreate, "auto_remove", ReasonCreateRefused)
		return
	}
	oldRunning := old.State != nil && old.State.Running
	oldImage := a.imageConfig(ctx, t, old.Image)
	newImage := a.imageConfig(ctx, t, t.pullRef)

	cfg, host, first, more := RecreateSpec(old, t.ref)
	ForgetImageDefaults(cfg, oldImage)
	short := old.ID
	if len(short) > shortIDLen {
		short = short[:shortIDLen]
	}
	tempName, oldName := name+"-tierplan-new-"+short, name+"-tierplan-old-"+short

	// The replacement is created beside the old container, which keeps running: a refusal here changes nothing.
	var newID string
	if err := timed(containerWriteTimeout, func(cctx context.Context) error {
		return write(cctx, func(c *dockerx.Client) error {
			var e error
			newID, _, e = c.ContainerCreate(cctx, tempName, cfg, host, first)
			return e
		})
	}); err != nil {
		code, reason := createFailure(err)
		a.log().Warn("the replacement of a container was not created", "environment", t.envID, "container", t.name, "reason", reason, "error", err)
		fail(ItemCreate, code, why(err, reason))
		return
	}

	// Everything from here undoes itself: the replacement is removed, the old container is given its name back and started.
	var stopAttempted, renamedOld bool
	restore := func() bool {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
		defer cancel()
		r.Output("Putting the old container back")
		ok := true
		if newID != "" {
			err := write(rctx, func(c *dockerx.Client) error {
				return c.ContainerRemove(rctx, newID, dockerx.RemoveOptions{Force: true})
			})
			if err != nil && !dockerx.IsNotFound(err) {
				ok = false
				a.log().Error("the replacement of a container could not be removed", "environment", t.envID, "container", t.name, "error", err)
			}
		}
		if renamedOld {
			err := write(rctx, func(c *dockerx.Client) error { return c.ContainerRename(rctx, old.ID, name) })
			if err != nil {
				ok = false
				a.log().Error("the old container could not be given its name back", "environment", t.envID, "container", t.name, "error", err)
			}
		}
		if oldRunning && stopAttempted {
			err := write(rctx, func(c *dockerx.Client) error {
				return c.ContainerAction(rctx, old.ID, dockerx.VerbStart, dockerx.ActionOptions{})
			})
			if err != nil {
				ok = false
				a.log().Error("the old container could not be started again", "environment", t.envID, "container", t.name, "error", err)
			}
		}
		return ok
	}
	abort := func(item, code, reason string) {
		cause = reason
		switch {
		case !restore():
			endState = RunRollbackFailed
			r.Output("The old container could not be put back. It is " + oldName + " if it was renamed; check " + name + " on the node.")
			reason = ReasonRestoreFailed
		case stopAttempted || renamedOld:
			// The old container had been touched and is back: the update is rolled back, not merely refused (IMG-07).
			endState = RunRolledBack
		}
		fail(item, code, reason)
	}

	// The networks beyond the first, which the create could not carry on the oldest engines.
	moreNames := make([]string, 0, len(more))
	for n := range more {
		moreNames = append(moreNames, n)
	}
	slices.Sort(moreNames)
	for _, n := range moreNames {
		if err := timed(containerWriteTimeout, func(cctx context.Context) error {
			return write(cctx, func(c *dockerx.Client) error { return c.NetworkConnectEndpoint(cctx, n, newID, more[n]) })
		}); err != nil {
			code, reason := createFailure(err)
			a.log().Warn("the replacement of a container could not join a network", "environment", t.envID, "container", t.name, "network", n, "reason", reason, "error", err)
			abort(ItemCreate, code, why(err, reason))
			return
		}
	}

	// The replacement's health check, as the engine made it from the old one and the new image.
	var repl container.InspectResponse
	_ = timed(pollReadTimeout, func(cctx context.Context) error {
		return read(cctx, func(c *dockerx.Client) error {
			var e error
			repl, e = c.ContainerInspectFull(cctx, newID)
			return e
		})
	})
	var hc *container.HealthConfig
	if repl.Config != nil {
		hc = repl.Config.Healthcheck
	}
	if hc == nil && newImage != nil {
		hc = newImage.Healthcheck
	}
	hasHealth := hc != nil && len(hc.Test) > 0 && hc.Test[0] != "NONE"
	r.Output("Created " + tempName + " from " + shortDigest(t.to))
	r.Finish(ItemCreate, jobs.ItemDone, "", "", map[string]string{"name": tempName})
	if hasHealth {
		r.Add(jobs.ItemSpec{ID: ItemHealth, Label: "Wait for a healthy check", Kind: stackops.KindStep})
		order = append(order, ItemHealth)
	}
	r.Add(jobs.ItemSpec{ID: ItemRemoveOld, Label: "Remove the old container", Kind: stackops.KindStep})
	order = append(order, ItemRemoveOld)

	// The old container is recorded before it is stopped, so a restart of the server finds it (12-24).
	r.Update(ItemSwap, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if ctx.Err() != nil {
		abort(ItemSwap, ReasonCancelled, ReasonCancelled)
		return
	}
	if err := timed(persistTimeout, func(cctx context.Context) error {
		_, e := a.Runs.SetOldContainer(cctx, run.ID, old.ID)
		return e
	}); err != nil {
		a.log().Error("the old container of an update could not be recorded", "environment", t.envID, "container", t.name, "error", err)
		abort(ItemSwap, ReasonEngine, why(err, ReasonEngine))
		return
	}
	if oldRunning {
		stop := defaultStopSeconds
		if old.Config.StopTimeout != nil && *old.Config.StopTimeout > 0 {
			stop = *old.Config.StopTimeout
		}
		stopAttempted = true
		r.Output("Stop " + name)
		if err := timed(time.Duration(stop)*time.Second+stopSlack, func(cctx context.Context) error {
			return write(cctx, func(c *dockerx.Client) error {
				return c.ContainerAction(cctx, old.ID, dockerx.VerbStop, dockerx.ActionOptions{})
			})
		}); err != nil {
			a.log().Warn("the old container did not stop", "environment", t.envID, "container", t.name, "error", err)
			abort(ItemSwap, ReasonEngine, why(err, ReasonEngine))
			return
		}
	}
	if err := timed(containerWriteTimeout, func(cctx context.Context) error {
		return write(cctx, func(c *dockerx.Client) error { return c.ContainerRename(cctx, old.ID, oldName) })
	}); err != nil {
		a.log().Warn("the old container could not be renamed", "environment", t.envID, "container", t.name, "error", err)
		abort(ItemSwap, ReasonEngine, why(err, ReasonEngine))
		return
	}
	renamedOld = true
	if err := timed(containerWriteTimeout, func(cctx context.Context) error {
		return write(cctx, func(c *dockerx.Client) error { return c.ContainerRename(cctx, newID, name) })
	}); err != nil {
		a.log().Warn("the replacement of a container could not take its name", "environment", t.envID, "container", t.name, "error", err)
		abort(ItemSwap, ReasonEngine, why(err, ReasonEngine))
		return
	}
	r.Output("Renamed " + name + " to " + oldName + " and " + tempName + " to " + name)
	r.Finish(ItemSwap, jobs.ItemDone, "", "", map[string]string{"old": oldName})

	// The replacement starts (a container that was stopped stays stopped) and is waited for.
	r.Update(ItemStart, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	if !oldRunning {
		r.Finish(ItemStart, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		if hasHealth {
			r.Finish(ItemHealth, jobs.ItemSkipped, stackops.CodeNotRun, "", nil)
		}
	} else {
		r.Output("Start " + name)
		if err := timed(containerWriteTimeout, func(cctx context.Context) error {
			return write(cctx, func(c *dockerx.Client) error {
				return c.ContainerAction(cctx, newID, dockerx.VerbStart, dockerx.ActionOptions{})
			})
		}); err != nil {
			a.log().Warn("the replacement of a container did not start", "environment", t.envID, "container", t.name, "error", err)
			abort(ItemStart, ReasonDidntStart, why(err, ReasonDidntStart))
			return
		}
		// With a health check the start step ends at the start and the health step carries the wait; without one, the start step
		// ends when the container has been seen running.
		waitItem := ItemStart
		if hasHealth {
			waitItem = ItemHealth
			r.Finish(ItemStart, jobs.ItemDone, "", "", nil)
			// The step counts the window the replacement is waited for in, so the console can show how long is left.
			window := int64(a.healthWindow(hc, hasHealth) / time.Second)
			r.Update(ItemHealth, func(it *jobs.Item) { it.State, it.Unit, it.Total = jobs.ItemRunning, "seconds", window })
		}
		if reason := a.waitUp(ctx, t, newID, hc, hasHealth, read); reason != "" {
			a.log().Warn("the replacement of a container did not come up", "environment", t.envID, "container", t.name, "reason", reason)
			abort(waitItem, reason, reason)
			return
		}
		r.Finish(waitItem, jobs.ItemDone, "", "", nil)
	}

	// The new container is up: the old one goes, its volumes stay (the replacement uses them).
	r.Update(ItemRemoveOld, func(it *jobs.Item) { it.State = jobs.ItemRunning })
	// The new container is up, so the old one goes whether or not the update was cancelled meanwhile.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), containerWriteTimeout)
	rmErr := write(rctx, func(c *dockerx.Client) error { return c.ContainerRemove(rctx, old.ID, dockerx.RemoveOptions{}) })
	rcancel()
	end(RunDone, "", "")
	if rmErr != nil && !dockerx.IsNotFound(rmErr) {
		a.log().Warn("the old container could not be removed after its update", "environment", t.envID, "container", t.name, "error", rmErr)
		r.Output("The old container could not be removed; it is stopped and named " + oldName)
		r.Finish(ItemRemoveOld, jobs.ItemFailed, "remove_old_failed", itemMessages["remove_old_failed"], nil)
		return
	}
	r.Output("Updated " + name + " to " + shortDigest(t.to))
	r.Finish(ItemRemoveOld, jobs.ItemDone, "", "", nil)
}

// createFailure words why the engine did not take the replacement: the code of the step and the reason of the run.
func createFailure(err error) (code, reason string) {
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrHostReadOnly):
		return ReasonHostReadOnly, ReasonHostReadOnly
	case errors.As(envs.ClassifyDockerError(err), &denied) && denied.HostPolicy:
		return ReasonAgentHostPolicy, ReasonAgentHostPolicy
	case denied != nil, dockerx.IsPolicyDenied(err), dockerx.IsRefusal(err):
		return ReasonCreateRefused, ReasonCreateRefused
	}
	return ReasonEngine, ReasonEngine
}

// imageConfig is the config of an image on the container's node, nil when it cannot be read: the image is then not known to
// carry anything, which keeps the old container's values as they are.
func (a *Applier) imageConfig(ctx context.Context, t target, ref string) *container.Config {
	if ref == "" {
		return nil
	}
	ictx, cancel := context.WithTimeout(ctx, previewInspectTimeout)
	defer cancel()
	var cfg *container.Config
	err := a.Aggregator.OnNode(ictx, t.envID, t.nodeID, func(c *dockerx.Client) error {
		_, raw, e := c.ImageDetail(ictx, ref)
		if e != nil {
			return e
		}
		var doc struct {
			Config *container.Config `json:"Config"`
		}
		if e := json.Unmarshal(raw, &doc); e != nil {
			return e
		}
		cfg = doc.Config
		return nil
	})
	if err != nil {
		a.log().Warn("could not read the config of an image to keep a container's own settings apart from the image's", "environment", t.envID, "container", t.name, "error", err)
		return nil
	}
	return cfg
}

// healthWindow is how long the replacement is waited for: HealthWindow, or the time its health check needs to fail or pass when
// that is longer, but never more than the converge timeout.
func (a *Applier) healthWindow(hc *container.HealthConfig, hasHealth bool) time.Duration {
	window := HealthWindow
	if hasHealth {
		interval, timeout, retries := hc.Interval, hc.Timeout, hc.Retries
		if interval <= 0 {
			interval = defaultHealthInterval
		}
		if timeout <= 0 {
			timeout = defaultHealthTimeout
		}
		if retries <= 0 {
			retries = defaultHealthRetries
		}
		window = max(window, hc.StartPeriod+(interval+timeout)*time.Duration(retries+1))
	}
	return min(window, a.convergeTimeout())
}

// waitUp waits for the replacement to run, and to be healthy when it has a health check. It answers "" when the container is up,
// else the reason it is not: didnt_start (it exited or keeps restarting), unhealthy, timeout, cancelled or engine_error. Time is
// the applier's clock.
func (a *Applier) waitUp(ctx context.Context, t target, id string, hc *container.HealthConfig, hasHealth bool, read func(context.Context, func(*dockerx.Client) error) error) string {
	clock := a.clock()
	deadline := clock.Now().Add(a.healthWindow(hc, hasHealth))
	var upSince time.Time
	failures := 0
	for {
		if ctx.Err() != nil {
			return ReasonCancelled
		}
		rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
		var d dockerx.ContainerDetail
		err := read(rctx, func(c *dockerx.Client) error {
			var e error
			d, _, e = c.ContainerDetail(rctx, id)
			return e
		})
		cancel()
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ReasonCancelled
			}
			if failures++; failures >= containerPollErrors {
				a.log().Warn("the replacement of a container could not be read", "environment", t.envID, "container", t.name, "error", err)
				return ReasonEngine
			}
		default:
			failures = 0
			switch {
			case d.Dead || d.Status == "exited" || d.Status == "removing" || d.Restarting || d.RestartCount > 0:
				// A replacement that has had to be restarted already crashed, whatever its restart policy made of it.
				return ReasonDidntStart
			case !d.Running:
				upSince = time.Time{}
			case !d.Paused:
				switch d.Health {
				case "unhealthy":
					return ReasonUnhealthy
				case "healthy":
					return ""
				case "starting":
					// The check has not passed yet.
				default:
					if !hasHealth {
						if upSince.IsZero() {
							upSince = clock.Now()
						}
						if !clock.Now().Before(upSince.Add(runningSettle)) {
							return ""
						}
					}
				}
			}
		}
		if !clock.Now().Before(deadline) {
			return ReasonTimeout
		}
		select {
		case <-ctx.Done():
			return ReasonCancelled
		case <-clock.After(stackops.PollInterval):
		}
	}
}
