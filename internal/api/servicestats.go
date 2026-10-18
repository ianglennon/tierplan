// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// The aggregate stats streams (plan 06-33, RES-11, D-29, D-31): the totals of a service with
// its tasks, one total per service of a list, one sample per container of a list and the
// sum of the tasks on each node. All four watch containers through the one StatsHub of the
// container stream, so a container watched by several surfaces is sampled once, and all
// four report once per round, which is about once every two seconds (statsFeed).

const (
	// maxStatsIDs is the most objects one list stream may name (T-06-96).
	maxStatsIDs = 100
	// maxNodeStatsTasks is the most running tasks the node stream samples. A swarm with more
	// is sampled up to this many and the event says it is partial.
	maxNodeStatsTasks = 1000
	// statsTaskRefresh is how often a stream reads its tasks again, so a task that starts
	// joins the totals and one that stops leaves them (D-29).
	statsTaskRefresh = 10 * time.Second

	problemTooManyIDs = "too_many_ids"
)

// statsNodeID is a node ID as a list of containers names it: the ID of an agent, of the
// local socket or of a socket folded into a swarm (socket:<envId>).
var statsNodeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// statsRound is what one aggregate stream is: the members it starts with, the first event, how
// to re-read its members and how to turn a round into the sample event.
type statsRound struct {
	members map[string]statsMemberSpec
	// ready is the payload of the ready event.
	ready any
	// refresh reads the members again. tasks, when not nil, is sent as a tasks event: the
	// stream's description of what it watches changed. errStatsGone ends the stream.
	refresh func(ctx context.Context) (members map[string]statsMemberSpec, tasks any, err error)
	// sample makes the payload of one sample event from a round.
	sample func(views []statsView) any
}

// errStatsGone is what refresh answers when the thing the stream is about no longer exists.
var errStatsGone = errors.New("api: the object of the stats stream is gone")

// runStatsRound serves one aggregate stream: ready, then a sample event for each round, a
// tasks event when the stream's description changes, a heartbeat that re-checks access, and
// an end at the lifetime. The viewer is subscribed to every member before ready is sent.
func runStatsRound(w http.ResponseWriter, req *http.Request, d StatsDeps, spec statsRound) {
	ctx := req.Context()
	env := httpx.EnvFrom(ctx)

	feed := newStatsFeed(ctx, d.Hub, env.ID)
	defer feed.close()
	feed.sync(spec.members)

	rc, send := startSSE(w, d.Log)
	if rc == nil {
		return
	}
	if !send("ready", "", spec.ready) {
		return
	}

	refreshC, stopRefresh := d.Hub.Ticker(statsTaskRefresh)
	defer stopRefresh()
	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()

	var (
		wait   *time.Timer
		waitC  <-chan time.Time
		disarm = func() {
			if wait != nil {
				wait.Stop()
				wait, waitC = nil, nil
			}
		}
	)
	defer disarm()
	// settle sends a round when one is complete; when one is not, it starts the wait for the
	// members that are slow.
	settle := func() bool {
		if feed.hubClosed {
			return false
		}
		if feed.complete() {
			disarm()
			return send("sample", "", spec.sample(feed.take()))
		}
		if feed.dirty && wait == nil {
			wait = time.NewTimer(d.RoundWait)
			waitC = wait.C
		}
		return true
	}
	if !settle() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			return
		case msg := <-feed.in:
			feed.apply(msg)
			// What else is waiting is taken in before a round is sent.
			for more := true; more; {
				select {
				case m := <-feed.in:
					feed.apply(m)
				default:
					more = false
				}
			}
			if !settle() {
				return
			}
		case <-waitC:
			disarm()
			if feed.dirty && !send("sample", "", spec.sample(feed.take())) {
				return
			}
		case <-refreshC:
			members, tasks, err := spec.refresh(ctx)
			switch {
			case errors.Is(err, errStatsGone):
				send("closed", "", map[string]string{"reason": "gone"})
				return
			case err != nil:
				// The manager may be away for a while; the totals go on with what they watch.
				d.Log.Debug("a stats stream could not read its tasks", "environment", env.ID, "error", err)
				continue
			}
			feed.sync(members)
			if tasks != nil && !send("tasks", "", tasks) {
				return
			}
			if !settle() {
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				send("closed", "", map[string]string{"reason": "access_lost"})
				return
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// openAggregate does what every aggregate stream starts with: the principal and environment,
// the per-user stream limit and, for the swarm streams, the check that the environment is a
// swarm. release must be called when the stream ends.
func openAggregate(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter, swarm bool) (release func(), ok bool) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return nil, false
	}
	release, acquired := lim.acquire(string(p.Type) + ":" + p.ID)
	if !acquired {
		writeJSONProblem(w, http.StatusTooManyRequests, problemRateLimited,
			fmt.Sprintf("You already have %d stats streams open. Close one and try again.", lim.max))
		return nil, false
	}
	if swarm {
		if d.Clusters == nil {
			release()
			d.Log.Error("the stats routes have no cluster reader")
			writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
			return nil, false
		}
		if err := requireSwarm(ctx, d.servicesDeps(), env.ID); err != nil {
			release()
			writeProblemError(w, err)
			return nil, false
		}
	}
	return release, true
}

// servicesDeps is the part of ServicesDeps the shared swarm checks and problems read.
func (d StatsDeps) servicesDeps() ServicesDeps {
	return ServicesDeps{Clusters: d.Clusters, Log: d.Log}
}

// swarmStatsRead is what the swarm streams read from a manager in one go.
type swarmStatsRead struct {
	nodes   []dockerx.NodeRow
	service dockerx.ServiceRow
	// tasks are the running tasks that have a container, newest first.
	tasks []dockerx.TaskRow
	// targets maps the swarm node IDs that Bayplan can reach now to the node the hub asks.
	targets map[string]envs.NodeRef
	// partial is true when the tasks were cut to maxNodeStatsTasks.
	partial bool
}

// readSwarm reads the running tasks, and with them the nodes and the service when asked:
// serviceRef names one service (404 when the swarm has none), serviceIDs narrow the tasks
// to those services without reading the rest, and withNodes reads the node list.
func (d StatsDeps) readSwarm(ctx context.Context, envID, serviceRef string, serviceIDs []string, withNodes bool) (swarmStatsRead, error) {
	var out swarmStatsRead
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		out = swarmStatsRead{}
		filter := dockerx.TaskFilter{DesiredState: "running", ServiceIDs: serviceIDs}
		if serviceRef != "" || withNodes {
			read, e := c.Cluster(readCtx)
			if e != nil {
				return e
			}
			out.nodes = read.Nodes
			if serviceRef != "" {
				i := slices.IndexFunc(read.Services, func(s dockerx.ServiceRow) bool { return s.ID == serviceRef || s.Name == serviceRef })
				if i < 0 {
					return dockerx.ErrServiceNotFound
				}
				out.service = read.Services[i]
				filter.ServiceIDs = []string{out.service.ID}
			}
		}
		rows, e := c.Tasks(readCtx, filter)
		if e != nil {
			return e
		}
		for _, r := range rows {
			if r.State == "running" && r.ContainerID != "" {
				out.tasks = append(out.tasks, r)
			}
		}
		return nil
	})
	if err != nil {
		return swarmStatsRead{}, err
	}
	if len(out.tasks) > maxNodeStatsTasks && serviceRef == "" && len(serviceIDs) == 0 {
		out.tasks, out.partial = out.tasks[:maxNodeStatsTasks], true
	}
	out.targets = d.swarmTargets(ctx, envID)
	return out, nil
}

// swarmTargets maps swarm node IDs to the nodes the hub is asked on. A node nothing reaches
// is a node that is not reporting, not a failed read, so a failure here is logged and
// answered with no targets.
func (d StatsDeps) swarmTargets(ctx context.Context, envID string) map[string]envs.NodeRef {
	targets, err := d.Aggregator.SwarmNodeTargets(ctx, envID)
	if err != nil {
		d.Log.Warn("mapping swarm nodes to agents failed", "environment", envID, "error", err)
		return nil
	}
	return targets
}

// taskMembers makes one member of each task: sampled on its node when an agent reaches it,
// silent otherwise. group names the sum a task belongs to.
func taskMembers(tasks []dockerx.TaskRow, targets map[string]envs.NodeRef, group func(dockerx.TaskRow) string) map[string]statsMemberSpec {
	out := make(map[string]statsMemberSpec, len(tasks))
	for _, t := range tasks {
		spec := statsMemberSpec{Container: t.ContainerID, Group: group(t)}
		if ref, ok := targets[t.NodeID]; ok && t.NodeID != "" {
			spec.Node = ref.ID
		} else {
			spec.Silent, spec.Container = true, ""
		}
		out[t.ID] = spec
	}
	return out
}

// sharedMemoryLimit is the memory limit every one of the tasks has, or zero when one has
// none or they differ: a percent of a limit means nothing across different limits.
func sharedMemoryLimit(tasks []dockerx.TaskRow) int64 {
	if len(tasks) == 0 {
		return 0
	}
	limit := tasks[0].MemoryLimit
	for _, t := range tasks[1:] {
		if t.MemoryLimit != limit {
			return 0
		}
	}
	return max(limit, 0)
}

// statsTaskRef names one task of a service's stats.
type statsTaskRef struct {
	ID     string `json:"id"`
	Slot   int    `json:"slot"`
	Node   string `json:"node"`
	NodeID string `json:"nodeId"`
}

// serviceStatsTasks is the ready and tasks event of a service stream: the running tasks it
// watches and the memory limit they all share, null when there is none or they differ.
type serviceStatsTasks struct {
	Tasks      []statsTaskRef `json:"tasks"`
	LimitBytes *int64         `json:"limitBytes"`
}

// statsTotal is the sum of the samples of a group of tasks. Counted tasks gave a sample;
// silent tasks run on a node that is not reporting and are left out.
type statsTotal struct {
	CPU      *float64 `json:"cpu"`
	Mem      uint64   `json:"mem"`
	NetRx    *float64 `json:"netRx"`
	NetTx    *float64 `json:"netTx"`
	BlkRead  *float64 `json:"blkRead"`
	BlkWrite *float64 `json:"blkWrite"`
	Counted  int      `json:"counted"`
	Silent   int      `json:"silent"`
}

// totalOf is the wire form of a sum; the two have the same fields in the same order.
func totalOf(s statsSum) statsTotal { return statsTotal(s) }

// statsTaskSample is one task's share of a round. NoData is set for a task on a node that is
// not reporting; a task that has not answered yet has null values and NoData false.
type statsTaskSample struct {
	ID       string   `json:"id"`
	CPU      *float64 `json:"cpu"`
	Mem      *uint64  `json:"mem"`
	NetRx    *float64 `json:"netRx"`
	NetTx    *float64 `json:"netTx"`
	BlkRead  *float64 `json:"blkRead"`
	BlkWrite *float64 `json:"blkWrite"`
	NoData   bool     `json:"noData"`
}

func taskSampleOf(v statsView) statsTaskSample {
	s := statsTaskSample{ID: v.ID, NoData: v.State == StatsKindNotReporting}
	if v.State != StatsKindSample {
		return s
	}
	mem := v.Point.MemBytes
	s.CPU, s.Mem = v.Point.CPUPercent, &mem
	if v.Point.HasRates {
		rx, tx, rd, wr := v.Point.NetRxRate, v.Point.NetTxRate, v.Point.BlkReadRate, v.Point.BlkWriteRate
		s.NetRx, s.NetTx, s.BlkRead, s.BlkWrite = &rx, &tx, &rd, &wr
	}
	return s
}

// serviceStatsSample is one sample event of a service stream.
type serviceStatsSample struct {
	T     int64             `json:"t"`
	Total statsTotal        `json:"total"`
	Tasks []statsTaskSample `json:"tasks"`
}

func nodeNames(nodes []dockerx.NodeRow) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n.ID] = cleanTaskText(n.Hostname)
	}
	return out
}

func serviceTasksEvent(tasks []dockerx.TaskRow, names map[string]string) serviceStatsTasks {
	ev := serviceStatsTasks{Tasks: make([]statsTaskRef, 0, len(tasks))}
	for _, t := range tasks {
		ev.Tasks = append(ev.Tasks, statsTaskRef{ID: t.ID, Slot: t.Slot, Node: names[t.NodeID], NodeID: t.NodeID})
	}
	slices.SortStableFunc(ev.Tasks, func(a, b statsTaskRef) int {
		return cmp.Or(cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.Node, b.Node), cmp.Compare(a.ID, b.ID))
	})
	if l := sharedMemoryLimit(tasks); l > 0 {
		ev.LimitBytes = &l
	}
	return ev
}

// serveServiceStatsStream streams the stats of one swarm service: the sum over its running
// tasks and each task's own sample, a round about every two seconds.
func serveServiceStatsStream(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter) {
	ctx := req.Context()
	serviceRef := chi.URLParam(req, "serviceId")
	if !statsContainerID.MatchString(serviceRef) {
		writeJSONProblem(w, http.StatusUnprocessableEntity, problemValidationFailed, "serviceId is not valid")
		return
	}
	release, ok := openAggregate(w, req, d, lim, true)
	if !ok {
		return
	}
	defer release()
	env := httpx.EnvFrom(ctx)

	read, err := d.readSwarm(ctx, env.ID, serviceRef, nil, true)
	if err != nil {
		writeProblemError(w, serviceReadProblem(d.servicesDeps(), env.ID, err))
		return
	}
	serviceID := read.service.ID
	group := func(dockerx.TaskRow) string { return serviceID }
	first := serviceTasksEvent(read.tasks, nodeNames(read.nodes))
	last := first
	runStatsRound(w, req, d, statsRound{
		members: taskMembers(read.tasks, read.targets, group),
		ready:   first,
		refresh: func(ctx context.Context) (map[string]statsMemberSpec, any, error) {
			r, err := d.readSwarm(ctx, env.ID, serviceID, nil, true)
			if errors.Is(err, dockerx.ErrServiceNotFound) {
				return nil, nil, errStatsGone
			}
			if err != nil {
				return nil, nil, err
			}
			ev := serviceTasksEvent(r.tasks, nodeNames(r.nodes))
			var changed any
			if !sameServiceTasks(last, ev) {
				changed, last = ev, ev
			}
			return taskMembers(r.tasks, r.targets, group), changed, nil
		},
		sample: func(views []statsView) any {
			slices.SortFunc(views, func(a, b statsView) int { return cmp.Compare(a.ID, b.ID) })
			ev := serviceStatsSample{T: time.Now().UnixMilli(), Total: totalOf(sumViews(views)), Tasks: make([]statsTaskSample, 0, len(views))}
			for _, v := range views {
				ev.Tasks = append(ev.Tasks, taskSampleOf(v))
			}
			return ev
		},
	})
}

func sameServiceTasks(a, b serviceStatsTasks) bool {
	if !slices.Equal(a.Tasks, b.Tasks) {
		return false
	}
	return (a.LimitBytes == nil) == (b.LimitBytes == nil) && (a.LimitBytes == nil || *a.LimitBytes == *b.LimitBytes)
}

// statsIDs reads the comma list of ids= (also repeated). The error is the problem to send.
func statsIDs(req *http.Request, valid func(string) bool) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range req.URL.Query()["ids"] {
		for part := range strings.SplitSeq(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, "ids needs at least one object.")
	}
	if len(out) > maxStatsIDs {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemTooManyIDs,
			fmt.Sprintf("A stats stream takes at most %d objects; this one names %d. Ask for the rows that are on screen.", maxStatsIDs, len(out)))
	}
	for _, id := range out {
		if !valid(id) {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, "ids holds a value that is not an object ID.")
		}
	}
	return out, nil
}

// serviceListEntry is one service's total in a services list round. Counted is zero for a
// service with no running task. LimitBytes is the memory limit each of its running tasks
// has when they all have the same one.
type serviceListEntry struct {
	ID         string   `json:"id"`
	CPU        *float64 `json:"cpu"`
	Mem        uint64   `json:"mem"`
	NetRx      *float64 `json:"netRx"`
	NetTx      *float64 `json:"netTx"`
	Counted    int      `json:"counted"`
	Silent     int      `json:"silent"`
	LimitBytes *int64   `json:"limitBytes"`
}

// serveServicesStatsStream streams one total per service for the rows of a services list.
func serveServicesStatsStream(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter) {
	ctx := req.Context()
	ids, err := statsIDs(req, statsContainerID.MatchString)
	if err != nil {
		writeProblemError(w, err)
		return
	}
	release, ok := openAggregate(w, req, d, lim, true)
	if !ok {
		return
	}
	defer release()
	env := httpx.EnvFrom(ctx)

	// Only the services asked for count: a name or a prefix that matches another service
	// is not one of them.
	asked := make(map[string]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}
	read := func(ctx context.Context) (map[string]statsMemberSpec, map[string]int64, error) {
		r, err := d.readSwarm(ctx, env.ID, "", ids, false)
		if err != nil {
			return nil, nil, err
		}
		var mine []dockerx.TaskRow
		byService := map[string][]dockerx.TaskRow{}
		for _, t := range r.tasks {
			if asked[t.ServiceID] {
				mine = append(mine, t)
				byService[t.ServiceID] = append(byService[t.ServiceID], t)
			}
		}
		limits := map[string]int64{}
		for id, ts := range byService {
			limits[id] = sharedMemoryLimit(ts)
		}
		return taskMembers(mine, r.targets, func(t dockerx.TaskRow) string { return t.ServiceID }), limits, nil
	}
	members, limits, err := read(ctx)
	if err != nil {
		writeProblemError(w, serviceReadProblem(d.servicesDeps(), env.ID, err))
		return
	}
	runStatsRound(w, req, d, statsRound{
		members: members,
		ready:   map[string]int{"services": len(ids)},
		refresh: func(ctx context.Context) (map[string]statsMemberSpec, any, error) {
			m, l, err := read(ctx)
			if err != nil {
				return nil, nil, err
			}
			limits = l
			return m, nil, nil
		},
		sample: func(views []statsView) any {
			by := map[string][]statsView{}
			for _, v := range views {
				by[v.Spec.Group] = append(by[v.Spec.Group], v)
			}
			out := make([]serviceListEntry, 0, len(ids))
			for _, id := range ids {
				s := sumViews(by[id])
				e := serviceListEntry{ID: id, CPU: s.CPU, Mem: s.Mem, NetRx: s.NetRx, NetTx: s.NetTx, Counted: s.Counted, Silent: s.Silent}
				if l := limits[id]; l > 0 {
					e.LimitBytes = &l
				}
				out = append(out, e)
			}
			return map[string]any{"t": time.Now().UnixMilli(), "services": out}
		},
	})
}

// containerListEntry is one container's sample in a containers list round, under the id the
// stream was asked with (node|container). NoData is set when the container's node is not
// reporting or is not one of the environment's; Stopped when the container is not running.
type containerListEntry struct {
	ID       string   `json:"id"`
	CPU      *float64 `json:"cpu"`
	Mem      *uint64  `json:"mem"`
	NetRx    *float64 `json:"netRx"`
	NetTx    *float64 `json:"netTx"`
	BlkRead  *float64 `json:"blkRead"`
	BlkWrite *float64 `json:"blkWrite"`
	NoData   bool     `json:"noData"`
	Stopped  bool     `json:"stopped"`
}

// validContainerListID reports whether id is node|container with both halves well formed.
func validContainerListID(id string) bool {
	node, ctr, ok := strings.Cut(id, "|")
	return ok && statsNodeID.MatchString(node) && statsContainerID.MatchString(ctr)
}

// serveContainersStatsStream streams one sample per container for the rows of a containers
// list. A container is named by the node it runs on and its ID, as the list's rows are.
func serveContainersStatsStream(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter) {
	ids, err := statsIDs(req, validContainerListID)
	if err != nil {
		writeProblemError(w, err)
		return
	}
	release, ok := openAggregate(w, req, d, lim, false)
	if !ok {
		return
	}
	defer release()

	// A node that is not one of this environment's is never asked: the hub's call on it
	// fails as a node that is not reporting (T-06-97).
	members := make(map[string]statsMemberSpec, len(ids))
	for _, id := range ids {
		node, ctr, _ := strings.Cut(id, "|")
		members[id] = statsMemberSpec{Node: node, Container: ctr, Group: id}
	}
	runStatsRound(w, req, d, statsRound{
		members: members,
		ready:   map[string]int{"containers": len(ids)},
		refresh: func(context.Context) (map[string]statsMemberSpec, any, error) { return members, nil, nil },
		sample: func(views []statsView) any {
			by := make(map[string]statsView, len(views))
			for _, v := range views {
				by[v.ID] = v
			}
			out := make([]containerListEntry, 0, len(ids))
			for _, id := range ids {
				v := by[id]
				s := taskSampleOf(v)
				out = append(out, containerListEntry{
					ID: id, CPU: s.CPU, Mem: s.Mem, NetRx: s.NetRx, NetTx: s.NetTx, BlkRead: s.BlkRead, BlkWrite: s.BlkWrite,
					NoData: s.NoData, Stopped: v.State == StatsKindStopped,
				})
			}
			return map[string]any{"t": time.Now().UnixMilli(), "containers": out}
		},
	})
}

// nodeStatsEntry is one node's share of a node round: the summed CPU percent and memory of
// its running tasks' containers against the node's own capacity. NotReporting is set when
// nothing reaches the node or every one of its tasks is silent.
type nodeStatsEntry struct {
	ID           string   `json:"id"`
	CPU          *float64 `json:"cpu"`
	Mem          uint64   `json:"mem"`
	Cpus         float64  `json:"cpus"`
	MemoryBytes  int64    `json:"memoryBytes"`
	Counted      int      `json:"counted"`
	Silent       int      `json:"silent"`
	NotReporting bool     `json:"notReporting"`
}

// serveNodesStatsStream streams the tasks' use of each node of the swarm.
func serveNodesStatsStream(w http.ResponseWriter, req *http.Request, d StatsDeps, lim *streamLimiter) {
	ctx := req.Context()
	release, ok := openAggregate(w, req, d, lim, true)
	if !ok {
		return
	}
	defer release()
	env := httpx.EnvFrom(ctx)

	read, err := d.readSwarm(ctx, env.ID, "", nil, true)
	if err != nil {
		writeProblemError(w, serviceReadProblem(d.servicesDeps(), env.ID, err))
		return
	}
	nodes, targets, partial := read.nodes, read.targets, read.partial
	group := func(t dockerx.TaskRow) string { return t.NodeID }
	runStatsRound(w, req, d, statsRound{
		members: taskMembers(read.tasks, read.targets, group),
		ready:   map[string]any{"nodes": len(nodes), "partial": partial},
		refresh: func(ctx context.Context) (map[string]statsMemberSpec, any, error) {
			r, err := d.readSwarm(ctx, env.ID, "", nil, true)
			if err != nil {
				return nil, nil, err
			}
			nodes, targets, partial = r.nodes, r.targets, r.partial
			return taskMembers(r.tasks, r.targets, group), nil, nil
		},
		sample: func(views []statsView) any {
			by := map[string][]statsView{}
			for _, v := range views {
				by[v.Spec.Group] = append(by[v.Spec.Group], v)
			}
			out := make([]nodeStatsEntry, 0, len(nodes))
			for _, n := range nodes {
				s := sumViews(by[n.ID])
				_, reachable := targets[n.ID]
				out = append(out, nodeStatsEntry{
					ID: n.ID, CPU: nodeCPU(s), Mem: s.Mem, Cpus: float64(n.NanoCPUs) / 1e9, MemoryBytes: n.MemoryBytes,
					Counted: s.Counted, Silent: s.Silent, NotReporting: !reachable || (s.Silent > 0 && s.Counted == 0),
				})
			}
			slices.SortFunc(out, func(a, b nodeStatsEntry) int { return cmp.Compare(a.ID, b.ID) })
			return map[string]any{"t": time.Now().UnixMilli(), "nodes": out, "partial": partial}
		},
	})
}

// nodeCPU is the CPU of a node's tasks: zero for a node that runs none, nil while its tasks
// have no CPU reading yet.
func nodeCPU(s statsSum) *float64 {
	if s.CPU == nil && s.Counted == 0 && s.Silent == 0 {
		zero := 0.0
		return &zero
	}
	return s.CPU
}
