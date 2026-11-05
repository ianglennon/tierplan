// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/updates"
)

// Stable problem types of the environment data routes. The SPA maps each to a
// full-page state (D-58).
const (
	problemEnvironmentOffline     = "environment_offline"
	problemEnvironmentUnsupported = "environment_unsupported"
	// problemAgentPolicyDenied is an agent that refused a read by its own policy:
	// the engine is fine, and the detail names what was refused (D-29).
	problemAgentPolicyDenied = "agent_policy_denied"
)

// maxListRows is the most rows a list answers with (D-56). The total is always
// the full count, so the page can say "Showing the first 500 of {n}".
const maxListRows = 500

// minSelfIDLen is the shortest container ID prefix trusted to identify
// TierPlan's own container: the 12 characters Docker uses as the default
// hostname.
const minSelfIDLen = 12

// AggregateSource is what the dashboard routes read: the single path from an
// environment to its Docker data. *envs.Aggregator implements it. A handler never
// reaches the monitor or the snapshot cache by environment, because an agent
// environment has no monitor of its own (D-47).
type AggregateSource interface {
	Snapshot(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error)
}

// DashboardDeps are the dependencies of the summary and list routes.
type DashboardDeps struct {
	Aggregator AggregateSource
	// Clusters is the cluster reader of swarm environments: the networks list counts
	// the services that mention a network from its last read. Nil counts none.
	Clusters *envs.Clusters
	// Checker says whether the caller may read services, which the restart-loop count needs.
	// Nil leaves the count out.
	Checker *rbac.Checker
	// SelfContainerID is TierPlan's own container ID, for the isSelf flag. Empty
	// when TierPlan does not run in a container, or cannot tell.
	SelfContainerID string
	// Store holds the stacks a stack label is confirmed against (T-07-146). Nil links no stack.
	Store *store.Store
	// AddressSpaces reads how full the address pools of an environment are, for the pool
	// column of the networks list and the dashboard's address tile. Nil means the feature is
	// off: nothing is read and no pool is shown.
	AddressSpaces *envs.AddressSpaces
	// Updates judges the image of each container and counts the updates that wait. Nil means update
	// checks are not wired: no container carries an update object and the summary carries no count.
	Updates *updates.Service
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// EnvironmentProblemEngine names the engine an unsupported environment runs.
type EnvironmentProblemEngine struct {
	Name       string `json:"name,omitempty" doc:"For example Docker Engine or Podman"`
	Version    string `json:"version,omitempty" doc:"The engine version"`
	APIVersion string `json:"apiVersion,omitempty" doc:"The newest Engine API version the daemon speaks"`
}

// EnvironmentProblem is the RFC 9457 body of 503 environment_offline and 409
// environment_unsupported. The extra members drive the full-page states.
type EnvironmentProblem struct {
	huma.ErrorModel
	State  string                    `json:"state,omitempty" enum:"offline,unsupported,pending,reconnecting,revoked,cluster_changed" doc:"The environment's health state, so the page can say what is wrong: pending (no agent has enrolled), reconnecting (the server just started), offline, revoked, cluster_changed or unsupported"`
	Since  string                    `json:"since,omitempty" format:"date-time" doc:"When the environment entered this state; for offline, when contact was lost"`
	Reason string                    `json:"reason,omitempty" doc:"Why, as a stable code. An agent environment uses agent_offline, timeout, engine_unreachable, unsupported_engine, revoked, cluster_changed, not_a_manager or no_quorum; the local socket uses unreachable, api_too_old, podman, rootless or docker_desktop"`
	Engine *EnvironmentProblemEngine `json:"engine,omitempty" doc:"environment_unsupported: the engine that was found"`
}

// SummaryEnvironmentBody identifies the environment a summary is for.
type SummaryEnvironmentBody struct {
	ID     string                `json:"id" doc:"The environment ID"`
	Name   string                `json:"name" doc:"The display name"`
	Type   string                `json:"type,omitempty" enum:"swarm-manager,swarm-worker,standalone" doc:"What the engine is"`
	Status EnvironmentStatusBody `json:"status"`
}

// SummaryEngineBody is the engine card of the dashboard (D-55).
type SummaryEngineBody struct {
	Name          string `json:"name,omitempty" doc:"For example Docker Engine"`
	Version       string `json:"version,omitempty" doc:"The engine version"`
	APIVersion    string `json:"apiVersion,omitempty" doc:"The newest Engine API version the daemon speaks"`
	OS            string `json:"os,omitempty" doc:"The operating system, for example linux"`
	Arch          string `json:"arch,omitempty" doc:"The CPU architecture, for example amd64"`
	StorageDriver string `json:"storageDriver,omitempty" doc:"The storage driver, for example overlayfs"`
	NCPU          int    `json:"ncpu" doc:"The number of CPUs"`
	MemTotal      int64  `json:"memTotal" doc:"Total memory in bytes"`
}

// SummaryCountsBody holds the dashboard tiles. Each is the length of the list
// its tile links to, because both come from one snapshot (Pitfall 22).
type SummaryCountsBody struct {
	ContainersRunning int  `json:"containersRunning" doc:"Containers in state running"`
	ContainersStopped int  `json:"containersStopped" doc:"Containers in any other state, including created, paused and exited"`
	Containers        int  `json:"containers" doc:"All containers, stopped ones included"`
	Images            int  `json:"images"`
	Volumes           int  `json:"volumes"`
	Networks          int  `json:"networks"`
	Nodes             *int `json:"nodes,omitempty" doc:"Swarm nodes; present on swarm managers only"`
	Services          *int `json:"services,omitempty" doc:"Swarm services; present on swarm managers only"`
}

// NodeCountsBody is what one node of an environment holds.
type NodeCountsBody struct {
	Node       string `json:"node" doc:"The node's host name"`
	Containers int    `json:"containers" doc:"Containers on this node, stopped ones included"`
	Images     int    `json:"images"`
	Volumes    int    `json:"volumes"`
	Networks   int    `json:"networks" doc:"Networks local to this node; a swarm-wide network is counted in the total only"`
}

// EnvironmentSummaryBody is the dashboard of one environment.
type EnvironmentSummaryBody struct {
	Environment     SummaryEnvironmentBody `json:"environment"`
	Engine          SummaryEngineBody      `json:"engine"`
	Counts          SummaryCountsBody      `json:"counts"`
	ByNode          []NodeCountsBody       `json:"byNode" doc:"Each node that answered, by name, with its own counts; a standalone host or the local socket is one node"`
	NotReporting    []string               `json:"notReporting" doc:"Nodes that should have answered and did not: no agent connected, or its read failed or timed out. Their rows are left out of the counts, so when this is not empty the counts are not the whole cluster"`
	IsWorker        bool                   `json:"isWorker" doc:"True on a swarm worker, which can show only its own containers, images, volumes and networks (D-24)"`
	SelfContainerID string                 `json:"selfContainerId,omitempty" doc:"The ID of TierPlan's own container, when it can tell (D-27)"`
	FetchedAt       string                 `json:"fetchedAt" format:"date-time" doc:"When the counts were read from Docker; they are cached for a few seconds"`
	RestartLoops    *RestartLoopCountsBody `json:"restartLoops,omitempty" doc:"Swarm managers only, and only for a caller who may read services: how many services are in a restart loop, from the last cluster read. Absent on a standalone host, on a worker, and while the swarm has not been read or has lost its quorum"`
	ImageUpdates    *int                   `json:"imageUpdates,omitempty" doc:"How many swarm services and containers have an update available (a task container counts as its service; skipped and ignored updates are not counted), from the stored image checks. Only for a caller who may read containers, and services on a swarm manager. Absent when it could not be read, so the dashboard says so instead of showing 0"`
}

// RestartLoopCountsBody counts the services in a restart loop (RES-13, D-15).
type RestartLoopCountsBody struct {
	Flagged int `json:"flagged" doc:"Services in a restart loop whose alerts are not muted"`
	Muted   int `json:"muted" doc:"Services in a restart loop whose alerts are muted"`
}

// EnvironmentSummaryOutput wraps EnvironmentSummaryBody for huma.
type EnvironmentSummaryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvironmentSummaryBody
}

// ContainerItem is one row of the containers list. It carries no variables, mounts
// or labels except the three that group a container: project, stack and service
// (T-01-77, T-04-29).
type ContainerItem struct {
	Node            string   `json:"node" doc:"The node the container runs on"`
	NodeID          string   `json:"nodeId" doc:"The ID of that node: what a write is routed by, since two hosts can share a display name"`
	ID              string   `json:"id" doc:"The full container ID"`
	Name            string   `json:"name" doc:"The container name without the leading slash"`
	State           string   `json:"state" enum:"created,running,paused,restarting,removing,exited,dead" doc:"The container state"`
	Status          string   `json:"status" doc:"The engine's status text, for example Up 3 hours"`
	Image           string   `json:"image" doc:"The image the container was created from"`
	Created         string   `json:"created,omitempty" format:"date-time"`
	Ports           []string `json:"ports" doc:"Ports as 0.0.0.0:8080->80/tcp, or 80/tcp when not published"`
	IsSelf          bool     `json:"isSelf" doc:"True for the container TierPlan itself runs in (D-27)"`
	Health          string   `json:"health" enum:"none,starting,healthy,unhealthy" doc:"The container's health: none when it has no healthcheck"`
	SelfKind        string   `json:"selfKind" enum:",server,agent" doc:"server for the container TierPlan itself runs in, agent for a TierPlan agent's container (marked by the label tierplan.agent=true), empty otherwise. Only adds care in the UI; the action endpoint decides again from a fresh inspect"`
	ReadOnly        bool     `json:"readOnly" doc:"True when the host of this container's node is read-only, so no change to it will be accepted"`
	Project         string   `json:"project" doc:"The compose project (com.docker.compose.project); empty when the container has none"`
	Stack           string   `json:"stack" doc:"The swarm stack namespace (com.docker.stack.namespace); empty when the container has none"`
	Service         string   `json:"service" doc:"The swarm service name (com.docker.swarm.service.name); empty when the container has none"`
	TierPlanStackID string   `json:"tierplanStackId" doc:"The ID of the TierPlan stack the container belongs to: set only when this environment holds a stack with that ID under the stack or project name the container carries (tierplan.stack.id). Empty for a stack or project TierPlan does not manage"`
	// RestartLoop is present while the container keeps dying and is restarted by its policy.
	// A task container of a swarm service never has one: the service does.
	RestartLoop *ContainerRestartLoopBody `json:"restartLoop,omitempty" doc:"Present while the container is in a restart loop: it died with a non-zero exit code the environment's threshold of times inside its window. Absent otherwise"`
	// Update is the state of the container's image; absent when update checks are not wired.
	Update *UpdateInfoBody `json:"update,omitempty" doc:"The image update state of the container, judged from the digest of the image it runs on its own node and the stored check of the tag it was created from. Absent when update checks are not wired"`
}

// VolumeItem is one row of the volumes list.
type VolumeItem struct {
	Node    string `json:"node" doc:"The node that holds the volume"`
	NodeID  string `json:"nodeId" doc:"The ID of that node: what a write is routed by, since two hosts can share a display name"`
	Name    string `json:"name"`
	Driver  string `json:"driver"`
	Created string `json:"created,omitempty" format:"date-time"`
	// UsedByContainers and UsedByServices are what the volume cannot be deleted while
	// any of them exist.
	UsedByContainers int    `json:"usedByContainers" doc:"Containers on this node, running or not, that mount the volume"`
	UsedByServices   int    `json:"usedByServices" doc:"Swarm services whose spec mounts a volume of this name, even with no running task, as of the last cluster read. A service can run a task on any node, so it counts for the volume on every node. Always 0 off a swarm"`
	Anonymous        bool   `json:"anonymous" doc:"True for a volume Docker made for a container without a name: it has the com.docker.volume.anonymous label, or on an engine without it a name of 64 hexadecimal characters"`
	Project          string `json:"project" doc:"The compose project (com.docker.compose.project); empty when the volume has none"`
	Stack            string `json:"stack" doc:"The swarm stack namespace (com.docker.stack.namespace); empty when the volume has none"`
}

// NetworkItem is one row of the networks list.
type NetworkItem struct {
	Node    string   `json:"node" doc:"The node the network is local to, or Cluster for a swarm-wide network"`
	NodeID  string   `json:"nodeId" doc:"The ID of the node the network was read from; for a swarm-wide network, the manager in use"`
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Driver  string   `json:"driver"`
	Scope   string   `json:"scope" doc:"local, or swarm for a cluster-wide network"`
	Subnets []string `json:"subnets" doc:"The IPAM subnets in CIDR notation; empty when the network has none"`
	Created string   `json:"created,omitempty" format:"date-time"`
	BuiltIn bool     `json:"builtIn" doc:"True for bridge, host, none, ingress, docker_gwbridge and any ingress network: the engine's own, which can never be deleted"`
	// AttachedContainers and AttachedServices are what the network cannot be deleted
	// while any of them exist.
	AttachedContainers int    `json:"attachedContainers" doc:"Containers, running or not, on any reporting node that are attached to the network. A local network counts the containers of its own node"`
	AttachedServices   int    `json:"attachedServices" doc:"Swarm services whose spec mentions the network, even with no running task, as of the last cluster read. Always 0 off a swarm and for a local network"`
	Owner              string `json:"owner" doc:"The stack (com.docker.stack.namespace) or compose project (com.docker.compose.project) label value; empty when the network has neither"`
	OwnerKind          string `json:"ownerKind" enum:"stack,project," doc:"stack, project, or empty when the network has no owner"`
	Internal           bool   `json:"internal" doc:"No access to outside networks"`
	Attachable         bool   `json:"attachable" doc:"Standalone containers can join it"`
	IPv6               bool   `json:"ipv6" doc:"IPv6 is enabled"`
	// AddressesUsed is read from the same cached address-space read as the address-space route.
	AddressesUsed *NetworkUsageBody `json:"addressesUsed,omitempty" doc:"Overlay networks only: addresses used against what the subnet can still hand out, from the manager (Engine 29 and later). Absent (null in the table) for every other network and when the count could not be read, never a zero"`
}

// NetworkUsageBody is how many of an overlay network's addresses are handed out.
type NetworkUsageBody struct {
	Used     int64  `json:"used" minimum:"0" doc:"Addresses the swarm has handed out on the network's IPv4 subnets"`
	Capacity int64  `json:"capacity" minimum:"1" doc:"Addresses handed out plus those it can still hand out"`
	Percent  int    `json:"percent" minimum:"0" maximum:"100" doc:"used over capacity as a whole percent, rounded down"`
	State    string `json:"state" enum:"normal,getting_full,almost_full" doc:"normal under 70%, getting_full from 70% and almost_full from 90%, decided on the exact ratio"`
}

// ContainerListBody and its siblings are a capped list with the full total.
type ContainerListBody struct {
	Items        []ContainerItem `json:"items" doc:"At most 500 rows"`
	Total        int             `json:"total" doc:"How many match the search, whatever the cap"`
	Truncated    bool            `json:"truncated" doc:"True when total is more than the rows returned"`
	Nodes        []string        `json:"nodes" doc:"The nodes that answered, for the node filter; Cluster is listed too when swarm-wide networks are shown"`
	NotReporting []string        `json:"notReporting" doc:"Nodes that should have answered and did not, so these rows are not the whole cluster"`
	// UnfilteredTotal is the count after the node filter and before the search.
	UnfilteredTotal int `json:"unfilteredTotal" doc:"How many containers there are before the search and the status filter are applied (after the node filter), so a page can say 3 of 600"`
	// StatusCounts is counted before the search and the status filter.
	StatusCounts StatusCountsBody `json:"statusCounts" doc:"How many containers each status chip counts, after the node filter and before the search and the status filter, so the numbers never shrink with what was typed"`
}

// VolumeListBody is a capped list of volumes.
type VolumeListBody struct {
	Items        []VolumeItem `json:"items" doc:"At most 500 rows"`
	Total        int          `json:"total" doc:"How many there are, whatever the cap"`
	Truncated    bool         `json:"truncated" doc:"True when total is more than the rows returned"`
	Nodes        []string     `json:"nodes" doc:"The nodes that answered, for the node filter; Cluster is listed too when swarm-wide networks are shown"`
	NotReporting []string     `json:"notReporting" doc:"Nodes that should have answered and did not, so these rows are not the whole cluster"`
}

// NetworkListBody is a capped list of networks.
type NetworkListBody struct {
	Items        []NetworkItem `json:"items" doc:"At most 500 rows"`
	Total        int           `json:"total" doc:"How many there are, whatever the cap"`
	Truncated    bool          `json:"truncated" doc:"True when total is more than the rows returned"`
	Nodes        []string      `json:"nodes" doc:"The nodes that answered, for the node filter; Cluster is listed too when swarm-wide networks are shown"`
	NotReporting []string      `json:"notReporting" doc:"Nodes that should have answered and did not, so these rows are not the whole cluster"`
}

// ContainerListOutput wraps ContainerListBody for huma.
type ContainerListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerListBody
}

// VolumeListOutput wraps VolumeListBody for huma.
type VolumeListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeListBody
}

// NetworkListOutput wraps NetworkListBody for huma.
type NetworkListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         NetworkListBody
}

// ContainerListInput is the containers list: the node filter and the search.
type ContainerListInput struct {
	NodeFilterInput
	Q       string `query:"q" maxLength:"128" doc:"Search: a case-insensitive substring of the name, image, compose project, stack, service or any label key or value. Applied before the 500-row cap"`
	Status  string `query:"status" maxLength:"64" doc:"Only containers with one of these statuses, comma separated: running, exited, paused, restarting or unhealthy (a health, not a state). Applied before the 500-row cap; any other value is refused 422 invalid_status"`
	Updates string `query:"updates" enum:",available" doc:"available: only containers whose image has an update available. Applied before the 500-row cap. Answers 503 when the checks cannot be read, never an empty list"`
}

// NetworkListInput is the networks list: the node filter and the search.
type NetworkListInput struct {
	NodeFilterInput
	Q string `query:"q" maxLength:"128" doc:"Search: a case-insensitive substring of the name, driver, a subnet or the owner (stack or compose project). Applied before the 500-row cap"`
}

// VolumeListInput is the path of the volumes list with its node filter, its search and its
// usage filter.
type VolumeListInput struct {
	NodeFilterInput
	Q     string `query:"q" maxLength:"128" doc:"Search: a case-insensitive substring of the name, driver, a label (its name or its value) or the stack or compose project. Applied before the 500-row cap"`
	Usage string `query:"usage" enum:"all,used,unused" default:"all" doc:"used: only volumes a container or a swarm service mounts. unused: only volumes nothing mounts. Applied before the 500-row cap"`
}

// NodeFilterInput is the path of a list and its optional node filter (T-02-74).
type NodeFilterInput struct {
	EnvironmentPathInput
	Node string `query:"node" maxLength:"128" doc:"Only the rows of this node, matched exactly against the node names the list reports. A name that is not one of them gives an empty list"`
}

// filterNode keeps the rows of node, all of them when node is empty. The filter is
// applied before the 500-row cap, so the total is the filtered count.
func filterNode[R any](rows []R, node string, nodeOf func(R) string) []R {
	if node == "" {
		return rows
	}
	out := make([]R, 0, len(rows))
	for _, r := range rows {
		if nodeOf(r) == node {
			out = append(out, r)
		}
	}
	return out
}

// reportingNodes are the names a node filter can take: every node that answered.
func reportingNodes(agg envs.Aggregate, extra ...string) []string {
	out := make([]string, 0, len(agg.Nodes)+len(extra))
	for _, n := range agg.Nodes {
		out = append(out, n.Name)
	}
	out = append(out, extra...)
	slices.Sort(out)
	return slices.Compact(out)
}

// clusterNode is NodeCluster when any network row carries it, for the filter.
func clusterNode(rows []envs.NetworkRow) []string {
	for _, n := range rows {
		if n.Node == envs.NodeCluster {
			return []string{envs.NodeCluster}
		}
	}
	return nil
}

// capRows converts at most maxListRows rows and reports the full total.
func capRows[R, T any](rows []R, conv func(R) T) (items []T, total int, truncated bool) {
	total = len(rows)
	n := min(total, maxListRows)
	items = make([]T, 0, n)
	for _, r := range rows[:n] {
		items = append(items, conv(r))
	}
	return items, total, total > n
}

// problemResponses documents the two problems on the OpenAPI operation, with
// the shared EnvironmentProblem schema.
func problemResponses(r *httpx.Router) map[string]*huma.Response {
	schema := r.API().OpenAPI().Components.Schemas.Schema(reflect.TypeFor[EnvironmentProblem](), true, "EnvironmentProblem")
	media := map[string]*huma.MediaType{"application/problem+json": {Schema: schema}}
	return map[string]*huma.Response{
		"503": {Description: "environment_offline: the environment cannot be read. Carries its state (pending, reconnecting, offline, revoked or cluster_changed), the reason code and since.", Content: media},
		"502": {Description: "agent_policy_denied: the environment's agent refused a read by its own policy. The detail names the endpoint.", Content: media},
		"409": {Description: "environment_unsupported: the engine answers but TierPlan does not manage it. Carries the state, the engine and the reason.", Content: media},
	}
}

// RegisterDashboard registers the environment summary and the four read-only
// lists under /api/env/{envId}/. Each needs its own permission on the
// environment (D-48); the counts and the lists come from one short-lived
// snapshot read live from Docker (D-57, Pitfall 22). An offline environment
// answers 503 environment_offline and an unsupported one 409
// environment_unsupported, so the SPA shows the full-page state (D-25, D-58). The
// data of every environment is read through the aggregator, which knows an agent
// environment's targets (D-47).
func RegisterDashboard(r *httpx.Router, d DashboardDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	problems := problemResponses(r)

	// read fetches the snapshot through the aggregator, turning the environment's
	// health into the problems the SPA understands.
	read := func(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error) {
		agg, health, err := d.Aggregator.Snapshot(ctx, envID)
		// An agent that refuses a read says so in the message of the Docker error.
		err = envs.ClassifyDockerError(err)
		var denied *envs.AgentPolicyDeniedError
		var down envs.ErrNotOnline
		switch {
		case err == nil:
			return agg, health, nil
		case errors.As(err, &denied):
			// Not an outage: the host's policy refused it, which is either a TierPlan
			// bug or a server asking for more than it should.
			d.Log.Warn("an agent refused a read of an environment", "environment", envID, "endpoint", denied.Endpoint)
			return agg, health, httpx.Problem(http.StatusBadGateway, problemAgentPolicyDenied,
				fmt.Sprintf("The agent refused a request (%s).", denied.Endpoint))
		case errors.As(err, &down):
			return agg, health, notOnlineProblem(down)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The caller gave up, or the read ran out of time. Say so without
			// blaming the engine's status.
			return agg, health, httpx.Problem(http.StatusServiceUnavailable, "", "The environment took too long to answer.")
		default:
			// The engine answered its last probe but not this read. The snapshot
			// cache has already asked the monitor to probe again.
			d.Log.Error("reading an environment failed", "environment", envID, "error", err)
			// There is no honest "offline since" yet, so it is left out.
			return agg, health, offlineProblem(time.Time{}, dockerx.ReasonUnreachable)
		}
	}
	isSelf := func(id string) bool {
		return len(d.SelfContainerID) >= minSelfIDLen && strings.HasPrefix(id, d.SelfContainerID)
	}
	need := func(p rbac.Perm) rbac.Requirement {
		return rbac.Requirement{Perm: p, Scope: rbac.ScopeEnvironment}
	}
	op := func(id, path, summary, description string) huma.Operation {
		// huma adds the 200 response into this map when the operation is
		// registered, so each operation needs its own copy: a shared one keeps the
		// first operation's 200 schema and every later list documents the summary.
		responses := maps.Clone(problems)
		return huma.Operation{
			OperationID: id, Method: http.MethodGet, Path: path, Summary: summary,
			Description: description + " 503 environment_offline (with the state, a reason code and since) when the environment cannot be read: no agent has enrolled, the server is waiting for its agents, they are all revoked or offline, or the swarm was re-initialised; " +
				"409 environment_unsupported (with the engine) when it is not one TierPlan manages, " +
				"502 agent_policy_denied (the detail names the endpoint) when the agent refuses a read by its own policy.",
			Tags: []string{"environments"}, Responses: responses,
		}
	}

	httpx.Register(r, op("getEnvironmentSummary", "/api/env/{envId}/summary",
		"The dashboard of an environment",
		"The engine card and the counts of running and stopped containers, images, volumes and networks, plus nodes and "+
			"services on a swarm manager. Every count is the length of the list its tile links to, because both come from "+
			"the same snapshot, cached for a few seconds. On a swarm the counts are the cluster's totals over the nodes that "+
			"answered, with each node's own counts in byNode; notReporting names the nodes that did not, and while it is not "+
			"empty the totals are not the whole cluster."),
		need(rbac.PermEnvironmentsRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*EnvironmentSummaryOutput, error) {
			agg, health, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			p, c := agg.Target.Probe, agg.Counts
			var loops *RestartLoopCountsBody
			if c.Services != nil {
				loops = restartLoopCounts(ctx, d, env.ID)
			}
			imageUpdates := imageUpdateCount(ctx, d, env.ID, c.Services != nil)
			return &EnvironmentSummaryOutput{CacheControl: "no-store", Body: EnvironmentSummaryBody{
				Environment: SummaryEnvironmentBody{
					ID: env.ID, Name: env.Name, Type: p.Type,
					Status: statusBody(health, agg.Target.LastAttempt),
				},
				Engine: SummaryEngineBody{
					Name: p.EngineName, Version: p.EngineVersion, APIVersion: p.APIVersion, OS: p.OS, Arch: p.Arch,
					StorageDriver: p.StorageDriver, NCPU: p.NCPU, MemTotal: p.MemTotal,
				},
				Counts: SummaryCountsBody{
					ContainersRunning: c.ContainersRunning, ContainersStopped: c.ContainersStopped, Containers: c.Containers,
					Images: c.Images, Volumes: c.Volumes, Networks: c.Networks, Nodes: c.Nodes, Services: c.Services,
				},
				ByNode:          byNode(agg),
				NotReporting:    agg.NotReporting,
				IsWorker:        p.Type == dockerx.TypeSwarmWorker,
				SelfContainerID: d.SelfContainerID,
				FetchedAt:       rfc3339(agg.FetchedAt),
				RestartLoops:    loops,
				ImageUpdates:    imageUpdates,
			}}, nil
		})

	httpx.Register(r, op("listContainers", "/api/env/{envId}/containers",
		"List the containers of an environment",
		"Every container, stopped ones included, by name. On a swarm: those of every node that answered, each with its node, so a name used on two nodes is two rows; the nodes that did not answer are named in notReporting. "+
			"Read-only rows: no variables or mounts, and of the labels only the compose project, stack and service. Optionally only one node's (node) and a search (q) and a status filter (status). Both run on the server before the 500-row cap, so at most 500 rows come back after them, with total the number that matched, unfilteredTotal the number before the search and the filter, and statusCounts the chip numbers, counted before them too. Each row carries its health, selfKind (server or agent for TierPlan's own containers) and readOnly (its host refuses changes), and its update object (what is known about its image, from the stored checks; updates=available keeps only the containers with an update available, before the cap)."),
		need(rbac.PermContainersRead),
		func(ctx context.Context, in *ContainerListInput) (*ContainerListOutput, error) {
			agg, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			statuses, err := parseStatuses(in.Status)
			if err != nil {
				return nil, err
			}
			readOnly := map[string]bool{}
			for _, n := range agg.Nodes {
				readOnly[n.ID] = n.ReadOnly
			}
			onNode := filterNode(agg.Containers, in.Node, func(c envs.ContainerRow) string { return c.Node })
			rows := filterContainers(onNode, in.Q, statuses)
			judged, judgeErr := containerUpdates(ctx, d.Updates, d.Log, in.EnvID, agg)
			if in.Updates == "available" {
				if judgeErr != nil {
					return nil, updatesUnread()
				}
				rows = slices.DeleteFunc(slices.Clone(rows), func(c envs.ContainerRow) bool {
					return judged.Verdicts[c.ID].State != updates.StateUpdateAvailable
				})
			}
			loops, _ := containerLoopsOf(d.Clusters, in.EnvID)
			known := loadStackIDs(ctx, d.Store, in.EnvID, d.Log)
			items, total, truncated := capRows(rows, func(c envs.ContainerRow) ContainerItem {
				var loop *ContainerRestartLoopBody
				if l, flagged := loops[envs.ContainerLoopKey(c.NodeID, c.Name)]; flagged {
					loop = containerRestartLoopBody(l)
				}
				return ContainerItem{
					RestartLoop: loop,
					Node:        c.Node, NodeID: c.NodeID, ID: c.ID, Name: c.Name, State: c.State, Status: c.Status, Image: c.Image,
					Created: rfc3339(c.Created), Ports: c.Ports, IsSelf: isSelf(c.ID),
					Health: c.Health, SelfKind: selfKindOf(isSelf(c.ID), c.Labels), ReadOnly: readOnly[c.NodeID],
					Project: c.Labels[labelProject], Stack: c.Labels[labelStack], Service: c.Labels[labelService],
					TierPlanStackID: known.confirmedFor(containerStack(c.Labels), c.Labels),
					Update:          updateOfContainer(judged, c.ID, known),
				}
			})
			return &ContainerListOutput{CacheControl: "no-store", Body: ContainerListBody{
				Items: items, Total: total, Truncated: truncated, Nodes: reportingNodes(agg), NotReporting: agg.NotReporting,
				UnfilteredTotal: len(onNode), StatusCounts: statusCounts(onNode),
			}}, nil
		})

	httpx.Register(r, op("listVolumes", "/api/env/{envId}/volumes",
		"List the volumes of an environment",
		"By name, then node. On a swarm every node's volumes, each with its node: the same name on two nodes is two volumes. "+
			"Each row says what uses it (usedByContainers: containers on its node, stopped ones included; usedByServices: swarm services whose spec mounts that name), whether Docker made it without a name (anonymous), and its compose project and stack. "+
			"Optionally only one node's (node), a search (q) and a usage filter (usage: used or unused). The node, the search and the usage filter run on the server before the 500-row cap, so at most 500 rows come back after them, with the total the number that matched."),
		need(rbac.PermVolumesRead),
		func(ctx context.Context, in *VolumeListInput) (*VolumeListOutput, error) {
			agg, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			onNode := filterNode(agg.Volumes, in.Node, func(v envs.VolumeRow) string { return v.Node })
			use := volumeUsageOf(ctx, d.Clusters, in.EnvID, agg)
			rows := filterVolumes(onNode, in.Q, in.Usage, use)
			items, total, truncated := capRows(rows, func(v envs.VolumeRow) VolumeItem {
				containers, services := use.of(v)
				return VolumeItem{
					Node: v.Node, NodeID: v.NodeID, Name: v.Name, Driver: v.Driver, Created: rfc3339(v.Created),
					UsedByContainers: containers, UsedByServices: services, Anonymous: v.Anonymous,
					Project: v.Labels[labelProject], Stack: v.Labels[labelStack],
				}
			})
			return &VolumeListOutput{CacheControl: "no-store", Body: VolumeListBody{
				Items: items, Total: total, Truncated: truncated, Nodes: reportingNodes(agg), NotReporting: agg.NotReporting,
			}}, nil
		})

	httpx.Register(r, op("listNetworks", "/api/env/{envId}/networks",
		"List the networks of an environment",
		"By name. On a swarm a swarm-wide network is listed once, from the manager in use, with node Cluster; local networks are listed per node. Optionally only one node's (node). At most 500 rows, after the filter, with the total."),
		need(rbac.PermNetworksRead),
		func(ctx context.Context, in *NetworkListInput) (*NetworkListOutput, error) {
			agg, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			onNode := filterNode(agg.Networks, in.Node, func(n envs.NetworkRow) string { return n.Node })
			rows := filterNetworks(onNode, in.Q)
			use := networkUsageOf(ctx, d.Clusters, in.EnvID, agg)
			fullness := overlayFullness(ctx, d, in.EnvID, rows)
			items, total, truncated := capRows(rows, func(n envs.NetworkRow) NetworkItem {
				owner, kind := networkOwner(n.Labels)
				containers, services := use.of(n)
				return NetworkItem{
					AddressesUsed: fullness[n.ID],
					Node:          n.Node, NodeID: n.NodeID, ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Subnets: n.Subnets, Created: rfc3339(n.Created),
					BuiltIn: dockerx.IsBuiltinNetwork(n.Name, n.Ingress), AttachedContainers: containers, AttachedServices: services,
					Owner: owner, OwnerKind: kind, Internal: n.Internal, Attachable: n.Attachable, IPv6: n.IPv6,
				}
			})
			return &NetworkListOutput{CacheControl: "no-store", Body: NetworkListBody{
				Items: items, Total: total, Truncated: truncated,
				Nodes: reportingNodes(agg, clusterNode(agg.Networks)...), NotReporting: agg.NotReporting,
			}}, nil
		})
}

// overlayFullness is how full each swarm overlay of rows is, by network ID, from the address
// reader's short-lived read. A network that is not an overlay, or whose count could not be read,
// is not in the map. Nothing is read when no row is a swarm overlay, and a read that fails leaves
// every value out: the list never fails for a column.
func overlayFullness(ctx context.Context, d DashboardDeps, envID string, rows []envs.NetworkRow) map[string]*NetworkUsageBody {
	if d.AddressSpaces == nil || !slices.ContainsFunc(rows, func(n envs.NetworkRow) bool {
		return n.Scope == dockerx.ScopeSwarm && n.Driver == "overlay"
	}) {
		return nil
	}
	space, err := d.AddressSpaces.Read(ctx, envID)
	if err != nil {
		if d.Log != nil {
			d.Log.Debug("the address use of the overlay networks could not be read", "environment", envID, "error", err)
		}
		return nil
	}
	out := make(map[string]*NetworkUsageBody, len(space.Overlays))
	for _, o := range space.Overlays {
		if !o.Measured || o.Capacity == 0 {
			continue
		}
		out[o.ID] = &NetworkUsageBody{Used: addresses(o.Used), Capacity: addresses(o.Capacity), Percent: o.Percent, State: string(o.State)}
	}
	return out
}

// byNode lists each reporting node's counts by node name.
func byNode(agg envs.Aggregate) []NodeCountsBody {
	out := make([]NodeCountsBody, 0, len(agg.ByNode))
	for node, c := range agg.ByNode {
		out = append(out, NodeCountsBody{Node: node, Containers: c.Containers, Images: c.Images, Volumes: c.Volumes, Networks: c.Networks})
	}
	slices.SortFunc(out, func(a, b NodeCountsBody) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// offlineProblem is the 503 for an environment whose engine does not answer.
func offlineProblem(since time.Time, reason string) error {
	if reason == "" {
		reason = dockerx.ReasonUnreachable
	}
	return &EnvironmentProblem{
		ErrorModel: huma.ErrorModel{
			Type: problemEnvironmentOffline, Title: http.StatusText(http.StatusServiceUnavailable), Status: http.StatusServiceUnavailable,
			Detail: "The Docker engine of this environment is not answering.",
		},
		State:  string(envs.StateOffline),
		Since:  rfc3339(since),
		Reason: reason,
	}
}

// notOnlineDetail is the sentence a state carries when the SPA has not replaced it
// with its own copy, which it keys by state and reason.
var notOnlineDetail = map[envs.State]string{
	envs.StatePending:        "No agent has connected to this environment yet.",
	envs.StateReconnecting:   "TierPlan has just started and is waiting for the agents of this environment to reconnect.",
	envs.StateRevoked:        "Every agent of this environment was revoked.",
	envs.StateClusterChanged: "The swarm behind this environment was re-initialised.",
	envs.StateOffline:        "TierPlan cannot reach this environment.",
}

// notOnlineProblem is the problem for an environment that cannot be read: 409
// environment_unsupported with the engine for an unsupported one, and 503
// environment_offline, carrying the state, the D-40 reason and since, for every
// other state. The local socket keeps its Phase 1 answers: a host that has not
// been probed yet is offline, with the reason unreachable.
func notOnlineProblem(e envs.ErrNotOnline) error {
	h := e.Health
	if h.State == envs.StateUnsupported {
		p := e.Target.Probe
		return &EnvironmentProblem{
			ErrorModel: huma.ErrorModel{
				Type: problemEnvironmentUnsupported, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
				Detail: "TierPlan does not manage the Docker engine on this host.",
			},
			State:  string(envs.StateUnsupported),
			Since:  rfc3339(h.Since),
			Reason: h.Reason,
			Engine: &EnvironmentProblemEngine{Name: p.EngineName, Version: p.EngineVersion, APIVersion: p.APIVersion},
		}
	}
	state, reason := h.State, h.Reason
	if state == envs.StateUnknown {
		state = envs.StateOffline // not probed yet: nothing is known to answer
	}
	if state == envs.StateOffline && reason == "" {
		reason = dockerx.ReasonUnreachable
	}
	detail := notOnlineDetail[state]
	if state == envs.StateOffline && reason == dockerx.ReasonUnreachable {
		detail = "The Docker engine of this environment is not answering."
	}
	if detail == "" {
		detail = notOnlineDetail[envs.StateOffline]
	}
	return &EnvironmentProblem{
		ErrorModel: huma.ErrorModel{
			Type: problemEnvironmentOffline, Title: http.StatusText(http.StatusServiceUnavailable), Status: http.StatusServiceUnavailable,
			Detail: detail,
		},
		State:  string(state),
		Since:  rfc3339(h.Since),
		Reason: reason,
	}
}

// shortImageID is the first 12 characters of an image ID, as `docker images`
// shows it.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(len(id), 12)]
}

// imageUpdateCount counts the updates that wait in an environment: the services of a swarm manager and the
// containers that are not tasks. It is nil, and the tile says so, when update checks are not wired, the
// caller may not read what it counts (containers, and services on a swarm manager), or the count could not
// be read: never a 0 that is not known.
func imageUpdateCount(ctx context.Context, d DashboardDeps, envID string, swarm bool) *int {
	p := httpx.PrincipalFrom(ctx)
	if d.Updates == nil || d.Checker == nil || p == nil {
		return nil
	}
	may := func(perm rbac.Perm) bool {
		return d.Checker.Check(ctx, p, rbac.Requirement{Perm: perm, Scope: rbac.ScopeEnvironment}, envID).Allowed
	}
	if !may(rbac.PermContainersRead) || swarm && !may(rbac.PermServicesRead) {
		return nil
	}
	n, err := d.Updates.Count(ctx, envID)
	if err != nil {
		d.Log.Warn("could not count the image updates of an environment", "environment", envID, "error", err)
		return nil
	}
	return &n
}

// restartLoopCounts counts the restart loops of a swarm from the cluster reader's last read, for
// a caller who may read its services. It is nil when there is nothing to say: no reader, no
// checker, no right to read services, no read yet, or a lost quorum (which hides the loops).
func restartLoopCounts(ctx context.Context, d DashboardDeps, envID string) *RestartLoopCountsBody {
	p := httpx.PrincipalFrom(ctx)
	if d.Clusters == nil || d.Checker == nil || p == nil {
		return nil
	}
	if !d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment}, envID).Allowed {
		return nil
	}
	st, ok := d.Clusters.Cached(envID)
	if !ok || !st.Known() {
		return nil
	}
	out := &RestartLoopCountsBody{}
	for _, l := range st.RestartLoops {
		if l.Muted {
			out.Muted++
		} else {
			out.Flagged++
		}
	}
	return out
}
