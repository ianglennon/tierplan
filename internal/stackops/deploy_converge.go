// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/jobs"
)

const (
	// DefaultConvergeTimeout is how long a deploy waits for one service to settle when the caller
	// sets none (BAYPLAN_STACK_CONVERGE_TIMEOUT has the same default).
	DefaultConvergeTimeout = 10 * time.Minute

	// maxParallelWaits bounds how many services a deploy polls at once, so a stack of sixty makes
	// sixteen reads of the swarm every two seconds at the most.
	maxParallelWaits = 8

	// pollReadTimeout bounds one read of a service and its tasks.
	pollReadTimeout = 20 * time.Second

	// maxFailedTasksShown is how many failed or rejected tasks of the current spec the console lists
	// beside the tasks the swarm wants running.
	maxFailedTasksShown = 3
)

// serviceWrite is a service the swarm accepted and the deploy now waits for.
type serviceWrite struct {
	name, id, action string
	// prior is the update status the service had before this deploy wrote it, nil for a new
	// service, so a status left by an earlier deploy is not taken for this one's.
	prior *dockerx.ServiceUpdateStatus
	// job is true for a replicated-job or global-job service: it runs to its end on its own and is
	// not waited for.
	job bool
}

// verdict is how the wait for one service ended: err is nil when it settled.
type verdict struct {
	name string
	err  error
}

func (d Deps) clock() Clock {
	if d.Clock == nil {
		return realClock{}
	}
	return d.Clock
}

func (d Deps) convergeTimeout() time.Duration {
	if d.ConvergeTimeout <= 0 {
		return DefaultConvergeTimeout
	}
	return d.ConvergeTimeout
}

// awaitServices waits for every service the swarm accepted to settle, at most maxParallelWaits at a
// time, and returns one verdict per service in the order given. Each row of the job ends as its wait
// does: done with the counts it settled at, or failed as Timed out, Stopped, or because the swarm will
// not start its tasks again. A wait that ends
// because ctx did leaves its row to the caller, which ends it as updated, not running. Nothing is
// written.
func (d Deps) awaitServices(ctx context.Context, envID string, writes []serviceWrite, prog *progress) []verdict {
	out := make([]verdict, len(writes))
	nodes := &nodeNames{names: map[string]string{}}
	var g errgroup.Group
	g.SetLimit(maxParallelWaits)
	for i, w := range writes {
		out[i].name = w.name
		if w.job {
			// A job runs to its end on its own schedule: it is done when the swarm took it.
			prog.notWaited(w.name)
			continue
		}
		g.Go(func() error {
			out[i].err = d.awaitService(ctx, envID, w, prog, nodes)
			return nil
		})
	}
	_ = g.Wait()
	return out
}

// awaitService waits for one service and ends its row.
func (d Deps) awaitService(ctx context.Context, envID string, w serviceWrite, prog *progress, nodes *nodeNames) error {
	var last Convergence
	// networks are the networks the service's tasks need, as the latest read found them. The wait calls
	// poll and then full on one goroutine, so full reads what poll set with no race.
	var networks []string
	err := WaitConverged(ctx, d.clock(), d.convergeTimeout(), func(ctx context.Context) (Convergence, []dockerx.TaskRow, error) {
		rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
		defer cancel()
		var (
			detail dockerx.ServiceDetail
			rows   []dockerx.TaskRow
		)
		if err := d.Clusters.ClusterRead(rctx, envID, func(c *dockerx.Client) error {
			var e error
			if detail, _, e = c.ServiceInspect(rctx, w.id); e != nil {
				return e
			}
			rows, e = c.Tasks(rctx, dockerx.TaskFilter{ServiceIDs: []string{detail.ID}})
			return e
		}); err != nil {
			return Convergence{}, nil, err
		}
		networks = detail.NetworkIDs
		return Progress(detail, rows).IgnoringUpdate(w.prior), rows, nil
	}, d.networkFull(envID, &networks), func(c Convergence, rows []dockerx.TaskRow) {
		last = c
		prog.converging(w.name, c, d.taskLines(ctx, envID, w.name, c, rows, nodes))
	})
	var nf *NetworkFullError
	switch {
	case err == nil:
		prog.settled(w.name, last)
	case errors.Is(err, ErrConvergeTimeout):
		// A timeout with tasks still new says how many were waiting for the swarm to give them an
		// address: the engine reports no address use, or no network is full.
		result := map[string]string{"minutes": strconv.Itoa(int(d.convergeTimeout() / time.Minute))}
		if last.Held > 0 {
			result["waiting"] = strconv.Itoa(last.Held)
		}
		prog.unsettled(w.name, CodeConvergeTimeout, firstWords(last.Err, last.Message), result)
	case errors.Is(err, ErrUpdateStopped):
		prog.unsettled(w.name, CodeUpdateStopped, firstWords(last.Message, last.Err), nil)
	case errors.Is(err, ErrTasksExhausted):
		prog.unsettled(w.name, CodeTasksFailed, firstWords(last.DeadErr, last.Err), nil)
	case errors.As(err, &nf):
		prog.unsettled(w.name, CodeNetworkFull, last.Err,
			map[string]string{"network": prog.clean(nf.Network), "subnet": prog.clean(nf.Subnet)})
	case ctx.Err() != nil:
		// The deploy was cancelled: the caller ends the row.
	default:
		d.log().Warn("could not read a stack service while waiting for it", "environment", envID, "service", w.name, "error", err)
		prog.unsettled(w.name, CodeEngineError, engineWords(err), nil)
	}
	return err
}

// networkFull is the question WaitConverged asks of a service with a task held new: does a network the
// service needs have a subnet with no dynamic address left? It reads through a manager only
// (Clusters.ClusterRead), because a worker's address use is node-local (07-48). It answers the first full
// subnet by network name, then subnet. A network that is gone is skipped; any other failed read is the
// caller's to ignore.
func (d Deps) networkFull(envID string, networks *[]string) func(context.Context) (*NetworkFull, error) {
	return func(ctx context.Context) (*NetworkFull, error) {
		rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
		defer cancel()
		var found []NetworkFull
		if err := d.Clusters.ClusterRead(rctx, envID, func(c *dockerx.Client) error {
			// ClusterRead may run this twice: start again.
			found = found[:0]
			for _, id := range *networks {
				nd, _, e := c.NetworkDetail(rctx, id, false)
				if e != nil {
					if dockerx.IsNotFound(e) {
						continue
					}
					return e
				}
				for _, u := range nd.Usage {
					if u.Available == 0 {
						found = append(found, NetworkFull{Network: nd.Name, Subnet: u.Subnet})
					}
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, nil
		}
		slices.SortFunc(found, func(a, b NetworkFull) int {
			if a.Network != b.Network {
				return cmp.Compare(a.Network, b.Network)
			}
			return cmp.Compare(a.Subnet, b.Subnet)
		})
		return &found[0], nil
	}
}

// clean masks Docker's text for the job, like every other remote text in it.
func (p *progress) clean(text string) string {
	if p == nil {
		return text
	}
	return jobs.Clean(text, p.mask)
}

// firstWords is the first of the swarm's texts that says anything.
func firstWords(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

// taskLines are the tasks of a service the console lists: every one the swarm wants running, and the
// newest few that failed or were rejected on the current spec. rows are newest first.
func (d Deps) taskLines(ctx context.Context, envID, service string, c Convergence, rows []dockerx.TaskRow, nodes *nodeNames) []taskLine {
	var out []taskLine
	failed := 0
	for _, r := range rows {
		switch {
		case r.DesiredState == TaskRunning:
		case (r.State == TaskFailed || r.State == TaskRejected) && c.Template != "" && r.SpecHash == c.Template && failed < maxFailedTasksShown:
			failed++
		default:
			continue
		}
		out = append(out, taskLine{
			id: r.ID, label: taskLabel(service, r), state: r.State, node: nodes.name(ctx, d, envID, r.NodeID), errText: r.Err,
		})
	}
	return out
}

// taskLabel names a task the way the swarm does: the service and the slot, or for a global service
// the start of the task's ID.
func taskLabel(service string, r dockerx.TaskRow) string {
	if r.Slot > 0 {
		return service + "." + strconv.Itoa(r.Slot)
	}
	return service + "." + r.ID[:min(len(r.ID), 8)]
}

// nodeNames resolves the node a task runs on to its host name, once for each node.
type nodeNames struct {
	mu    sync.Mutex
	names map[string]string
}

// name is the host name of a node, else its ID: a node that cannot be read is named by its ID rather
// than failing the deploy for a label.
func (n *nodeNames) name(ctx context.Context, d Deps, envID, nodeID string) string {
	if nodeID == "" {
		return ""
	}
	n.mu.Lock()
	known, ok := n.names[nodeID]
	n.mu.Unlock()
	if ok {
		return known
	}
	host := nodeID
	rctx, cancel := context.WithTimeout(ctx, pollReadTimeout)
	defer cancel()
	_ = d.Clusters.ClusterRead(rctx, envID, func(c *dockerx.Client) error {
		detail, _, err := c.NodeInspect(rctx, nodeID)
		if err == nil && detail.Hostname != "" {
			host = detail.Hostname
		}
		return err
	})
	n.mu.Lock()
	n.names[nodeID] = host
	n.mu.Unlock()
	return host
}
