// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// quorumLossMarker is in the message of the 500 a manager answers every cluster
// question with when the swarm has no leader (RESEARCH Pitfall 11).
const quorumLossMarker = "The swarm does not have a leader"

// IsQuorumLoss reports whether err is a manager saying the swarm has no leader.
// Node-local calls to the same manager still work.
func IsQuorumLoss(err error) bool {
	return err != nil && strings.Contains(err.Error(), quorumLossMarker)
}

// agentServiceName is the name the install command gives the swarm service.
// Its tasks are the agents, one per node. A service installed from the stack file
// is named <stack>_bayplan-agent and the stack name is the user's, so the name
// alone cannot find it; agentServiceLabel does.
const agentServiceName = "bayplan-agent"

// agentServiceLabel and agentServiceLabelValue are the label both installs put on
// the service (--label of the command, deploy.labels of the stack file). It is
// how Bayplan recognises its agent service. A service the install command created
// before the label existed is still found by agentServiceName.
const (
	agentServiceLabel      = "bayplan.agent"
	agentServiceLabelValue = "true"
)

// isAgentService reports whether s is a Bayplan agent service: it carries the
// label, or it has the exact name the install command gives it. The image is
// never looked at; anyone can run it under any name.
func isAgentService(s swarm.Service) bool {
	return s.Spec.Name == agentServiceName || s.Spec.Labels[agentServiceLabel] == agentServiceLabelValue
}

// NodeRow is one swarm node as the cluster view shows it.
type NodeRow struct {
	ID, Hostname, Role, Availability, State, EngineVersion, OS string
	// Leader is true for the one manager that leads the swarm.
	Leader bool
	// Reachability is the manager's reachability (reachable, unreachable or
	// unknown), empty for a worker.
	Reachability string
	// Address is the address the swarm reaches the node at (Status.Addr), the one the
	// node.ip constraint is matched against.
	Address string
	// Arch is the platform architecture the engine reports (x86_64, aarch64).
	Arch string
	// NanoCPUs and MemoryBytes are the node's resources, zero when not reported.
	NanoCPUs, MemoryBytes int64
	// Labels are the labels of the node's spec (node.labels.*), EngineLabels those of
	// its engine (engine.labels.*). Never nil.
	Labels, EngineLabels map[string]string
	// ManagerAddr is the address a manager listens on for the swarm, empty for a worker.
	ManagerAddr string
}

// ServiceRow is one swarm service with its task counts.
type ServiceRow struct {
	ID, Name string
	// Mode is replicated, global, replicated-job or global-job.
	Mode             string
	Running, Desired uint64
	// UpdateState is the state of the service's last update, empty when it has none.
	UpdateState string
	UpdatedAt   time.Time
	// Image is the image the service's spec names, as written; empty for a spec with no
	// container spec.
	Image string
	// Stack is the stack the service belongs to (the com.docker.stack.namespace label of
	// its spec), empty for a service made on its own.
	Stack string
	// Agent is true for a Bayplan agent service (see isAgentService).
	Agent bool
	// Ports are the ports the service publishes, sorted by published port, then target
	// port, then protocol. Never nil.
	Ports []ServicePort
	// UpdateMessage is the message of the service's last update (UpdateStatus.Message),
	// empty when it has none.
	UpdateMessage string
	// HasPreviousSpec is true when the swarm still holds the spec before the last update,
	// which is what a rollback goes back to.
	HasPreviousSpec bool
	// CreatedAt is when the service was created, in UTC.
	CreatedAt time.Time
	// Labels are the labels of the service's spec. They stay on the server, where the
	// list search reads them; no response carries them.
	Labels map[string]string
	// Constraints are the placement constraints of the service's spec, as written. Never
	// nil.
	Constraints []string
	// Preferences are the spread descriptors of the service's placement preferences (the
	// label each Spread preference spreads tasks over), in the spec's order. Never nil.
	Preferences []string
	// MaxReplicas is the most tasks of the service one node may run; 0 when unset.
	MaxReplicas uint64
}

// ServicePort is one port a swarm service publishes.
type ServicePort struct {
	// Published is the port on the swarm (or on the node, for host mode), Target the port
	// in the container.
	Published, Target uint32
	// Protocol is tcp, udp or sctp.
	Protocol string
	// Mode is ingress (the routing mesh) or host (only the node that runs the task).
	Mode string
}

// stackLabel is the label docker stack deploy puts on every service of a stack (E19).
const stackLabel = "com.docker.stack.namespace"

// servicePorts converts an endpoint's ports, sorted so a row reads the same on every
// refresh. The result is never nil.
func servicePorts(in []swarm.PortConfig) []ServicePort {
	out := make([]ServicePort, 0, len(in))
	for _, p := range in {
		out = append(out, ServicePort{
			Published: p.PublishedPort, Target: p.TargetPort,
			Protocol: string(p.Protocol), Mode: string(p.PublishMode),
		})
	}
	slices.SortStableFunc(out, func(a, b ServicePort) int {
		return cmp.Or(cmp.Compare(a.Published, b.Published), cmp.Compare(a.Target, b.Target), cmp.Compare(a.Protocol, b.Protocol))
	})
	return out
}

// AgentTaskRow is the state of the agent service's current task on one node.
type AgentTaskRow struct{ NodeID, State, Error string }

// ServiceRef is what one swarm service's spec mentions, whatever its tasks are doing:
// the networks it attaches to, by network ID, and the volumes it mounts, by name. A
// service scaled to zero still mentions them, which is why the rules for deleting a
// network or a volume read specs and not tasks (D-41, RESEARCH Pitfall 7).
type ServiceRef struct {
	ID, Name string
	// Networks are network IDs. A spec that names a swarm network by name is resolved
	// to its ID; a target that is neither is kept as the spec wrote it. Never nil.
	Networks []string
	// Volumes are the sources of the service's volume mounts. Bind, tmpfs and other
	// mounts are not volumes. Never nil.
	Volumes []string
	// VolumeMounts are those volume mounts with where and how each is mounted. Never nil.
	VolumeMounts []ServiceVolumeMount
	// Image is the image the service's spec names, as written: a tag, and a digest too once
	// the swarm has pinned it. Empty for a spec with no container spec.
	Image string
}

// ServiceVolumeMount is one volume mount of a service's container spec.
type ServiceVolumeMount struct {
	// Source is the volume's name, Target where it appears in the container.
	Source, Target string
	ReadOnly       bool
}

// ClusterRead is what one manager says about the cluster.
type ClusterRead struct {
	Nodes      []NodeRow
	Services   []ServiceRow
	AgentTasks []AgentTaskRow
	// ServiceRefs are the networks and volumes each service's spec mentions, sorted
	// by service name then ID.
	ServiceRefs []ServiceRef
}

// Cluster reads the nodes, the services with their task counts and the tasks of
// the agent service (found by its label, see isAgentService) from a swarm manager. It is read-only and has no
// variant that changes anything. A worker, and a manager that has lost quorum,
// answer with errors whose text carries the daemon's own message, which
// envs.ReasonFor maps to not_a_manager and no_quorum.
//
// Nodes are sorted by hostname then ID and services by name then ID. The agent
// tasks are reduced to the newest task of each node, which is its current one;
// the rows are sorted by node ID.
func (c *Client) Cluster(ctx context.Context) (ClusterRead, error) {
	cli := c.cur()
	var out ClusterRead

	ns, err := cli.NodeList(ctx, client.NodeListOptions{})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list swarm nodes on %s: %w", c.t.Describe(), err)
	}
	out.Nodes = make([]NodeRow, 0, len(ns.Items))
	for _, n := range ns.Items {
		row := NodeRow{
			ID: n.ID, Hostname: n.Description.Hostname, Role: string(n.Spec.Role),
			Availability: string(n.Spec.Availability), State: string(n.Status.State),
			EngineVersion: n.Description.Engine.EngineVersion, OS: n.Description.Platform.OS,
			Address: n.Status.Addr, Arch: n.Description.Platform.Architecture,
			NanoCPUs: n.Description.Resources.NanoCPUs, MemoryBytes: n.Description.Resources.MemoryBytes,
			Labels: nonNilLabels(n.Spec.Labels), EngineLabels: nonNilLabels(n.Description.Engine.Labels),
		}
		if n.ManagerStatus != nil {
			row.Leader = n.ManagerStatus.Leader
			row.Reachability = string(n.ManagerStatus.Reachability)
			row.ManagerAddr = n.ManagerStatus.Addr
		}
		out.Nodes = append(out.Nodes, row)
	}
	slices.SortFunc(out.Nodes, func(a, b NodeRow) int {
		return cmp.Or(cmp.Compare(a.Hostname, b.Hostname), cmp.Compare(a.ID, b.ID))
	})

	ss, err := cli.ServiceList(ctx, client.ServiceListOptions{Status: true})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list swarm services on %s: %w", c.t.Describe(), err)
	}
	out.Services = make([]ServiceRow, 0, len(ss.Items))
	var agentServices []string
	for _, s := range ss.Items {
		if isAgentService(s) {
			agentServices = append(agentServices, s.ID)
		}
		row := ServiceRow{
			ID: s.ID, Name: s.Spec.Name, Mode: serviceMode(s.Spec.Mode), UpdatedAt: s.UpdatedAt.UTC(),
			Stack: s.Spec.Labels[stackLabel], Agent: isAgentService(s),
		}
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
			row.Image = cs.Image
		}
		if s.ServiceStatus != nil {
			row.Running, row.Desired = s.ServiceStatus.RunningTasks, s.ServiceStatus.DesiredTasks
		}
		if s.UpdateStatus != nil {
			row.UpdateState = string(s.UpdateStatus.State)
			row.UpdateMessage = s.UpdateStatus.Message
		}
		row.HasPreviousSpec = s.PreviousSpec != nil
		row.CreatedAt = s.CreatedAt.UTC()
		row.Labels = s.Spec.Labels
		row.Ports = servicePorts(s.Endpoint.Ports)
		row.Constraints, row.Preferences = []string{}, []string{}
		if pl := s.Spec.TaskTemplate.Placement; pl != nil {
			row.Constraints = append(row.Constraints, pl.Constraints...)
			row.MaxReplicas = pl.MaxReplicas
			for _, pref := range pl.Preferences {
				if pref.Spread != nil {
					row.Preferences = append(row.Preferences, pref.Spread.SpreadDescriptor)
				}
			}
		}
		out.Services = append(out.Services, row)
	}
	slices.SortFunc(out.Services, func(a, b ServiceRow) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	if out.ServiceRefs, err = c.serviceRefsOf(ctx, cli, ss.Items); err != nil {
		return ClusterRead{}, err
	}

	if len(agentServices) == 0 {
		return out, nil // no agent service, so no agent tasks to ask for
	}
	// The services are named by ID: a service filter that names none would list
	// every task, and an ID cannot be mistaken for another service's name.
	ts, err := cli.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", agentServices...)})
	if err != nil {
		return ClusterRead{}, fmt.Errorf("list the agent service's tasks on %s: %w", c.t.Describe(), err)
	}
	out.AgentTasks = currentAgentTasks(ts.Items)
	return out, nil
}

// nonNilLabels returns the labels as a map that is never nil, so a consumer can range
// over it and encode it as an object.
func nonNilLabels(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return maps.Clone(in)
}

// serviceMode names a service's mode.
func serviceMode(m swarm.ServiceMode) string {
	switch {
	case m.Global != nil:
		return "global"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	case m.GlobalJob != nil:
		return "global-job"
	default:
		return "replicated"
	}
}

// currentAgentTasks keeps the newest task of each node: a swarm keeps the history
// of a service's tasks, and only the latest says how the agent is doing there.
func currentAgentTasks(tasks []swarm.Task) []AgentTaskRow {
	newest := map[string]swarm.Task{}
	for _, t := range tasks {
		if t.NodeID == "" {
			continue // not scheduled yet
		}
		if cur, ok := newest[t.NodeID]; !ok || t.CreatedAt.After(cur.CreatedAt) || t.CreatedAt.Equal(cur.CreatedAt) && t.ID > cur.ID {
			newest[t.NodeID] = t
		}
	}
	rows := make([]AgentTaskRow, 0, len(newest))
	for node, t := range newest {
		rows = append(rows, AgentTaskRow{NodeID: node, State: string(t.Status.State), Error: t.Status.Err})
	}
	slices.SortFunc(rows, func(a, b AgentTaskRow) int { return cmp.Compare(a.NodeID, b.NodeID) })
	return rows
}

// RestartPolicy is the restart policy of one container.
type RestartPolicy struct {
	Name     string
	MaxRetry int
}

// RestartPolicies inspects each container and returns only its restart policy.
// The inspect answer also carries the container's environment variables and
// mounts, which can hold secrets, and none of it is kept. A container with no
// policy is reported as "no". An unknown container is an error; the caller asks
// for IDs it has just listed.
func (c *Client) RestartPolicies(ctx context.Context, ids []string) (map[string]RestartPolicy, error) {
	cli := c.cur()
	out := make(map[string]RestartPolicy, len(ids))
	for _, id := range ids {
		res, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("inspect container %s on %s: %w", id, c.t.Describe(), err)
		}
		p := RestartPolicy{Name: "no"}
		if hc := res.Container.HostConfig; hc != nil && hc.RestartPolicy.Name != "" {
			p = RestartPolicy{Name: string(hc.RestartPolicy.Name), MaxRetry: hc.RestartPolicy.MaximumRetryCount}
		}
		out[id] = p
	}
	return out, nil
}

// ServiceRefs reads, from a swarm manager, the networks and volumes each service's spec
// mentions. It is read-only, and it is what a delete re-reads live just before it acts,
// so a service created a moment ago counts.
func (c *Client) ServiceRefs(ctx context.Context) ([]ServiceRef, error) {
	cli := c.cur()
	ss, err := cli.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list swarm services on %s: %w", c.t.Describe(), err)
	}
	return c.serviceRefsOf(ctx, cli, ss.Items)
}

// serviceRefsOf turns listed services into their references. The networks are listed
// once, and only when some service names one, to turn the names a spec may use into IDs.
func (c *Client) serviceRefsOf(ctx context.Context, cli *client.Client, services []swarm.Service) ([]ServiceRef, error) {
	targets := false
	for _, s := range services {
		if len(s.Spec.TaskTemplate.Networks) > 0 {
			targets = true
			break
		}
	}
	idOf := map[string]string{}
	if targets {
		ns, err := cli.NetworkList(ctx, client.NetworkListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list networks on %s: %w", c.t.Describe(), err)
		}
		for _, n := range ns.Items {
			if n.Scope == ScopeSwarm {
				idOf[n.Name] = n.ID
			}
		}
	}
	refs := make([]ServiceRef, 0, len(services))
	for _, s := range services {
		ref := ServiceRef{ID: s.ID, Name: s.Spec.Name, Networks: []string{}, Volumes: []string{}, VolumeMounts: []ServiceVolumeMount{}}
		for _, a := range s.Spec.TaskTemplate.Networks {
			target := a.Target
			if id, ok := idOf[target]; ok {
				target = id
			}
			if target != "" && !slices.Contains(ref.Networks, target) {
				ref.Networks = append(ref.Networks, target)
			}
		}
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs != nil {
			ref.Image = cs.Image
			for _, m := range cs.Mounts {
				if m.Type != mount.TypeVolume || m.Source == "" {
					continue
				}
				if !slices.Contains(ref.Volumes, m.Source) {
					ref.Volumes = append(ref.Volumes, m.Source)
				}
				ref.VolumeMounts = append(ref.VolumeMounts, ServiceVolumeMount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
			}
		}
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b ServiceRef) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return refs, nil
}
