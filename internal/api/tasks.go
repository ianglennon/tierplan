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
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
)

const (
	// maxTaskText is the most bytes of a task's error or message that is sent.
	maxTaskText = 2048

	// taskStateUnknown is what a task reads on a node that cannot vouch for it (RESEARCH E13).
	taskStateUnknown = "unknown"
	// nodeStateReady is the one node state whose tasks' states are believed.
	nodeStateReady = "ready"
)

// TaskItem is one swarm task.
type TaskItem struct {
	ID           string `json:"id" doc:"The task ID"`
	Slot         int    `json:"slot" doc:"The replica number of a replicated service's task; 0 for a global service's"`
	NodeID       string `json:"nodeId" doc:"The swarm node the task was placed on; empty while it is not placed. The swarm keeps the ID of a node that has since been removed"`
	Node         string `json:"node" doc:"The node's host name; empty when the task has no node or its node is no longer in the swarm"`
	NodeKnown    bool   `json:"nodeKnown" doc:"False when the task names a node the swarm no longer lists (a removed node); true otherwise, also for a task with no node"`
	TargetNodeID string `json:"targetNodeId" doc:"The node ID the container routes of this environment take (?node= of the container page) for the task's node; empty when no connected agent or local socket reaches that node now"`

	State        string `json:"state" doc:"The state to show: Docker's own word, except unknown for a task that has not ended on a node that is down, disconnected or removed, which cannot say it is still running"`
	DockerState  string `json:"dockerState" doc:"The state Docker last recorded for the task (new, pending, assigned, accepted, preparing, ready, starting, running, complete, failed, shutdown, rejected, remove, orphaned)"`
	DesiredState string `json:"desiredState" doc:"The state the swarm wants the task in"`
	Error        string `json:"error" doc:"Docker's error text about the task, cleaned of control characters; empty when there is none. Docker's free text, to be shown as plain text"`
	Message      string `json:"message" doc:"Docker's status message, cleaned the same way"`
	ExitCode     *int   `json:"exitCode" nullable:"true" doc:"The exit code of the task's container once the task has ended; null while it runs and when it never had a container"`
	ContainerID  string `json:"containerId" doc:"The task's container ID on its node; empty until it has one"`
	CreatedAt    string `json:"createdAt" format:"date-time" doc:"When the task was created"`
	UpdatedAt    string `json:"updatedAt" format:"date-time" doc:"When the task last changed"`
	StateSince   string `json:"stateSince" format:"date-time" doc:"When the task reached dockerState"`
}

// TaskSlot is one slot of a replicated service, or one node of a global service: the task
// that is current there and the ones before it.
type TaskSlot struct {
	Slot    int        `json:"slot" doc:"The slot number; 0 for a global service's row"`
	Node    string     `json:"node" doc:"The host name of the node the current task is on; empty when it has none or its node is gone"`
	Current TaskItem   `json:"current" doc:"The task that should be running there: the newest one whose desired state is running, else the newest"`
	History []TaskItem `json:"history" doc:"The earlier tasks, newest first, as many as Docker keeps; empty when there are none"`
}

// ServiceTasksBody is the tasks of one service.
type ServiceTasksBody struct {
	Slots     []TaskSlot `json:"slots" doc:"A replicated service by slot number, a global one by node name (nodes the swarm no longer lists last)"`
	Retention int        `json:"retention" doc:"How many tasks per slot Docker keeps (TaskHistoryRetentionLimit); 5 when the cluster sets none"`
	Total     int        `json:"total" doc:"Every task listed, current and earlier"`
	Global    bool       `json:"global" doc:"True for a global service, whose rows are nodes rather than slots"`
}

// ServiceTasksOutput wraps ServiceTasksBody for huma.
type ServiceTasksOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceTasksBody
}

// registerServiceTasks mounts the service tasks route.
func registerServiceTasks(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "listServiceTasks", Method: http.MethodGet, Path: "/api/env/{envId}/services/{serviceId}/tasks",
		Summary: "The tasks of a swarm service, grouped by slot",
		Description: "The tasks of one service read live from a manager: for a replicated service one entry per slot, for a global service one per node, each with the task that is current there and the earlier tasks Docker still keeps (up to the cluster's TaskHistoryRetentionLimit, given as retention). " +
			"A task that has not ended on a node that is down, disconnected or no longer in the swarm reads unknown, never running; dockerState keeps Docker's own word. A task whose node the swarm no longer lists has nodeKnown false. targetNodeId is the node ID the container routes take, empty when nothing reaches the node now. " +
			"Docker's error and message text is cleaned of control characters and invalid UTF-8 on the server and is to be shown as plain text. " +
			"404 service_not_found when the swarm has no such service; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceDetailInput) (*ServiceTasksOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := listServiceTasks(ctx, d, env.ID, in.ServiceID)
			if err != nil {
				return nil, err
			}
			return &ServiceTasksOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// listServiceTasks reads a service's tasks, the node list and the history limit in one
// cluster read, and groups them. Nothing is written.
func listServiceTasks(ctx context.Context, d ServicesDeps, envID, serviceID string) (ServiceTasksBody, error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return ServiceTasksBody{}, err
	}
	var (
		read      dockerx.ClusterRead
		svc       dockerx.ServiceRow
		rows      []dockerx.TaskRow
		retention int
	)
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		if read, e = c.Cluster(readCtx); e != nil {
			return e
		}
		i := slices.IndexFunc(read.Services, func(s dockerx.ServiceRow) bool { return s.ID == serviceID || s.Name == serviceID })
		if i < 0 {
			return dockerx.ErrServiceNotFound
		}
		svc = read.Services[i]
		if rows, e = c.Tasks(readCtx, dockerx.TaskFilter{ServiceIDs: []string{svc.ID}}); e != nil {
			return e
		}
		retention, e = c.TaskHistoryLimit(readCtx)
		return e
	})
	if err != nil {
		return ServiceTasksBody{}, serviceReadProblem(d, envID, err)
	}

	targets := swarmNodeTargets(ctx, d, envID)
	global := svc.Mode == "global" || svc.Mode == "global-job"
	return ServiceTasksBody{
		Slots: groupTasks(rows, global, read.Nodes, targets), Retention: retention, Total: len(rows), Global: global,
	}, nil
}

// serviceReadProblem turns the error of a cluster read of one service into the problem the
// service routes answer with.
func serviceReadProblem(d ServicesDeps, envID string, err error) error {
	switch {
	case errors.Is(err, dockerx.ErrServiceNotFound):
		return httpx.Problem(http.StatusNotFound, problemServiceNotFound, "This service does not exist. It may have been removed.")
	case swarmWriteCode(err) == codeNoManagerConnected:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoManager, "No manager of the swarm is connected.")
	case envs.ReasonFor(err) == envs.ReasonNoQuorum:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoQuorum, "The swarm has lost its quorum.")
	}
	return snapshotProblem(d.Log, envID, err)
}

// groupTasks turns the tasks of a service (newest first) into slots: by slot number for a
// replicated service, by node for a global one.
func groupTasks(rows []dockerx.TaskRow, global bool, nodes []dockerx.NodeRow, targets map[string]envs.NodeRef) []TaskSlot {
	nodeOf := make(map[string]dockerx.NodeRow, len(nodes))
	for _, n := range nodes {
		nodeOf[n.ID] = n
	}
	type group struct {
		slot  int
		items []TaskItem
		desc  []bool // whether each item's desired state is running
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, r := range rows {
		key := "slot:" + strconv.Itoa(r.Slot)
		if global {
			key = "node:" + r.NodeID
		}
		g, ok := byKey[key]
		if !ok {
			g = &group{slot: r.Slot}
			if global {
				g.slot = 0
			}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, taskItem(r, nodeOf, targets))
		g.desc = append(g.desc, r.DesiredState == "running")
	}
	slots := make([]TaskSlot, 0, len(groups))
	for _, g := range groups {
		cur := slices.Index(g.desc, true)
		if cur < 0 {
			cur = 0
		}
		s := TaskSlot{Slot: g.slot, Node: g.items[cur].Node, Current: g.items[cur], History: []TaskItem{}}
		for i, it := range g.items {
			if i != cur {
				s.History = append(s.History, it)
			}
		}
		slots = append(slots, s)
	}
	if global {
		slices.SortStableFunc(slots, func(a, b TaskSlot) int {
			// Named nodes by name, then nodes the swarm no longer lists, then tasks with no node.
			return cmp.Or(
				cmp.Compare(nodeGroupRank(a.Current), nodeGroupRank(b.Current)),
				cmp.Compare(a.Node, b.Node),
				cmp.Compare(a.Current.NodeID, b.Current.NodeID),
			)
		})
	} else {
		slices.SortStableFunc(slots, func(a, b TaskSlot) int { return cmp.Compare(a.Slot, b.Slot) })
	}
	return slots
}

// nodeGroupRank orders the rows of a global service: nodes with a name, then removed nodes,
// then tasks not placed yet.
func nodeGroupRank(t TaskItem) int {
	switch {
	case t.NodeID == "":
		return 2
	case !t.NodeKnown:
		return 1
	}
	return 0
}

// taskItem turns a task into its body, with the node it is on named, its state made honest
// and its free text cleaned.
func taskItem(r dockerx.TaskRow, nodes map[string]dockerx.NodeRow, targets map[string]envs.NodeRef) TaskItem {
	n, listed := nodes[r.NodeID]
	it := TaskItem{
		ID: r.ID, Slot: r.Slot, NodeID: r.NodeID, NodeKnown: r.NodeID == "" || listed,
		DockerState: r.State, DesiredState: r.DesiredState,
		Error: cleanTaskText(r.Err), Message: cleanTaskText(r.Message), ContainerID: r.ContainerID,
		CreatedAt: rfc3339(r.CreatedAt), UpdatedAt: rfc3339(r.UpdatedAt), StateSince: rfc3339(r.StateSince),
	}
	if listed {
		it.Node = cleanTaskText(n.Hostname)
	}
	it.State = effectiveTaskState(r, n.State)
	if taskEnded(r.State) && r.ExitCode != nil {
		// A running task's container has no exit code yet; the engine's zero is not one.
		code := *r.ExitCode
		it.ExitCode = &code
	}
	if ref, ok := targets[r.NodeID]; ok && r.NodeID != "" {
		it.TargetNodeID = ref.ID
	}
	return it
}

// taskEnded reports whether a task state is one the task cannot leave.
func taskEnded(state string) bool {
	switch state {
	case "complete", "failed", "shutdown", "rejected", "remove", "orphaned":
		return true
	}
	return false
}

// effectiveTaskState is the state to show for a task whose node is in nodeState (the
// swarm's word for it; empty when the swarm no longer lists the node). Docker leaves a
// task running on a node that is down (RESEARCH E13), so a task that has not ended is
// shown as unknown unless its node is ready. A task that has ended keeps its state: the
// node cannot un-end it. A task with no node has none to vouch for.
func effectiveTaskState(t dockerx.TaskRow, nodeState string) string {
	if t.NodeID == "" || taskEnded(t.State) || nodeState == nodeStateReady {
		return t.State
	}
	return taskStateUnknown
}

// cleanTaskText makes Docker's free text safe to send: invalid UTF-8 becomes U+FFFD,
// tabs and line breaks become spaces, every other control character is dropped, and the
// result is cut to maxTaskText bytes on a character boundary (T-06-21).
func cleanTaskText(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(min(len(s), maxTaskText))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxTaskText {
		cut := maxTaskText
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
	}
	return out
}
