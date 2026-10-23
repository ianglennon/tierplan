// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Result values and codes of one target of a container action. A code is a stable
// word the SPA maps to its own copy; the detail is only a fallback sentence.
const (
	actionDone    = "done"
	actionFailed  = "failed"
	actionSkipped = "skipped" // a target the request did not act on and did not fail: Bayplan's own container in a bulk

	codeOwnContainer     = "own_container"
	codeNodeUnreachable  = "node_unreachable"
	codeNotFound         = "not_found"
	codeAgentPolicyDeny  = problemAgentPolicyDenied
	codeHostReadOnly     = "host_read_only"
	codeActionFailed     = "action_failed"
	codeContainerRunning = "container_running"
	codeConnectionLost   = "connection_lost"
	// codeConfirmationRequired never appears in a result: a single target that needs its
	// name typed makes the whole request a 422 of this type.
	codeConfirmationRequired = "confirmation_required"
	maxActionDetail          = 300
	containerTargetType      = "container"
	ownerLabelAgent          = "bayplan.agent"
	ownerLabelAgentValue     = "true"

	// A target's fresh inspect gets inspectTimeout. Its write gets writeTimeout, or for a
	// stop or restart the stop timeout (the request's, else the container's own, else
	// defaultStopTimeout) plus stopGrace, because the engine waits that long before it
	// kills the container (D-09).
	inspectTimeout     = 30 * time.Second
	writeTimeout       = 30 * time.Second
	stopGrace          = 30 * time.Second
	defaultStopTimeout = 10 // seconds, the engine's own default
	// actionAuditTimeout bounds the write of one audit row for an action already done.
	actionAuditTimeout = 10 * time.Second

	problemOptionsSingleOnly = "options_single_only"
	problemNodeRequired      = "node_required"
	problemContainerNotFound = "container_not_found"
	problemNodeUnavailable   = "node_unavailable"

	// The audit stages of an action on one of Bayplan's own containers: the attempt is
	// recorded before the call, because the call may take the recorder down, and again
	// after it (RESEARCH Pitfall 10).
	stageInitiated      = "initiated"
	stageCompleted      = "completed"
	stageConnectionLost = "connection_lost"
	ownServer           = "server"
	ownAgent            = "agent"

	// A bulk runs at most perNodeCalls calls at once on one node and bulkParallel
	// targets at once overall, so 25 targets cannot flood one engine (T-04-25).
	perNodeCalls = 4
	bulkParallel = 16
)

// ContainersDeps are the dependencies of the container action routes.
type ContainersDeps struct {
	// Aggregator reaches a node's engine: reads through OnNode, writes through
	// WriteOnNode, the one write path.
	Aggregator *envs.Aggregator
	Audit      *audit.Writer
	// Checker decides the second-permission checks (volumes.delete on remove) and the
	// masking of the detail (secrets.reveal).
	Checker *rbac.Checker
	// SelfContainerID is Bayplan's own container ID; empty when it cannot tell.
	SelfContainerID string
	// Clusters is the cluster reader whose last read says which containers are in a restart
	// loop and which are muted (RES-13). Nil says none are.
	Clusters *envs.Clusters
	// Store holds the stacks a stack label is confirmed against (T-07-146). Nil links no stack.
	Store *store.Store
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// ContainerActionTarget names one container: the node it is on, by ID, and its ID.
// The node ID is never a display name, because two hosts can share one (T-02-73).
type ContainerActionTarget struct {
	NodeID string `json:"nodeId" minLength:"1" maxLength:"128" doc:"The node the container is on: the nodeId of its row in the containers list. Never the display name"`
	ID     string `json:"id" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The container ID or name"`
}

// ContainerActionRequest is the body of POST /api/env/{envId}/containers/actions.
type ContainerActionRequest struct {
	Action  string                  `json:"action" enum:"start,stop,restart,kill,pause,unpause" doc:"What to do. A stop or restart uses the container's own stop timeout and signal, and a kill sends SIGKILL, unless this is a single-target request that sets timeout or signal"`
	BulkID  string                  `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request. Send a UUID to name the bulk yourself; with more than one target and none sent, the server makes one (a UUIDv7) and answers it"`
	Timeout *int                    `json:"timeout,omitempty" minimum:"0" maximum:"86400" doc:"Seconds a stop or restart waits before it kills the container. Only with exactly one target (else 422 options_single_only); ignored by the other actions"`
	Signal  string                  `json:"signal,omitempty" pattern:"^[A-Z0-9+]{1,16}$" doc:"The signal a stop, restart or kill sends, for example SIGTERM. Only with exactly one target (else 422 options_single_only); ignored by the other actions"`
	Confirm string                  `json:"confirm,omitempty" maxLength:"256" doc:"The container's name, typed. Needed to stop, kill, restart or pause Bayplan's own server or an agent container, which only a single-target request may do (else 422 confirmation_required); with more than one target it is refused 422 options_single_only"`
	Targets []ContainerActionTarget `json:"targets" minItems:"1" maxItems:"25" doc:"The containers to act on, at most 25. Each is acted on and answered separately; one that fails never stops the others"`
}

// ContainerRemoveRequest is the body of POST /api/env/{envId}/containers/remove.
type ContainerRemoveRequest struct {
	BulkID        string                  `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request, as for the actions route"`
	Confirm       string                  `json:"confirm,omitempty" maxLength:"256" doc:"The container's name, typed. Needed to remove Bayplan's own server or an agent container, which only a single-target request may do (else 422 confirmation_required); with more than one target it is refused 422 options_single_only"`
	Force         bool                    `json:"force" doc:"Remove a running container too. Without it a running container is refused with container_running"`
	RemoveVolumes bool                    `json:"removeVolumes" doc:"Also remove the container's anonymous volumes; named volumes are never removed. Needs the delete volumes permission on top of containers.remove"`
	Targets       []ContainerActionTarget `json:"targets" minItems:"1" maxItems:"25" doc:"The containers to remove, at most 25. Each is answered separately; one that fails never stops the others"`
}

// ContainerRemoveInput is the path and body of the remove route.
type ContainerRemoveInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ContainerRemoveRequest
}

// ContainerActionInput is the path and body of the actions route.
type ContainerActionInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ContainerActionRequest
}

// ContainerActionResult is the answer for one target.
type ContainerActionResult struct {
	// own and stage are for the audit row only: which of Bayplan's own containers this is,
	// and how far an action on it got. They are not part of the contract.
	own, stage string

	NodeID string `json:"nodeId" doc:"The node ID the request named"`
	ID     string `json:"id" doc:"The container ID or name the request named"`
	Name   string `json:"name" doc:"The container name when the engine could be asked; otherwise the ID or name the request named"`
	Result string `json:"result" enum:"done,failed,skipped" doc:"done: the engine accepted the action. failed: it was refused or could not be sent; code says why. skipped: not acted on because it is one of Bayplan's own containers in a bulk"`
	Code   string `json:"code,omitempty" enum:"own_container,node_unreachable,not_found,host_read_only,agent_policy_denied,container_running,connection_lost,action_failed" doc:"Why a target failed. own_container (with result skipped): Bayplan's own server or an agent container in a request with more than one target, which is never acted on in a bulk; one is acted on alone with its name typed. connection_lost (with result done): the call to one of Bayplan's own containers ended because the connection dropped, which is what stopping it does; the action is taken as accepted, so refresh to see. node_unreachable: the node is not a connected node of this environment, or its engine did not answer; nothing was sent. not_found: the engine has no such container. container_running: a remove of a running container without force; nothing was sent. host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the node's agent, so Bayplan refused before sending anything. agent_policy_denied: the node's agent refused the call by its own policy, which is what an agent older than this server does for a call it does not know. action_failed: the engine refused it; detail has its message"`
	Detail string `json:"detail,omitempty" doc:"The engine's message for action_failed, at most 300 characters; for the other codes a plain sentence"`
}

// ContainerActionResponse is the per-target outcome, in request order.
type ContainerActionResponse struct {
	BulkID  string                  `json:"bulkId,omitempty" doc:"The bulk ID recorded in the audit rows of this request: the one sent, or the one the server made for a request with more than one target. Absent for a single target with none sent"`
	Results []ContainerActionResult `json:"results" doc:"One entry per requested target, in request order"`
}

// ContainerActionOutput wraps ContainerActionResponse for huma.
type ContainerActionOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerActionResponse
}

// ContainerDetailInput is the path and query of the container detail route.
type ContainerDetailInput struct {
	EnvironmentPathInput
	ContainerID string `path:"containerId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The container ID or name"`
	NodeID      string `query:"nodeId" maxLength:"128" doc:"The node the container is on: the nodeId of its row in the containers list. May be left out on an environment of one node; on a swarm it is required (422 node_required)"`
}

// ContainerPortBody is one published port.
type ContainerPortBody struct {
	ContainerPort int    `json:"containerPort" doc:"The port inside the container"`
	Protocol      string `json:"protocol" doc:"tcp, udp or sctp"`
	HostIP        string `json:"hostIp,omitempty" doc:"The host address it is bound to; empty when the engine did not say"`
	HostPort      string `json:"hostPort" doc:"The port on the host"`
}

// ContainerMountBody is one mount.
type ContainerMountBody struct {
	Type        string `json:"type" doc:"bind, volume, tmpfs, npipe or cluster"`
	Name        string `json:"name,omitempty" doc:"The volume name, for a volume"`
	Source      string `json:"source" doc:"The host path or, for a volume, where the engine keeps it"`
	Destination string `json:"destination" doc:"The path inside the container"`
	Mode        string `json:"mode,omitempty" doc:"The engine's mount options, for example z or ro"`
	RW          bool   `json:"rw" doc:"True when the container may write to it"`
}

// ContainerNetworkBody is one network the container is attached to.
type ContainerNetworkBody struct {
	NetworkID string   `json:"networkId"`
	Name      string   `json:"name"`
	IPv4      string   `json:"ipv4,omitempty"`
	IPv6      string   `json:"ipv6,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Aliases   []string `json:"aliases" doc:"The DNS names the container answers to on this network"`
}

// ContainerLogConfigBody is the log driver and its options.
type ContainerLogConfigBody struct {
	Type    string             `json:"type" doc:"The log driver, for example json-file; empty when the engine did not say"`
	Options []redact.OptionVar `json:"options" doc:"The driver's options by name. Their values can carry credentials, so they are withheld unless the caller holds secrets.reveal on this environment"`
}

// ContainerDetailBody is one container as its detail page shows it.
type ContainerDetailBody struct {
	Node     string `json:"node,omitempty" doc:"The display name of the node, when it is among the nodes that answered"`
	NodeID   string `json:"nodeId" doc:"The ID of the node the container is on: what a write is routed by"`
	ReadOnly bool   `json:"readOnly" doc:"True when the host of this container's node is read-only, so no change to it will be accepted"`

	ID       string `json:"id" doc:"The full container ID"`
	Name     string `json:"name" doc:"The container name without the leading slash"`
	Image    string `json:"image" doc:"The image as the container was created with it"`
	ImageID  string `json:"imageId" doc:"The ID of the image it runs"`
	Created  string `json:"created,omitempty" format:"date-time"`
	State    string `json:"state" enum:"created,running,paused,restarting,removing,exited,dead" doc:"The container state"`
	Health   string `json:"health" enum:"none,starting,healthy,unhealthy"`
	Started  string `json:"startedAt,omitempty" format:"date-time" doc:"When it last started; absent when it never did"`
	Finished string `json:"finishedAt,omitempty" format:"date-time" doc:"When it last stopped; absent while it runs or when it never ran"`
	ExitCode int    `json:"exitCode" doc:"The exit code of its last run"`
	OOM      bool   `json:"oomKilled" doc:"True when the kernel killed it for running out of memory"`
	Error    string `json:"error,omitempty" doc:"The engine's last start error"`
	Restarts int    `json:"restartCount" doc:"How many times the engine has restarted it"`

	Command       []string `json:"command" doc:"The command (Cmd). Withheld, as the single entry <hidden>, unless the caller holds secrets.reveal on this environment: a command line often carries a password or token"`
	Entrypoint    []string `json:"entrypoint" doc:"The entrypoint. Withheld, as the single entry <hidden>, on the same terms as command"`
	CommandMasked bool     `json:"commandMasked" doc:"True when command or entrypoint was withheld because the caller may not reveal secrets on this environment"`
	WorkingDir    string   `json:"workingDir,omitempty"`
	User          string   `json:"user,omitempty"`
	StopTimeout   *int     `json:"stopTimeout,omitempty" doc:"Seconds the container's own setting waits before a kill; absent when it sets none"`
	StopSignal    string   `json:"stopSignal,omitempty"`

	RestartPolicy     string `json:"restartPolicy" enum:"no,always,on-failure,unless-stopped"`
	RestartMaxRetries int    `json:"restartMaxRetries" doc:"The on-failure maximum; 0 when none is set"`

	Ports    []ContainerPortBody    `json:"ports" doc:"Published ports only"`
	Mounts   []ContainerMountBody   `json:"mounts"`
	Networks []ContainerNetworkBody `json:"networks"`
	Labels   map[string]string      `json:"labels" doc:"Every label, as the engine reports them. Do not put secrets in labels"`

	Env       []redact.EnvVar        `json:"env" doc:"The environment variables by name. Their values are withheld unless the caller holds secrets.reveal on this environment"`
	LogConfig ContainerLogConfigBody `json:"logConfig"`
	Masked    bool                   `json:"masked" doc:"True when any value was withheld from env, logConfig.options, command or entrypoint"`

	IsSelf         bool   `json:"isSelf" doc:"True for the container Bayplan itself runs in"`
	SelfKind       string `json:"selfKind" enum:",server,agent" doc:"server for the container Bayplan itself runs in, agent for a Bayplan agent's container, empty otherwise"`
	Project        string `json:"project" doc:"The compose project (com.docker.compose.project); empty when the container has none"`
	Stack          string `json:"stack" doc:"The swarm stack namespace (com.docker.stack.namespace); empty when the container has none"`
	Service        string `json:"service" doc:"The swarm service name (com.docker.swarm.service.name); empty when the container has none"`
	BayplanStackID string `json:"bayplanStackId" doc:"The ID of the Bayplan stack the container belongs to: set only when this environment holds a stack with that ID under the stack or project name the container carries (bayplan.stack.id). Empty for a stack or project Bayplan does not manage"`

	RestartLoop *ContainerRestartLoopBody `json:"restartLoop,omitempty" doc:"Present while the container is in a restart loop: it died with a non-zero exit code the environment's threshold of times inside its window. Absent otherwise"`
	Muted       bool                      `json:"muted" doc:"True when a user muted the container's restart-loop alerts, whether or not it is in a loop now. The mute is keyed by node and name"`
}

// ContainerDetailOutput wraps ContainerDetailBody for huma.
type ContainerDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerDetailBody
}

// ContainerInspectBody is the raw inspect document of one container.
type ContainerInspectBody struct {
	Masked  bool           `json:"masked" doc:"True when a value was replaced by the placeholder <hidden> because the caller may not reveal secrets on this environment"`
	Inspect map[string]any `json:"inspect" doc:"The engine's inspect document, with the environment variables' values, the log driver's option values and the command line replaced by <hidden> unless the caller holds secrets.reveal"`
}

// ContainerInspectOutput wraps ContainerInspectBody for huma.
type ContainerInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerInspectBody
}

// decodeInspect reads the masked document into the response, keeping every number
// with its exact digits.
func decodeInspect(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// RegisterContainers registers the container action route. Every target is routed
// by node ID through the aggregator's one write path (WriteOnNode), is checked against Bayplan's
// own containers from a fresh inspect, and gets exactly one audit row (D-14). A
// write is never retried (P2 D-42).
func RegisterContainers(r *httpx.Router, d ContainersDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	isSelf := func(id string) bool { return isSelfContainer(d.SelfContainerID, id) }

	httpx.Register(r, huma.Operation{
		OperationID: "getContainer", Method: http.MethodGet, Path: "/api/env/{envId}/containers/{containerId}",
		Summary: "One container: its state and configuration",
		Description: "Everything the container detail page shows, read live from the container's engine: state, health, image, command, ports, mounts, networks with their aliases, restart policy, labels, environment variables and log driver. " +
			"The container lives on one node, named by nodeId (the nodeId of its row in the containers list); it may be left out on an environment of one node and is required on a swarm (422 node_required). " +
			"The values of the environment variables and of the log driver's options can be credentials: they are withheld on the server, and sent only to a caller who holds secrets.reveal on the environment. env and logConfig.options then list the names with masked true and no value, and masked at the top says something was withheld. " +
			"The command and the entrypoint can carry credentials too (a password on the command line): without secrets.reveal each is the single entry <hidden> and commandMasked is true. " +
			"404 container_not_found when the engine has no such container (it may have been removed meanwhile); 503 node_unavailable when the node is not a connected node of the environment. " +
			"Needs containers.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerDetailInput) (*ContainerDetailOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			detail, _, nodeID, node, err := inspectOnNode(ctx, d, env.ID, in.NodeID, in.ContainerID)
			if err != nil {
				return nil, err
			}
			reveal := redact.CanReveal(ctx, d.Checker, p, env.ID)
			body := containerDetailBody(detail, nodeID, node, isSelf(detail.ID), reveal)
			body.BayplanStackID = loadStackIDs(ctx, d.Store, env.ID, d.Log).confirmedFor(containerStack(detail.Labels), detail.Labels)
			loops, muted := containerLoopsOf(d.Clusters, env.ID)
			key := envs.ContainerLoopKey(nodeID, detail.Name)
			if l, flagged := loops[key]; flagged {
				body.RestartLoop = containerRestartLoopBody(l)
			}
			body.Muted = muted[key]
			return &ContainerDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectContainer", Method: http.MethodGet, Path: "/api/env/{envId}/containers/{containerId}/inspect",
		Summary: "One container's raw inspect document",
		Description: "The engine's inspect document of the container as it is, for the Inspect tab. " +
			"The container lives on one node, named by nodeId, with the same rules and problems as the container detail route (422 node_required, 404 container_not_found, 503 node_unavailable). " +
			"The environment variables' values, the log driver's option values and the command line (Config.Cmd, Config.Entrypoint, Path, Args and the command of Config.Healthcheck.Test) can be credentials: for a caller without secrets.reveal on the environment each is replaced by the placeholder <hidden> on the server, and masked says something was. " +
			"Needs containers.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermContainersRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerDetailInput) (*ContainerInspectOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			_, raw, _, _, err := inspectOnNode(ctx, d, env.ID, in.NodeID, in.ContainerID)
			if err != nil {
				return nil, err
			}
			shown, masked, err := redact.MaskContainerInspect(raw, redact.CanReveal(ctx, d.Checker, p, env.ID))
			if err != nil {
				d.Log.Error("masking a container inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			doc, err := decodeInspect(shown)
			if err != nil {
				d.Log.Error("reading a container inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &ContainerInspectOutput{CacheControl: "no-store", Body: ContainerInspectBody{Masked: masked, Inspect: doc}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "actOnContainers", Method: http.MethodPost, Path: "/api/env/{envId}/containers/actions",
		Summary: "Start, stop, restart, kill, pause or unpause containers",
		Description: "Does one action to up to 25 containers, each named by its node ID and container ID, and answers one result per target in request order. " +
			"The actions are start, stop, restart, kill, pause and unpause. A stop or restart uses the container's own stop timeout and signal and a kill sends SIGKILL; " +
			"a request with exactly one target may set timeout or signal, and a request with more is refused 422 options_single_only. " +
			"Starting a running container or stopping a stopped one is answered done, and a container removed meanwhile is not_found, so repeating an action is safe. " +
			"A target is routed by node ID, never by display name. A target that fails never stops the others, and nothing is retried. " +
			"At most 4 calls run at once on one node and 16 targets at once overall. " +
			"With more than one target (or a bulkId sent) every audit row of the request carries the same bulk detail, a UUIDv7 the server makes when the client sends none, and the response returns it. " +
			"Bayplan's own server container and any agent container are protected: to stop, kill, restart or pause one, send a single-target request whose confirm is the container's name (else 422 confirmation_required, and nothing is sent); " +
			"in a request with more than one target such a container is skipped with code own_container and never acted on. Start and unpause need no confirm. " +
			"An action on one of them is audited before the call (stage initiated) and after it (stage completed), and a connection that drops during the call is answered done with code connection_lost and audited as stage connection_lost with result success. " +
			"A node whose agent runs read-only (BAYPLAN_AGENT_READ_ONLY) is refused with host_read_only before any call; an agent that refuses by its own policy, such as one older than the server, is agent_policy_denied. " +
			"Each target is audited as container.started, container.stopped, container.restarted, container.killed, container.paused or container.unpaused, with result failure and the code when it failed. " +
			"Needs containers.control (Operator) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusOK,
	}, rbac.Requirement{Perm: rbac.PermContainersControl, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerActionInput) (*ContainerActionOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			single := len(in.Body.Targets) == 1
			if (in.Body.Timeout != nil || in.Body.Signal != "") && !single {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemOptionsSingleOnly,
					"timeout and signal can be set for one container at a time; a bulk request uses each container's own settings")
			}
			if in.Body.Confirm != "" && !single {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemOptionsSingleOnly,
					"a typed name confirms one container at a time; Bayplan's own containers are never acted on in a bulk")
			}
			spec := verbSpecs[in.Body.Action]
			opts := dockerx.ActionOptions{Timeout: in.Body.Timeout, Signal: in.Body.Signal}
			bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
			if err != nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			extra := map[string]string{}
			if spec.usesTimeout && in.Body.Timeout != nil {
				extra["timeout"] = strconv.Itoa(*in.Body.Timeout)
			}
			if spec.usesSignal && in.Body.Signal != "" {
				extra["signal"] = in.Body.Signal
			}
			op := containerOp{
				verb: spec.verb, opts: opts, single: single, confirm: in.Body.Confirm,
				initiate: initiator(ctx, d, p, env.ID, in.UserAgent, spec.action, bulkID, extra),
			}
			results := runTargets(ctx, in.Body.Targets, func(ctx context.Context, t ContainerActionTarget) ContainerActionResult {
				return actOnOne(ctx, d, isSelf, env.ID, t, op)
			})
			if err := confirmationRequired(single, results); err != nil {
				return nil, err
			}
			auditTargets(ctx, d, p, env.ID, in.UserAgent, spec.action, bulkID, extra, in.Body.Targets, results)
			return &ContainerActionOutput{CacheControl: "no-store", Body: ContainerActionResponse{BulkID: bulkID, Results: results}}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "removeContainers", Method: http.MethodPost, Path: "/api/env/{envId}/containers/remove",
		Summary: "Remove containers",
		Description: "Removes up to 25 containers, each named by its node ID and container ID, and answers one result per target in request order, as the actions route does. " +
			"A running container is refused with container_running unless force is true, and nothing is sent for it. " +
			"Named volumes are never removed. removeVolumes also removes the container's anonymous volumes, and needs the delete volumes permission on top of containers.remove: " +
			"a caller without it is refused 403 before any call and the refusal is audited as permission.denied. " +
			"Bayplan's own server container and any agent container are protected: to remove one, send a single-target request whose confirm is the container's name (else 422 confirmation_required, and nothing is sent); " +
			"in a request with more than one target such a container is skipped with code own_container and never acted on. Start and unpause need no confirm. " +
			"An action on one of them is audited before the call (stage initiated) and after it (stage completed), and a connection that drops during the call is answered done with code connection_lost and audited as stage connection_lost with result success. " +
			"Each target is audited as container.removed with its force and volumes choices. " +
			"Needs containers.remove (Operator) on the environment.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusOK,
	}, rbac.Requirement{Perm: rbac.PermContainersRemove, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ContainerRemoveInput) (*ContainerActionOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			if in.Body.RemoveVolumes {
				if err := requireVolumesDelete(ctx, d, p, env.ID, in.UserAgent); err != nil {
					return nil, err
				}
			}
			single := len(in.Body.Targets) == 1
			if in.Body.Confirm != "" && !single {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemOptionsSingleOnly,
					"a typed name confirms one container at a time; Bayplan's own containers are never acted on in a bulk")
			}
			bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
			if err != nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			extra := map[string]string{"force": strconv.FormatBool(in.Body.Force), "volumes": strconv.FormatBool(in.Body.RemoveVolumes)}
			op := containerOp{
				remove: true, rm: dockerx.RemoveOptions{Force: in.Body.Force, Volumes: in.Body.RemoveVolumes},
				single: single, confirm: in.Body.Confirm,
				initiate: initiator(ctx, d, p, env.ID, in.UserAgent, audit.ActionContainerRemoved, bulkID, extra),
			}
			results := runTargets(ctx, in.Body.Targets, func(ctx context.Context, t ContainerActionTarget) ContainerActionResult {
				return actOnOne(ctx, d, isSelf, env.ID, t, op)
			})
			if err := confirmationRequired(single, results); err != nil {
				return nil, err
			}
			auditTargets(ctx, d, p, env.ID, in.UserAgent, audit.ActionContainerRemoved, bulkID, extra, in.Body.Targets, results)
			return &ContainerActionOutput{CacheControl: "no-store", Body: ContainerActionResponse{BulkID: bulkID, Results: results}}, nil
		})
}

// snapshotReader is what resolveContainerNode reads: the aggregate of an environment.
// *envs.Aggregator implements it.
type snapshotReader interface {
	Snapshot(ctx context.Context, envID string) (envs.Aggregate, envs.Health, error)
}

// resolveContainerNode finds the node a container route is about. Without a node only
// an environment of one node that is not a swarm has an answer (422 node_required). A
// node the environment does not have is not an error here: the call that follows says
// 503 node_unavailable. The aggregate is returned for callers that look up the
// container's row in it.
func resolveContainerNode(ctx context.Context, src snapshotReader, log *slog.Logger, envID, wantNode string) (agg envs.Aggregate, nodeID string, node envs.NodeRef, err error) {
	agg, _, err = src.Snapshot(ctx, envID)
	if err != nil {
		return agg, "", node, snapshotProblem(log, envID, err)
	}
	nodeID = wantNode
	if nodeID == "" {
		// Without a node, only an environment of one node that is not a swarm has
		// an answer: anywhere else the container could be on any of several.
		swarmType := agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker
		if len(agg.Nodes) != 1 || swarmType {
			return agg, "", node, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
				"This environment has more than one node. Say which one the container is on (nodeId).")
		}
		nodeID = agg.Nodes[0].ID
	}
	for _, n := range agg.Nodes {
		if n.ID == nodeID {
			node = n
		}
	}
	return agg, nodeID, node, nil
}

// inspectOnNode resolves the node a container is on and reads its inspect there: the
// typed detail and the raw document. Without a node only an environment of one node
// that is not a swarm has an answer (422 node_required). The errors are already
// problems: 404 container_not_found, 503 node_unavailable, or the environment's own.
func inspectOnNode(ctx context.Context, d ContainersDeps, envID, wantNode, containerID string) (detail dockerx.ContainerDetail, raw []byte, nodeID string, node envs.NodeRef, err error) {
	return inspectOn(ctx, d.Aggregator, d.Log, envID, wantNode, containerID)
}

// nodeInspector is what reading one container's inspect needs: the aggregate, to find
// the node, and that node's engine. *envs.Aggregator implements it.
type nodeInspector interface {
	snapshotReader
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// inspectOn is inspectOnNode for any source that can reach a node: the container routes
// and the stats stream read a container the same way and map the same problems.
func inspectOn(ctx context.Context, src nodeInspector, log *slog.Logger, envID, wantNode, containerID string) (detail dockerx.ContainerDetail, raw []byte, nodeID string, node envs.NodeRef, err error) {
	_, nodeID, node, err = resolveContainerNode(ctx, src, log, envID, wantNode)
	if err != nil {
		return detail, nil, "", node, err
	}

	inspectCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err = src.OnNode(inspectCtx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		detail, raw, e = c.ContainerDetail(inspectCtx, containerID)
		return e
	})
	switch {
	case err == nil:
		return detail, raw, nodeID, node, nil
	case dockerx.IsNotFound(err):
		return detail, nil, nodeID, node, httpx.Problem(http.StatusNotFound, problemContainerNotFound, "This container does not exist on that node. It may have been removed.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return detail, nil, nodeID, node, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
	default:
		return detail, nil, nodeID, node, snapshotProblem(log, envID, err)
	}
}

// containerDetailBody is the response for one container. Every value that can be a
// secret goes through the redact package here, the one place it can leave the server.
func containerDetailBody(c dockerx.ContainerDetail, nodeID string, node envs.NodeRef, self, reveal bool) ContainerDetailBody {
	env := redact.Env(c.Env, reveal)
	opts := redact.Options(c.LogConfig.Options, reveal)
	// The command line is where credentials are passed (--requirepass, a DSN), so it
	// is withheld like an environment value.
	cmd, cmdMasked := redact.Argv(c.Cmd, reveal)
	entrypoint, entrypointMasked := redact.Argv(c.Entrypoint, reveal)
	b := ContainerDetailBody{
		Node: node.Name, NodeID: nodeID, ReadOnly: node.ReadOnly,
		ID: c.ID, Name: c.Name, Image: c.Image, ImageID: c.ImageID, Created: engineTime(c.Created),
		State: c.Status, Health: c.Health, Started: engineTime(c.StartedAt), Finished: engineTime(c.FinishedAt),
		ExitCode: c.ExitCode, OOM: c.OOMKilled, Error: c.Error, Restarts: c.RestartCount,
		Command: cmd, Entrypoint: entrypoint, CommandMasked: cmdMasked || entrypointMasked, WorkingDir: c.WorkingDir, User: c.User,
		StopTimeout: c.StopTimeout, StopSignal: c.StopSignal,
		RestartPolicy: c.RestartPolicy, RestartMaxRetries: c.RestartMaxRetries,
		Ports: make([]ContainerPortBody, 0, len(c.Ports)), Mounts: make([]ContainerMountBody, 0, len(c.Mounts)),
		Networks: make([]ContainerNetworkBody, 0, len(c.Networks)), Labels: c.Labels,
		Env: env, LogConfig: ContainerLogConfigBody{Type: c.LogConfig.Type, Options: opts},
		Masked: !reveal && (len(env) > 0 || len(opts) > 0 || cmdMasked || entrypointMasked),
		IsSelf: self, SelfKind: selfKindOf(self, c.Labels),
		Project: c.Labels[labelProject], Stack: c.Labels[labelStack], Service: c.Labels[labelService],
	}
	for _, p := range c.Ports {
		b.Ports = append(b.Ports, ContainerPortBody{ContainerPort: int(p.ContainerPort), Protocol: p.Protocol, HostIP: p.HostIP, HostPort: p.HostPort})
	}
	for _, m := range c.Mounts {
		b.Mounts = append(b.Mounts, ContainerMountBody{Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, Mode: m.Mode, RW: m.RW})
	}
	for _, n := range c.Networks {
		b.Networks = append(b.Networks, ContainerNetworkBody{NetworkID: n.NetworkID, Name: n.Name, IPv4: n.IPv4, IPv6: n.IPv6, MAC: n.MAC, Aliases: n.Aliases})
	}
	return b
}

// engineTime turns a timestamp of the engine into RFC 3339 UTC. The engine's zero
// time, 0001-01-01T00:00:00Z, and anything unreadable are no time.
func engineTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return ""
	}
	return rfc3339(t)
}

// snapshotProblem turns the error of a read of an environment, or of one of its
// engines, into the problem the SPA understands, as the lists do.
func snapshotProblem(log *slog.Logger, envID string, err error) error {
	err = envs.ClassifyDockerError(err)
	var denied *envs.AgentPolicyDeniedError
	var down envs.ErrNotOnline
	switch {
	case errors.As(err, &denied):
		log.Warn("an agent refused a read of an environment", "environment", envID, "endpoint", denied.Endpoint)
		return httpx.Problem(http.StatusBadGateway, problemAgentPolicyDenied, "The agent refused a request ("+denied.Endpoint+").")
	case errors.As(err, &down):
		return notOnlineProblem(down)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return httpx.Problem(http.StatusServiceUnavailable, "", "The environment took too long to answer.")
	}
	log.Error("reading an environment failed", "environment", envID, "error", err)
	return offlineProblem(time.Time{}, dockerx.ReasonUnreachable)
}

// verbSpec says what an action name does and how it is audited.
type verbSpec struct {
	verb   dockerx.ContainerVerb
	action string
	// usesTimeout and usesSignal say whether the verb reads those options, so the
	// audit row records only what could have had an effect.
	usesTimeout, usesSignal bool
}

var verbSpecs = map[string]verbSpec{
	"start":   {verb: dockerx.VerbStart, action: audit.ActionContainerStarted},
	"stop":    {verb: dockerx.VerbStop, action: audit.ActionContainerStopped, usesTimeout: true, usesSignal: true},
	"restart": {verb: dockerx.VerbRestart, action: audit.ActionContainerRestarted, usesTimeout: true, usesSignal: true},
	"kill":    {verb: dockerx.VerbKill, action: audit.ActionContainerKilled, usesSignal: true},
	"pause":   {verb: dockerx.VerbPause, action: audit.ActionContainerPaused},
	"unpause": {verb: dockerx.VerbUnpause, action: audit.ActionContainerUnpaused},
}

// containerOp is what is done to each target: one of the six verbs with its options,
// or a remove.
type containerOp struct {
	verb   dockerx.ContainerVerb
	opts   dockerx.ActionOptions
	remove bool
	rm     dockerx.RemoveOptions
	// single is true for a request with exactly one target, the only kind that may act on
	// one of Bayplan's own containers; confirm is the name typed for it.
	single  bool
	confirm string
	// initiate records the audit row written before a call to one of Bayplan's own
	// containers.
	initiate func(t ContainerActionTarget, name, own string) error
}

// disruptive reports whether op, done to one of Bayplan's own containers, can cut Bayplan
// off from a host. A start and an unpause cannot (D-11).
func (op containerOp) disruptive() bool {
	if op.remove {
		return true
	}
	switch op.verb {
	case dockerx.VerbStop, dockerx.VerbKill, dockerx.VerbRestart, dockerx.VerbPause:
		return true
	}
	return false
}

// name is the action's name in the log: the verb, or remove.
func (op containerOp) name() string {
	if op.remove {
		return "remove"
	}
	return string(op.verb)
}

// writeDeadline is how long the write of op may take on a container with the given
// own stop timeout.
func (op containerOp) writeDeadline(own *int) time.Duration {
	if op.remove || (op.verb != dockerx.VerbStop && op.verb != dockerx.VerbRestart) {
		return writeTimeout
	}
	secs := defaultStopTimeout
	switch {
	case op.opts.Timeout != nil:
		secs = *op.opts.Timeout
	case own != nil:
		secs = *own
	}
	return time.Duration(secs)*time.Second + stopGrace
}

// requireVolumesDelete is the second permission check of a remove that also removes
// anonymous volumes. A caller without it is refused before any call, and the refusal is
// audited as permission.denied like a route refusal (T-04-22).
func requireVolumesDelete(ctx context.Context, d ContainersDeps, p *rbac.Principal, envID, userAgent string) error {
	var dec rbac.Decision
	if d.Checker != nil {
		dec = d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermVolumesDelete, Scope: rbac.ScopeEnvironment}, envID)
	}
	if dec.Err != nil {
		d.Log.Error("could not check volumes.delete", "error", dec.Err)
		return huma.Error500InternalServerError("internal error")
	}
	if dec.Allowed {
		return nil
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
		Reason: rbac.ReasonForbidden, TargetType: "route", TargetID: "POST /api/env/{envId}/containers/remove", EnvID: envID,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		Details: map[string]string{"permission": string(rbac.PermVolumesDelete)},
	}); err != nil {
		// A refusal nobody can see must not look like a normal one.
		d.Log.Error("could not audit a refusal", "error", err)
		return huma.Error500InternalServerError("internal error")
	}
	return httpx.Problem(http.StatusForbidden, "forbidden", "Removing anonymous volumes needs the delete volumes permission")
}

// doneActionAuditContext is the context for the audit row of a change that has
// already been made or refused. It keeps the request's values but not its
// cancellation, with a deadline of its own: a browser that closed while the change
// ran must not leave the change without a record.
func doneActionAuditContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), actionAuditTimeout)
}

// auditTargets writes one audit row per target, in request order, once every target has
// answered, so the rows of one bulk read in the order the targets were sent. extra is
// added to every row.
func auditTargets(ctx context.Context, d ContainersDeps, p *rbac.Principal, envID, userAgent, action, bulkID string, extra map[string]string, targets []ContainerActionTarget, results []ContainerActionResult) {
	for i, t := range targets {
		res := results[i]
		result, reason, code := audit.ResultSuccess, "", actionDone
		if res.Result != actionDone {
			result, reason, code = audit.ResultFailure, res.Code, res.Code
		} else if res.Code != "" {
			code = res.Code // connection_lost: accepted, not failed
		}
		details := map[string]string{"container": res.Name, "node": t.NodeID, "code": code}
		if res.own != "" {
			details["own"] = res.own
		}
		if res.stage != "" {
			details["stage"] = res.stage
		}
		for k, v := range extra {
			details[k] = v
		}
		if bulkID != "" {
			details["bulk"] = bulkID
		}
		// The action has already happened, so its row must outlive a browser that went away
		// while it ran (a restart waits out the stop timeout): a cancelled request would
		// otherwise leave an action with no record.
		wctx, cancel := doneActionAuditContext(ctx)
		err := d.Audit.Record(wctx, audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: containerTargetType, TargetID: t.ID,
			EnvID: envID, Result: result, Reason: reason,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent,
			Details: details,
		})
		cancel()
		if err != nil {
			// The action has been done or refused, and the caller must still be
			// told which; the lost row is the server's fault and is logged.
			d.Log.Error("could not audit a container action", "action", action, "container", t.ID, "error", err)
		}
	}
}

// bulkIDFor is the bulk ID of a request: the one the client sent, else a new UUIDv7
// when more than one target is named, else none.
func bulkIDFor(sent string, targets int) (string, error) {
	if sent != "" || targets < 2 {
		return sent, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// nodeLimiter holds the semaphore of each node a request touches, so no node gets
// more than perNodeCalls calls at once.
type nodeLimiter struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

func (l *nodeLimiter) acquire(ctx context.Context, node string) (release func(), err error) {
	l.mu.Lock()
	sem, ok := l.sems[node]
	if !ok {
		sem = make(chan struct{}, perNodeCalls)
		l.sems[node] = sem
	}
	l.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runTargets runs do for every target, at most bulkParallel at once and perNodeCalls per
// node. Each answer goes in its own slot, so results keep the order the targets were
// sent in whatever order the calls finish. A target that fails never cancels the others.
func runTargets(ctx context.Context, targets []ContainerActionTarget, do func(context.Context, ContainerActionTarget) ContainerActionResult) []ContainerActionResult {
	results := make([]ContainerActionResult, len(targets))
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	for i, t := range targets {
		g.Go(func() error {
			release, err := lim.acquire(ctx, t.NodeID)
			if err != nil {
				results[i] = ContainerActionResult{NodeID: t.NodeID, ID: t.ID, Name: t.ID, Result: actionFailed, Code: codeActionFailed, Detail: "The request was cancelled before this target was reached."}
				return nil
			}
			defer release()
			results[i] = do(ctx, t)
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: failures are results
	return results
}

// actOnOne does op to one target and says what happened. It never returns an
// error: a failure is a result of its own.
func actOnOne(ctx context.Context, d ContainersDeps, isSelf func(string) bool, envID string, t ContainerActionTarget, op containerOp) ContainerActionResult {
	res := ContainerActionResult{NodeID: t.NodeID, ID: t.ID, Name: t.ID}
	fail := func(code, detail string) ContainerActionResult {
		res.Result, res.Code, res.Detail = actionFailed, code, detail
		return res
	}
	// failCall is a failure of an engine call. Its error is logged whole: the result's
	// detail is clipped, and the audit row keeps only the code.
	failCall := func(err error) ContainerActionResult {
		code, detail := actionError(err)
		d.Log.Warn("container action failed", "environment", envID, "node", t.NodeID,
			"container", res.Name, "action", op.name(), "code", code, "error", err)
		return fail(code, detail)
	}

	// The own-container rule reads the container fresh from its engine: a list can
	// be seconds old, and detection of the server's own container can miss, so
	// neither the list's flag nor anything the client sent is trusted (D-11).
	var own dockerx.ContainerOwnership
	inspectCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	err := d.Aggregator.OnNode(inspectCtx, envID, t.NodeID, func(c *dockerx.Client) error {
		var e error
		own, e = c.ContainerOwnership(inspectCtx, t.ID)
		return e
	})
	cancel()
	if err != nil {
		return failCall(err)
	}
	if own.Name != "" {
		res.Name = own.Name
	}
	switch {
	case isSelf(own.ID):
		res.own = ownServer
	case own.Labels[ownerLabelAgent] == ownerLabelAgentValue:
		res.own = ownAgent
	}
	recorded := false // the initiated row of an action on an own container is written
	if res.own != "" && op.disruptive() {
		if !op.single {
			// A bulk, and a select-all, never reaches Bayplan's own containers (D-12).
			res.Result, res.Code = actionSkipped, codeOwnContainer
			res.Detail = "This is Bayplan's own container. It is not acted on in a bulk; act on it alone, typing its name."
			return res
		}
		if own.Name == "" || op.confirm != own.Name {
			return fail(codeConfirmationRequired, "Type the container's name to confirm this.")
		}
		// The call may take the recorder down with it, so the attempt is written first.
		// With no row, nothing is sent.
		if err := op.initiate(t, res.Name, res.own); err != nil {
			d.Log.Error("could not audit the start of an action on an own container", "container", res.Name, "error", err)
			return fail(codeActionFailed, "The action could not be recorded in the audit log, so nothing was sent.")
		}
		recorded = true
	}
	if op.remove && own.Running && !op.rm.Force {
		return fail(codeContainerRunning, "The container is running. Stop it first, or remove it with force.")
	}

	// A write is sent once. If its answer is lost the outcome is unknown, and the
	// caller refreshes the list to see it (P2 D-42).
	//
	// Once sent it is seen through: the write does not end with the request. The engine
	// finishes a stop, restart or remove whether or not its caller is still there, so a
	// write cut short when the browser's request ended (a closed page, a dropped
	// connection, or Chromium abandoning every request when the host's network changes)
	// would answer and audit "context canceled" as a failure for an action that happened.
	// It keeps its own deadline.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), op.writeDeadline(own.StopTimeout))
	defer cancelWrite()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, t.NodeID, func(c *dockerx.Client) error {
		if op.remove {
			return c.ContainerRemove(writeCtx, own.ID, op.rm)
		}
		return c.ContainerAction(writeCtx, own.ID, op.verb, op.opts)
	})
	if recorded {
		res.stage = stageCompleted
	}
	if err != nil {
		if recorded && isConnectionLost(err) {
			// Stopping the process that serves this call ends the call. The action is
			// taken as accepted, not as a failure; the list shows what happened.
			res.stage, res.Result, res.Code = stageConnectionLost, actionDone, codeConnectionLost
			res.Detail = "The connection ended while the action ran, which is what it does to Bayplan's own container. Refresh to see the outcome."
			return res
		}
		return failCall(err)
	}
	res.Result = actionDone
	return res
}

// isConnectionLost reports whether err is the end of a connection rather than an answer
// from the engine: a dropped tunnel, a closed or reset socket, an unexpected end of stream,
// or a broken pipe (the connection closed while a request body was still being written,
// as a network disconnect that cuts Bayplan off can do).
func isConnectionLost(err error) bool {
	return errors.Is(err, tunnel.ErrAgentGone) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed)
}

// confirmationRequired turns the one target that needs its name typed into the request's
// answer: a 422, with nothing sent for it.
func confirmationRequired(single bool, results []ContainerActionResult) error {
	if single && len(results) == 1 && results[0].Code == codeConfirmationRequired {
		return httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired, results[0].Detail)
	}
	return nil
}

// initiator returns the function that writes the initiated row of an action on one of
// Bayplan's own containers. The row carries what the final row would, with stage
// initiated, and the result success because nothing has failed yet.
func initiator(ctx context.Context, d ContainersDeps, p *rbac.Principal, envID, userAgent, action, bulkID string, extra map[string]string) func(ContainerActionTarget, string, string) error {
	return func(t ContainerActionTarget, name, own string) error {
		details := map[string]string{"container": name, "node": t.NodeID, "code": stageInitiated, "stage": stageInitiated, "own": own}
		for k, v := range extra {
			details[k] = v
		}
		if bulkID != "" {
			details["bulk"] = bulkID
		}
		return d.Audit.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: containerTargetType, TargetID: t.ID,
			EnvID: envID, Result: audit.ResultSuccess,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
		})
	}
}

// actionError maps the error of one engine call to a result code and a sentence.
func actionError(err error) (code, detail string) {
	switch denied := (*envs.AgentPolicyDeniedError)(nil); {
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly, "This host is read-only: its owner set BAYPLAN_AGENT_READ_ONLY on the agent, so Bayplan cannot change anything on it."
	case errors.Is(err, envs.ErrNodeUnavailable):
		return codeNodeUnreachable, "The node is not connected to Bayplan."
	case errors.Is(err, envs.ErrNotFound):
		return codeNodeUnreachable, "The environment could not be found."
	case errors.Is(err, envs.ErrOffline), errors.Is(err, envs.ErrUnsupported), errors.Is(err, tunnel.ErrAgentGone):
		return codeNodeUnreachable, "The node did not answer."
	case dockerx.IsNotFound(err):
		return codeNotFound, "The container no longer exists."
	case errors.As(envs.ClassifyDockerError(err), &denied):
		return codeAgentPolicyDeny, "The node's agent refused the request."
	}
	return codeActionFailed, clipDetail(err.Error())
}

// clipDetail cuts an engine message to maxActionDetail characters on a rune
// boundary.
func clipDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxActionDetail {
		return s
	}
	return strings.ToValidUTF8(s[:maxActionDetail], "")
}
