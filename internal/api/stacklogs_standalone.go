// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The log of a standalone stack (plan 07-29, STK-10, D-22): each container of the stack's Compose
// project is followed through the container log's own transport, its lines fed into the source of
// its service. A deploy that recreates a container is seen in the engine's event feed: the new
// container is followed in place of the old, and a marker row says so.

const (
	// labelComposeNumber is the label that numbers a service's containers (web-1, web-2).
	labelComposeNumber = "com.docker.compose.container-number"
	// labelComposeOneOff marks a container made by `compose run`, which is not the stack's.
	labelComposeOneOff = "com.docker.compose.oneoff"

	unitContainers = "containers"
	unitTasks      = "tasks"
)

// stackContainer is a container of a standalone stack as the stream describes it.
type stackContainer struct {
	ID, Name, Service, State string
	// Number is the container's number among its service's: the index a prefix shows.
	Number  int
	Created time.Time
}

// live says the container writes, or may write, a log: it is not one that ended or never started.
func (c stackContainer) live() bool {
	switch c.State {
	case "running", "restarting", "paused":
		return true
	}
	return false
}

// containersOf reads the containers the stack owns from a host's list: those of its Compose
// project that carry its ID, one-off containers left out.
func containersOf(st stackops.Stack, all []dockerx.ContainerRow) []stackContainer {
	var out []stackContainer
	for _, c := range all {
		if c.Labels[stacks.LabelComposeProject] != st.Name || c.Labels[stacks.LabelStackID] != st.ID {
			continue
		}
		if strings.EqualFold(c.Labels[labelComposeOneOff], "true") {
			continue
		}
		service := c.Labels[stacks.LabelComposeService]
		if service == "" {
			continue
		}
		n, err := strconv.Atoi(c.Labels[labelComposeNumber])
		if err != nil || n < 1 {
			n = 1
		}
		out = append(out, stackContainer{
			ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Service: service, State: c.State, Number: n, Created: c.Created,
		})
	}
	return out
}

// containerWanted says whether a container passes the containers filter: its ID, the start of one
// of at least eight characters, or its name.
func (p stackLogParams) containerWanted(c stackContainer) bool {
	return len(p.containers) == 0 || slices.ContainsFunc(p.containers, func(f string) bool {
		return c.ID == f || (len(f) >= minTaskPrefix && strings.HasPrefix(c.ID, f)) || c.Name == f
	})
}

// stackContainerOf is how a container is told to the browser.
func stackContainerOf(c stackContainer) StackLogTask {
	return StackLogTask{ID: c.ID, Service: c.Service, Slot: c.Number, State: c.State}
}

// containerFollow is the read of one container's log and what a reopen of it needs.
type containerFollow struct {
	c stackContainer

	run *logRun
	// done is closed when the feeder of the current run has left. The feeder owns track and seam
	// until then.
	done  chan struct{}
	track logTrack
	seam  *logSeam
	// gone is set when the container was replaced by another: its run ends on its own.
	gone bool
	// reopening is set while the log is being opened again after a restart.
	reopening bool
}

// standaloneFollower follows the containers of a standalone stack on the host's engine.
func (d StacksDeps) standaloneFollower(ctx context.Context, envID string, st stackops.Stack, params stackLogParams) (*stackFollower, error) {
	if d.Aggregator == nil {
		d.Log.Error("the stack log route has no aggregator")
		return nil, httpx.Problem(http.StatusInternalServerError, "", "internal error")
	}
	node, err := d.ops().StandaloneNode(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	list := func(ctx context.Context) ([]stackContainer, error) {
		var rows []dockerx.ContainerRow
		readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
		defer cancel()
		if err := d.Aggregator.OnNode(readCtx, envID, node, func(c *dockerx.Client) error {
			var e error
			rows, e = c.ContainersWithLabel(readCtx, stacks.LabelComposeProject, st.Name)
			return e
		}); err != nil {
			return nil, err
		}
		return containersOf(st, rows), nil
	}
	all, err := list(ctx)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	// The events are subscribed before the logs are opened, so a start between the two is not missed.
	_, feed, cancelFeed := d.Aggregator.Events(ctx, envID)

	wanted := func(c stackContainer) bool {
		return params.serviceWanted(c.Service, c.Service) && params.containerWanted(c) && (params.stopped || c.live())
	}
	var chosen []stackContainer
	for _, c := range all {
		if wanted(c) {
			chosen = append(chosen, c)
		}
	}
	capped := len(chosen) > d.Logs.MaxFollow
	total := len(chosen)
	if capped {
		// Past the cap the newest containers are followed.
		slices.SortStableFunc(chosen, func(a, b stackContainer) int {
			return cmp.Or(b.Created.Compare(a.Created), cmp.Compare(a.Name, b.Name))
		})
		chosen = chosen[:d.Logs.MaxFollow]
	}
	slices.SortFunc(chosen, func(a, b stackContainer) int {
		return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Number, b.Number), cmp.Compare(a.ID, b.ID))
	})

	var (
		mu       sync.Mutex
		follows  = map[string]*containerFollow{}
		announce = map[string]StackLogTask{}
		feeds    = map[string]chan svcRec{}
		sources  []logSource
		broken   = make(chan struct{}, 1)
	)
	logsDeps := LogsDeps{Aggregator: d.Aggregator, Log: d.Log}
	signalBroken := func() {
		select {
		case broken <- struct{}{}:
		default:
		}
	}
	// feedOf is the channel of a service's source, made when the service first has a container.
	feedOf := func(service string, create bool) chan svcRec {
		if ch, ok := feeds[service]; ok || !create {
			return ch
		}
		ch := make(chan svcRec, 256)
		feeds[service] = ch
		sources = append(sources, logSource{Name: service, Records: ch})
		return ch
	}
	// start reads the container's log into its service's source until the run ends.
	start := func(cf *containerFollow, run *logRun, out chan<- svcRec) {
		cf.run = run
		cf.done = make(chan struct{})
		done, seam := cf.done, cf.seam
		go func() {
			defer close(done)
			for l := range run.lines {
				if seam.skip(l) {
					continue
				}
				cf.track.note(l)
				select {
				case out <- svcRec{T: l.T, S: l.S, M: l.M, TaskID: cf.c.ID, NodeID: node}:
				case <-ctx.Done():
					return
				}
			}
			if run.err != nil && !errors.Is(run.err, context.Canceled) {
				signalBroken()
			}
		}()
	}
	// open starts following a container. A container whose log cannot be read, or that is gone
	// already, is left out: it must not end the stream of the others.
	open := func(c stackContainer, p logParams, out chan<- svcRec) (*containerFollow, error) {
		run, _, err := openLogRun(ctx, logsDeps, envID, node, c.ID, p)
		if errors.Is(err, dockerx.ErrLogsUnreadable) || dockerx.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		cf := &containerFollow{c: c}
		start(cf, run, out)
		return cf, nil
	}
	stopAll := func() {
		cancelFeed()
		mu.Lock()
		defer mu.Unlock()
		for _, cf := range follows {
			if cf.run != nil {
				cf.run.stop()
			}
		}
	}
	fol := &stackFollower{stop: stopAll, feed: feed, broken: broken}

	first := logParams{follow: true, nodeID: node, since: params.since, tail: params.tail}
	for _, c := range chosen {
		cf, err := open(c, first, feedOf(c.Service, true))
		if err != nil {
			stopAll()
			return nil, err
		}
		if cf == nil {
			continue
		}
		follows[c.ID] = cf
		announce[c.ID] = stackContainerOf(c)
	}
	bodyOf := func(announce map[string]StackLogTask) stackLogTasks {
		out := make([]StackLogTask, 0, len(announce))
		for _, t := range announce {
			out = append(out, t)
		}
		slices.SortFunc(out, func(a, b StackLogTask) int {
			return cmp.Or(cmp.Compare(a.Service, b.Service), cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.ID, b.ID))
		})
		return stackLogTasks{Target: string(stacks.TargetStandalone), Tasks: out}
	}
	fol.sources = sources
	fol.ready = bodyOf(announce)
	if capped {
		fol.capped = &stackLogCap{Count: total, Cap: d.Logs.MaxFollow, Unit: unitContainers}
	}
	fol.tasks = func() map[string]taskInfo {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]taskInfo, len(follows))
		for id, cf := range follows {
			out[id] = taskInfo{ID: id, Service: cf.c.Service, Slot: cf.c.Number}
		}
		return out
	}
	fol.accept = func(r svcRec) bool { return params.after == "" || r.T > params.after }
	fol.line = func(_ context.Context, r svcRec) (int, string, []stackEvent) {
		mu.Lock()
		defer mu.Unlock()
		if cf, ok := follows[r.TaskID]; ok {
			return cf.c.Number, "", nil
		}
		return 0, "", nil
	}
	fol.afterBurst = func(context.Context, string) []stackEvent { return nil }

	// adopt follows a container that appeared after the stream opened, in place of the one of its
	// service with the same number that it replaces, if there is one. It reports whether the
	// stream has to end to take the container in (its service has no source yet).
	adopt := func(c stackContainer, at time.Time) (events []stackEvent, reconnect bool, err error) {
		mu.Lock()
		out := feedOf(c.Service, false)
		if out == nil {
			defer mu.Unlock()
			return nil, len(follows) < d.Logs.MaxFollow, nil
		}
		if len(follows) >= d.Logs.MaxFollow {
			// Past the cap the newest container is not followed: the viewer was told at the start.
			mu.Unlock()
			return nil, false, nil
		}
		var replaced *containerFollow
		for _, cf := range follows {
			if cf.c.Service == c.Service && cf.c.Number == c.Number && cf.c.ID != c.ID && !cf.gone {
				replaced = cf
			}
		}
		mu.Unlock()
		// A container that appears is followed from its first line.
		cf, err := open(c, logParams{follow: true, nodeID: node, tail: strconv.Itoa(maxLogTail)}, out)
		if err != nil || cf == nil {
			return nil, false, err
		}
		mu.Lock()
		defer mu.Unlock()
		follows[c.ID] = cf
		announce[c.ID] = stackContainerOf(c)
		if replaced != nil {
			replaced.gone = true
			delete(announce, replaced.c.ID)
			events = append(events, stackEvent{name: "marker", delay: true, body: slotMarker{
				Kind: markerReplaced, Service: c.Service, Unit: "container", Slot: c.Number,
				Old: replaced.c.ID, New: c.ID, At: at.UTC().Format(logTimeLayout),
			}})
		}
		events = append(events, stackEvent{name: "tasks", body: bodyOf(announce)})
		return events, false, nil
	}
	// reconcile reads the host's containers and follows the new ones that are wanted; it also tells
	// the browser of a container whose state changed.
	reconcile := func(ctx context.Context, at time.Time) ([]stackEvent, bool) {
		rows, err := list(ctx)
		if err != nil {
			return nil, false
		}
		slices.SortStableFunc(rows, func(a, b stackContainer) int { return a.Created.Compare(b.Created) })
		var evs []stackEvent
		changed := false
		for _, c := range rows {
			mu.Lock()
			cf, followed := follows[c.ID]
			if followed && !cf.gone && announce[c.ID].State != c.State {
				announce[c.ID] = stackContainerOf(c)
				changed = true
			}
			if followed && cf.gone {
				// The container it was replaced by is followed; this one is no longer listed as a
				// task, whatever its state.
				delete(announce, c.ID)
			}
			mu.Unlock()
			if followed || !wanted(c) || !c.live() {
				continue
			}
			more, reconnect, err := adopt(c, at)
			if err != nil {
				d.Log.Warn("could not follow a new container of a stack's log", "stack", st.ID, "error", err)
				continue
			}
			if reconnect {
				return evs, true
			}
			evs = append(evs, more...)
		}
		if changed {
			mu.Lock()
			evs = append(evs, stackEvent{name: "tasks", body: bodyOf(announce)})
			mu.Unlock()
		}
		return evs, false
	}
	fol.refresh = func(ctx context.Context) ([]stackEvent, bool) { return reconcile(ctx, time.Now()) }
	// reopen follows a container's log again from its last line after the container started again.
	reopen := func(cf *containerFollow) {
		mu.Lock()
		if cf.reopening || cf.gone {
			mu.Unlock()
			return
		}
		cf.reopening = true
		prev, done := cf.run, cf.done
		mu.Unlock()
		// The feeder leaves first, so the track is ours; this runs apart from the stream's loop,
		// which the feeder may be waiting on.
		prev.stop()
		<-done
		again := logParams{follow: true, nodeID: node}
		if cf.track.stamp != "" {
			again.since = cf.track.stamp
		}
		run, _, err := openLogRun(ctx, logsDeps, envID, node, cf.c.ID, again)
		mu.Lock()
		defer mu.Unlock()
		cf.reopening = false
		if err != nil {
			if ctx.Err() == nil && !dockerx.IsNotFound(err) {
				signalBroken()
			}
			return
		}
		cf.seam = cf.track.seam()
		start(cf, run, feeds[cf.c.Service])
	}
	fol.onEvent = func(ctx context.Context, ev envs.NodeEvent) ([]stackEvent, bool) {
		if ev.Type != "container" || ev.Action != "start" || ev.Attributes[stacks.LabelComposeProject] != st.Name {
			return nil, false
		}
		mu.Lock()
		cf, followed := follows[ev.ActorID]
		mu.Unlock()
		if followed {
			if !cf.gone {
				go reopen(cf)
			}
			return nil, false
		}
		// The marker sits where the engine's own clock puts the start, between the old container's
		// last line and the new one's first.
		at := ev.Time
		if at.IsZero() {
			at = time.Now()
		}
		return reconcile(ctx, at)
	}
	return fol, nil
}
