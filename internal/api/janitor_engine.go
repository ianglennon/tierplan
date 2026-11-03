// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/janitor"
	"github.com/ianglennon/tierplan/internal/redact"
	"github.com/ianglennon/tierplan/internal/stacks"
)

// inspectNodeTimeout is how long the inspects of one node may take before the node's containers
// are left un-inspected, which the planner reads as cant_tell.
const inspectNodeTimeout = time.Minute

// nodeTimeTimeout is how long reading one node's clock may take. A node that does not answer in
// time has no clock in the snapshot, which the planner reads as unknown (RESEARCH A9).
const nodeTimeTimeout = 10 * time.Second

// nodeTimeFanout is how many nodes are asked for their clock at once.
const nodeTimeFanout = 4

// janitorEngine is janitor.Engine over the server's aggregator: a snapshot is read fresh from
// every node, never from the cache, and the swarm's services and tasks come from one manager.
type janitorEngine struct {
	agg      *envs.Aggregator
	clusters *envs.Clusters
	// facts are TierPlan's own stacks, which say whether the stack or project a container is
	// labelled with still exists. Nil means they are not read, and no compose container is removable.
	facts envs.StackFacts
	// keeps are the objects a person kept out of cleanup. The zero value has none.
	keeps janitor.KeepStore
	log   *slog.Logger
}

var _ janitor.Engine = (*janitorEngine)(nil)

// NewJanitorEngine is the janitor's engine over the server's aggregator and cluster reader, for
// the Runner the server builds. clusters may be nil on a server with no swarm reader; facts are the
// stacks of the environment (the same ones the Address space check reads), nil when there are none to
// ask, in which case no compose container is ever removable; keeps are the objects a person kept out of
// cleanup, read with every snapshot and again just before a removal (a KeepStore with no store has none);
// log nil means slog.Default().
func NewJanitorEngine(agg *envs.Aggregator, clusters *envs.Clusters, facts envs.StackFacts, keeps janitor.KeepStore, log *slog.Logger) janitor.Engine {
	if log == nil {
		log = slog.Default()
	}
	return &janitorEngine{agg: agg, clusters: clusters, facts: facts, keeps: keeps, log: log}
}

// stillNotKept reads the keeps again and answers janitor.ErrNoLongerStale when a person has kept the
// object since the run planned, so a keep made while a run is under way is still honoured (D-12). A
// keep that cannot be read stops the removal: an object is never removed on a guess.
func (e *janitorEngine) stillNotKept(ctx context.Context, envID string, kind janitor.Kind, nodeID, ref string) error {
	keeps, err := e.keeps.List(ctx, envID)
	if err != nil {
		return &janitor.CodedError{Code: janitor.CodeActionFailed, Err: err}
	}
	if keeps[janitor.KeepKey{Kind: kind, NodeID: nodeID, Ref: ref}] {
		return janitor.ErrNoLongerStale
	}
	return nil
}

// Snapshot implements janitor.Engine.
func (e *janitorEngine) Snapshot(ctx context.Context, envID string) (janitor.Snapshot, error) {
	agg, _, err := e.agg.FreshSnapshot(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap := janitor.Snapshot{
		// An environment that is not a swarm has no service specs to read.
		ServicesRead: true, ReadAt: agg.FetchedAt, NotReporting: append([]string{}, agg.NotReporting...),
		Nodes: make([]janitor.Node, 0, len(agg.Nodes)), Containers: make([]janitor.Container, 0, len(agg.Containers)),
	}
	for _, n := range agg.Nodes {
		snap.Nodes = append(snap.Nodes, janitor.Node{ID: n.ID, Name: n.Name})
	}
	for _, n := range agg.Unreachable {
		snap.Unreachable = append(snap.Unreachable, janitor.Node{ID: n.ID, Name: n.Name})
	}
	for _, c := range agg.Containers {
		snap.Containers = append(snap.Containers, janitor.Container{
			ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), NodeID: c.NodeID, Node: c.Node, State: c.State,
			ImageID: c.ImageID, Created: c.Created, Labels: c.Labels,
		})
	}
	for _, img := range agg.Images {
		snap.Images = append(snap.Images, janitor.Image{
			ID: img.ID, NodeID: img.NodeID, Node: img.Node, RepoTags: img.RepoTags, RepoDigests: img.RepoDigests,
			Size: img.Size, Created: img.Created, Labels: img.Labels,
		})
	}
	e.readStacks(ctx, envID, &snap)
	// A keep that cannot be read fails the snapshot: judging without it could remove what a person kept.
	if snap.Keeps, err = e.keeps.List(ctx, envID); err != nil {
		return janitor.Snapshot{}, err
	}
	snap.NodeTimes = e.readNodeTimes(ctx, envID, agg.Nodes)
	if e.clusters == nil {
		return snap, nil
	}
	swarm, err := e.clusters.IsSwarm(ctx, envID)
	if err != nil {
		return janitor.Snapshot{}, err
	}
	snap.Swarm = swarm
	if swarm {
		snap.ServicesRead = false
		e.readManager(ctx, envID, &snap)
		if err := e.mapSwarmNodes(ctx, envID, &snap); err != nil {
			return janitor.Snapshot{}, err
		}
	}
	return snap, nil
}

// mapSwarmNodes fills Snapshot.SwarmNodeOf: the swarm node ID of each node the snapshot was read from, which is
// what a node's own janitor settings are stored under. The IDs of the snapshot are the agents' (or the local
// socket's), so a node whose agent is enrolled again keeps its settings. A node that no agent reports a swarm
// node ID for has no entry and follows its environment. A mapping that cannot be read is an error and not an
// empty map: without it a node that was switched off could not be told from one that was not.
func (e *janitorEngine) mapSwarmNodes(ctx context.Context, envID string, snap *janitor.Snapshot) error {
	targets, err := e.agg.SwarmNodeTargets(ctx, envID)
	if err != nil {
		return fmt.Errorf("map the swarm's nodes to their agents: %w", err)
	}
	snap.SwarmNodeOf = make(map[string]string, len(targets))
	for swarmID, ref := range targets {
		snap.SwarmNodeOf[ref.ID] = swarmID
	}
	return nil
}

// readStacks fills the names and IDs of the TierPlan stacks of the environment. When they cannot be
// read StacksRead stays false, and the planner removes no compose container.
func (e *janitorEngine) readStacks(ctx context.Context, envID string, snap *janitor.Snapshot) {
	if e.facts == nil {
		return
	}
	names, ids, err := e.facts.Stacks(ctx, envID)
	if err != nil {
		e.log.Warn("the janitor could not read the stacks of an environment", "environment", envID, "error", err)
		return
	}
	snap.StackNames, snap.StackIDs, snap.StacksRead = names, ids, true
}

// readNodeTimes asks each node's engine what time it is, at most nodeTimeFanout nodes at once. A
// node whose engine does not say, or does not answer in time, has no entry: its containers are
// judged as before and the reason is logged (RESEARCH Pitfall 11, A9).
func (e *janitorEngine) readNodeTimes(ctx context.Context, envID string, nodes []envs.NodeRef) map[string]time.Time {
	var (
		mu  sync.Mutex
		out = make(map[string]time.Time, len(nodes))
	)
	var g errgroup.Group
	g.SetLimit(nodeTimeFanout)
	for _, n := range nodes {
		g.Go(func() error {
			ctx, cancel := context.WithTimeout(ctx, nodeTimeTimeout)
			defer cancel()
			var at time.Time
			err := e.agg.OnNode(ctx, envID, n.ID, func(c *dockerx.Client) error {
				var err error
				at, err = c.SystemTime(ctx)
				return err
			})
			switch {
			case err != nil:
				e.log.Warn("the janitor could not read the clock of a node", "environment", envID, "node", n.Name, "error", err)
			case at.IsZero():
				e.log.Info("the engine of a node does not report its time, so its clock is not checked", "environment", envID, "node", n.Name)
			default:
				mu.Lock()
				out[n.ID] = at
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// readManager fills the swarm's services and the tasks it wants running from one manager. When no
// manager answers, ManagerRead stays false and Services nil, so nothing is judged by whether a
// service exists.
func (e *janitorEngine) readManager(ctx context.Context, envID string, snap *janitor.Snapshot) {
	var (
		services map[string]janitor.Service
		tasks    []janitor.Task
		images   []string
	)
	err := e.clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		read, err := c.Cluster(ctx)
		if err != nil {
			return err
		}
		wanted, err := c.Tasks(ctx, dockerx.TaskFilter{DesiredState: "running"})
		if err != nil {
			return err
		}
		for _, ref := range read.ServiceRefs {
			if ref.Image != "" {
				images = append(images, ref.Image)
			}
		}
		services = make(map[string]janitor.Service, len(read.Services))
		for _, s := range read.Services {
			services[s.ID] = janitor.Service{Name: s.Name, Global: s.Mode == "global" || s.Mode == "global-job"}
		}
		tasks = make([]janitor.Task, 0, len(wanted))
		for _, t := range wanted {
			tasks = append(tasks, janitor.Task{ID: t.ID, ContainerID: t.ContainerID, ServiceID: t.ServiceID, NodeID: t.NodeID, DesiredState: t.DesiredState, Slot: t.Slot})
		}
		return nil
	})
	if err != nil {
		e.log.Warn("the janitor could not read the swarm's services", "environment", envID, "error", err)
		return
	}
	snap.ManagerRead, snap.Services, snap.Tasks = true, services, tasks
	snap.ServiceImages, snap.ServicesRead = images, true
}

// Inspect implements janitor.Engine: the end of each container on one node, with the size asked
// for. A container that cannot be inspected is left out.
func (e *janitorEngine) Inspect(ctx context.Context, envID, nodeID string, ids []string) (map[string]janitor.ContainerEnd, error) {
	ctx, cancel := context.WithTimeout(ctx, inspectNodeTimeout)
	defer cancel()
	out := make(map[string]janitor.ContainerEnd, len(ids))
	err := e.agg.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		for _, id := range ids {
			if ctx.Err() != nil {
				return nil
			}
			end, err := c.ContainerEnd(ctx, id)
			if err != nil {
				continue
			}
			out[id] = janitor.ContainerEnd{
				StartedAt: end.StartedAt, FinishedAt: end.FinishedAt, ExitCode: end.ExitCode, Error: end.Error,
				OOMKilled: end.OOMKilled, Image: end.Image, ImageID: end.ImageID, SizeRw: end.SizeRw,
			}
		}
		return nil
	})
	return out, err
}

// removeContainerTimeout is how long inspecting and removing one container may take.
const removeContainerTimeout = time.Minute

// RemoveContainer implements janitor.Engine. On the container's node, through its agent, it
// inspects the container again (its state and labels) and removes it only if it is still exited and is not labelled
// tierplan.keep; the removal is neither forced nor takes the container's volumes (JAN-06). Nothing
// is retried: a removal whose answer was lost may have happened, and a second one would be wrong.
// The engine's refusal because the container is in use is ErrInUse, a container that is not there
// is ErrGone, a node that is not connected is ErrNodeOffline, and any other failure is a
// *janitor.CodedError.
func (e *janitorEngine) RemoveContainer(ctx context.Context, envID, nodeID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, removeContainerTimeout)
	defer cancel()
	if err := e.stillNotKept(ctx, envID, janitor.KindContainer, nodeID, id); err != nil {
		return err
	}
	err := e.agg.WriteOnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		d, _, err := c.ContainerDetail(ctx, id)
		if err != nil {
			return err
		}
		// The engine's own word for the state: only a container that ran and stopped is stale.
		if d.Status != "exited" || d.Labels[stacks.LabelKeep] == "true" {
			return janitor.ErrNoLongerStale
		}
		return c.ContainerRemove(ctx, id, dockerx.RemoveOptions{})
	})
	switch {
	case err == nil, errors.Is(err, janitor.ErrNoLongerStale):
		return err
	case errors.Is(err, envs.ErrNodeUnavailable):
		return fmt.Errorf("%w: %w", janitor.ErrNodeOffline, err)
	case dockerx.IsNotFound(err):
		return fmt.Errorf("%w: %w", janitor.ErrGone, err)
	case dockerx.IsConflict(err):
		return fmt.Errorf("%w: %w", janitor.ErrInUse, err)
	}
	// Anything else is a failure of the removal itself, given the word the containers API gives it:
	// the agent's policy refused, the host is read-only, the node did not answer, or it failed.
	code, _ := actionError(err)
	return &janitor.CodedError{Code: code, Err: err}
}

// evidenceReadLimit is the most bytes read from a crashed container's log. The engine is asked for
// the last lines only; the limit keeps a daemon that ignores that from sending a whole log.
const evidenceReadLimit = 1 << 20

// evidenceTimeout is how long reading a crashed container's end, image and log may take.
const evidenceTimeout = time.Minute

// Evidence implements janitor.Engine. On the container's node it reads why the container stopped,
// the image it ran and the last janitor.EvidenceLines lines of its log, and the secret values to
// hide in them: the container's own secret-looking environment values. Nothing is masked here.
// A log the engine cannot read back is reported as such and is not an error.
func (e *janitorEngine) Evidence(ctx context.Context, envID, nodeID, id string) (janitor.EvidenceSource, error) {
	ctx, cancel := context.WithTimeout(ctx, evidenceTimeout)
	defer cancel()
	var src janitor.EvidenceSource
	err := e.agg.OnNode(ctx, envID, nodeID, func(c *dockerx.Client) error {
		d, _, err := c.ContainerDetail(ctx, id)
		if err != nil {
			return err
		}
		src.End = janitor.ContainerEnd{
			StartedAt: engineInstant(d.StartedAt), FinishedAt: engineInstant(d.FinishedAt), ExitCode: d.ExitCode, Error: d.Error,
			OOMKilled: d.OOMKilled, Image: d.Image, ImageID: d.ImageID, SizeRw: -1,
		}
		src.Labels = d.Labels
		src.Secrets = redact.SecretEnvValues(d.Env)
		maskable := e.stackMask(ctx, &src, d.Labels)
		if img, _, err := c.ImageDetail(ctx, d.ImageID); err == nil && len(img.RepoDigests) > 0 {
			src.ImageDigest = img.RepoDigests[0]
		}
		if !maskable {
			// The stack's secret values could not be read, so no line of this log is kept: a tail that
			// has not been masked is never stored (T-11-35).
			src.LogsUnavailable = true
			return nil
		}
		src.Lines, src.LogsUnavailable = e.crashLog(ctx, c, id)
		return nil
	})
	return src, err
}

// stackMasker is what the stack records offer the janitor: the masker of a stack's deployed
// revision. The facts the engine is built with implement it (stackops.NetFacts).
type stackMasker interface {
	DeployedMasker(ctx context.Context, stackID string) (stacks.Masker, error)
}

// stackMask gives src the masker of the TierPlan stack a container belongs to, if it belongs to one.
// It answers false when the container is a stack's and the stack's values could not be read. A stack
// that does not exist, or has not deployed, hides nothing and is not a failure.
func (e *janitorEngine) stackMask(ctx context.Context, src *janitor.EvidenceSource, labels map[string]string) bool {
	stackID := labels[stacks.LabelStackID]
	sm, ok := e.facts.(stackMasker)
	if stackID == "" || !ok {
		return true
	}
	m, err := sm.DeployedMasker(ctx, stackID)
	if err != nil {
		e.log.Warn("the janitor could not read the secret values of a stack, so a crashed container's log was not kept", "stack", stackID, "error", err)
		return false
	}
	src.Mask = m.Mask
	return true
}

// crashLog reads the last janitor.EvidenceLines lines of a container's log, each with the engine's
// timestamp in front. A log that cannot be read at all is unavailable; one that fails part way keeps
// the lines read so far.
func (e *janitorEngine) crashLog(ctx context.Context, c *dockerx.Client, id string) (lines []string, unavailable bool) {
	rc, tty, err := c.ContainerLogs(ctx, id, dockerx.LogOptions{Tail: strconv.Itoa(janitor.EvidenceLines)})
	if err != nil {
		if !errors.Is(err, dockerx.ErrLogsUnreadable) {
			e.log.Warn("the janitor could not read the log of a crashed container", "container", id, "error", err)
		}
		return nil, true
	}
	defer func() { _ = rc.Close() }()
	// A log is read to its end, but never further than the tail asked for plus the line that
	// straddles it, so a daemon that ignores the tail cannot make this read a whole log.
	err = pumpLogs(ctx, io.LimitReader(rc, evidenceReadLimit), tty, func(l LogLine) {
		lines = append(lines, l.T+" "+l.M)
		if len(lines) > 2*janitor.EvidenceLines {
			lines = lines[len(lines)-janitor.EvidenceLines:]
		}
	})
	if err != nil {
		e.log.Warn("the janitor could not read the whole log of a crashed container", "container", id, "error", err)
		if len(lines) == 0 {
			return nil, true
		}
	}
	return lines, false
}

// engineInstant parses a time the engine sends in RFC 3339; the zero time (year 1), an empty text and
// anything that does not parse are the zero time.
func engineInstant(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}
	return t.UTC()
}
