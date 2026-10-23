// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/stacks"
)

// The roll-up of one service of a stack, from its tasks or containers. It is what the stack's own state
// is counted from (DeriveState); the Overview draws the rows with the same cells the service and container
// lists use.
const (
	// ServiceRunning is every wanted task or container running (a job counts: it runs to its end).
	ServiceRunning = "running"
	// ServiceDegraded is some, but not all, running, or an update that stopped halfway, or a container
	// whose health check fails.
	ServiceDegraded = "degraded"
	// ServiceNotRunning is wanted and none running.
	ServiceNotRunning = "not_running"
	// ServiceStopped is scaled to 0 on a swarm, or every container exited on a standalone host.
	ServiceStopped = "stopped"
)

// How an image a service runs relates to the registry.
const (
	// PinPinned is an image carrying a digest: it cannot move until a deploy says so.
	PinPinned = "pinned"
	// PinNew is an image with no digest: the next deploy resolves it.
	PinNew = "new"
	// PinLocal is an image held by its ID alone: no registry has it.
	PinLocal = "local"
)

// ContainerRow is one container of a standalone service, as the Overview lists it under the service.
type ContainerRow struct {
	ID, Name string
	// State is the engine's word (running, exited ...), Health none, starting, healthy or unhealthy.
	State, Health string
}

// ServiceRow is one service of a deployed stack as the engine holds it.
type ServiceRow struct {
	// ID is the swarm service ID; empty on a standalone host, where a service is a name only.
	ID string
	// Name is the full name (stack_web on a swarm, web on a host), Short the name in the compose file.
	Name, Short string
	// Mode is replicated, global, replicated-job or global-job on a swarm; empty on a host.
	Mode string
	// State is the roll-up: ServiceRunning, ServiceDegraded, ServiceNotRunning or ServiceStopped.
	State string
	// UpdateState is the state of a swarm service's last update or rollback; empty for none and on a host.
	UpdateState string
	// Running and Desired are the tasks that run and the tasks wanted (a swarm), or the containers that
	// run and the containers there are (a host).
	Running, Desired int
	// Image is the image as the compose file wrote it, Pinned the reference the engine runs, and PinState
	// how the two relate: PinPinned, PinNew or PinLocal.
	Image, Pinned, PinState string
	// Ports are the published ports as published->target/protocol.
	Ports []string
	// Containers are the containers of a standalone service, by name.
	Containers []ContainerRow
}

// imageState says how a reference the engine runs relates to the registry.
func imageState(ref string) string {
	switch {
	case imageIDRe.MatchString(ref):
		return PinLocal
	case stacks.HasDigest(ref):
		return PinPinned
	default:
		return PinNew
	}
}

// writtenImage is the image as the compose file wrote it: the label a deploy leaves, or, for a service that
// has none, the running reference without its digest.
func writtenImage(labels map[string]string, running string) string {
	if w := labels[stacks.LabelStackImage]; w != "" {
		return w
	}
	if i := strings.LastIndex(running, "@"); i >= 0 {
		return running[:i]
	}
	return running
}

// swarmRollup is the roll-up of a swarm service from its mode, its task counts and the state of its last
// update.
func swarmRollup(r dockerx.ServiceRow) string {
	paused := r.UpdateState == "paused" || r.UpdateState == "rollback_paused"
	switch {
	case r.Mode == "replicated-job" || r.Mode == "global-job":
		// A job runs to its end and has no count to hold; its row says Completed.
		return ServiceRunning
	case r.Desired == 0:
		return ServiceStopped
	case r.Running == 0:
		return ServiceNotRunning
	case r.Running < r.Desired || paused:
		return ServiceDegraded
	}
	return ServiceRunning
}

// taskCount is a task count as an int, held at the largest a count of tasks can be told in.
func taskCount(n uint64) int {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(n)
}

func swarmPorts(ps []dockerx.ServicePort) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		text := strconv.FormatUint(uint64(p.Published), 10) + "->" + strconv.FormatUint(uint64(p.Target), 10) + "/" + p.Protocol
		if p.Mode == "host" {
			text += " (host)"
		}
		out = append(out, text)
	}
	return out
}

// SwarmRows are the services a stack owns among the services of a swarm: those in its namespace that carry
// its ID, so a service of the same name that Bayplan did not make is never the stack's. They are in name
// order.
func SwarmRows(stack Stack, all []dockerx.ServiceRow) []ServiceRow {
	out := make([]ServiceRow, 0)
	for _, r := range all {
		if r.Stack != stack.Name || r.Labels[stacks.LabelStackID] != stack.ID {
			continue
		}
		out = append(out, ServiceRow{
			ID: r.ID, Name: r.Name, Short: strings.TrimPrefix(r.Name, stack.Name+"_"), Mode: r.Mode,
			State: swarmRollup(r), UpdateState: r.UpdateState, Running: taskCount(r.Running), Desired: taskCount(r.Desired),
			Image: writtenImage(r.Labels, r.Image), Pinned: r.Image, PinState: imageState(r.Image),
			Ports: swarmPorts(r.Ports),
		})
	}
	slices.SortFunc(out, func(a, b ServiceRow) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

// hostPort turns the engine's 0.0.0.0:8080->80/tcp into 8080->80/tcp, and an unpublished port into "".
func hostPort(p string) string {
	arrow := strings.Index(p, "->")
	if arrow < 0 {
		return ""
	}
	left := p[:arrow]
	if i := strings.LastIndex(left, ":"); i >= 0 {
		left = left[i+1:]
	}
	return left + p[arrow:]
}

// standaloneRollup is the roll-up of a service from its containers.
func standaloneRollup(cs []ContainerRow) string {
	running, exited, unhealthy := 0, 0, false
	for _, c := range cs {
		switch c.State {
		case "running":
			running++
			if c.Health == "unhealthy" {
				unhealthy = true
			}
		case "exited":
			exited++
		}
	}
	switch {
	case len(cs) == 0:
		return ServiceNotRunning
	case running == len(cs) && !unhealthy:
		return ServiceRunning
	case running > 0:
		return ServiceDegraded
	case exited == len(cs):
		return ServiceStopped
	}
	return ServiceNotRunning
}

// StandaloneRows are the services a stack owns among the containers of a host: those of its Compose
// project that carry its ID, one-off containers (`compose run`) left out. A service is the containers that
// share the service label, so a service with none has no row.
func StandaloneRows(stack Stack, all []dockerx.ContainerRow) []ServiceRow {
	byService := map[string][]dockerx.ContainerRow{}
	for _, c := range all {
		if c.Labels[stacks.LabelComposeProject] != stack.Name || c.Labels[stacks.LabelStackID] != stack.ID {
			continue
		}
		if strings.EqualFold(c.Labels["com.docker.compose.oneoff"], "true") {
			continue
		}
		name := c.Labels[stacks.LabelComposeService]
		if name == "" {
			continue
		}
		byService[name] = append(byService[name], c)
	}
	out := make([]ServiceRow, 0, len(byService))
	for name, cs := range byService {
		slices.SortFunc(cs, func(a, b dockerx.ContainerRow) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
		})
		row := ServiceRow{Name: name, Short: name, Image: writtenImage(cs[0].Labels, cs[0].Image), Pinned: cs[0].Image, PinState: imageState(cs[0].Image)}
		seen := map[string]bool{}
		row.Ports = []string{}
		for _, c := range cs {
			row.Containers = append(row.Containers, ContainerRow{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), State: c.State, Health: c.Health})
			if c.State == "running" {
				row.Running++
			}
			for _, p := range c.Ports {
				if h := hostPort(p); h != "" && !seen[h] {
					seen[h] = true
					row.Ports = append(row.Ports, h)
				}
			}
		}
		row.Desired = len(cs)
		row.State = standaloneRollup(row.Containers)
		slices.Sort(row.Ports)
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b ServiceRow) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Overview reads what a stack runs now, live: one cluster read for a swarm, one list of the project's
// containers for a standalone host. It is a read. A swarm with no manager to ask is the cluster reader's
// error (envs.ErrNoManager), a host that cannot be reached is the aggregator's.
func (d Deps) Overview(ctx context.Context, envID string, stack Stack) ([]ServiceRow, error) {
	switch stack.Target {
	case stacks.TargetSwarm:
		if d.Clusters == nil {
			return nil, errors.New("stackops: no cluster reader to read the swarm with")
		}
		var read dockerx.ClusterRead
		if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
			var e error
			read, e = c.Cluster(ctx)
			return e
		}); err != nil {
			return nil, err
		}
		return SwarmRows(stack, read.Services), nil
	case stacks.TargetStandalone:
		node, err := d.StandaloneNode(ctx, envID)
		if err != nil {
			return nil, err
		}
		var rows []dockerx.ContainerRow
		if err := d.Aggregator.OnNode(ctx, envID, node, func(c *dockerx.Client) error {
			var e error
			rows, e = c.ContainersWithLabel(ctx, stacks.LabelComposeProject, stack.Name)
			return e
		}); err != nil {
			return nil, err
		}
		return StandaloneRows(stack, rows), nil
	}
	return nil, errors.New("stackops: unknown stack target " + string(stack.Target))
}

// ErrNotRead means what an environment's engine runs is not known: its cluster was never read, or has lost
// its quorum.
var ErrNotRead = errors.New("stackops: what the environment runs has not been read")

// Held is what an environment holds, read from the caches the lists use: the swarm's services, or the
// standalone host's containers.
type Held struct {
	Services   []dockerx.ServiceRow
	Containers []dockerx.ContainerRow
}

// RowsOf are the services the stack owns in what the environment holds.
func (h Held) RowsOf(stack Stack) []ServiceRow {
	if stack.Target == stacks.TargetSwarm {
		return SwarmRows(stack, h.Services)
	}
	return StandaloneRows(stack, h.Containers)
}

// Cached reads what the environment holds from the caches the lists are made from, so a list of stacks
// costs no read of its own: the cluster state of a swarm, the aggregate snapshot of a standalone host. A
// cluster that has never been read, or an environment that cannot be, is an error and the caller shows the
// stacks without a state read from the engine.
func (d Deps) Cached(ctx context.Context, envID string, swarm bool) (Held, error) {
	if swarm {
		if d.Clusters == nil {
			return Held{}, errors.New("stackops: no cluster reader to read the swarm with")
		}
		st, err := d.Clusters.State(ctx, envID)
		// A cluster that was never read, or whose quorum is lost, says nothing about the stacks. One
		// whose latest read failed after a good one serves that one: it is old, not wrong, and the
		// environment's own banners say it is not being read.
		if !st.Known() {
			if err == nil {
				err = ErrNotRead
			}
			return Held{}, err
		}
		return Held{Services: st.Read.Services}, nil
	}
	if d.Aggregator == nil {
		return Held{}, errors.New("stackops: no aggregator to read the host with")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return Held{}, err
	}
	rows := make([]dockerx.ContainerRow, len(agg.Containers))
	for i, c := range agg.Containers {
		rows[i] = c.ContainerRow
	}
	return Held{Containers: rows}, nil
}
