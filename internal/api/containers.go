// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

// Result values and codes of one target of a container action. A code is a stable
// word the SPA maps to its own copy; the detail is only a fallback sentence.
const (
	actionDone   = "done"
	actionFailed = "failed"
	// The result enum also names skipped, a target the endpoint did not act on and
	// did not fail. It is reserved for the bulk plans; nothing here returns it.

	codeOwnContainer     = "own_container"
	codeNodeUnreachable  = "node_unreachable"
	codeNotFound         = "not_found"
	codeAgentPolicyDeny  = problemAgentPolicyDenied
	codeHostReadOnly     = "host_read_only"
	codeActionFailed     = "action_failed"
	codeContainerRunning = "container_running"
	maxActionDetail      = 300
	containerTargetType  = "container"
	ownerLabelAgent      = "bayplan.agent"
	ownerLabelAgentValue = "true"

	// A target's fresh inspect gets inspectTimeout. Its write gets writeTimeout, or for a
	// stop or restart the stop timeout (the request's, else the container's own, else
	// defaultStopTimeout) plus stopGrace, because the engine waits that long before it
	// kills the container (D-09).
	inspectTimeout     = 30 * time.Second
	writeTimeout       = 30 * time.Second
	stopGrace          = 30 * time.Second
	defaultStopTimeout = 10 // seconds, the engine's own default

	problemOptionsSingleOnly = "options_single_only"

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
	// Checker is carried for the second-permission checks and the masking the later
	// plans of the phase add (volumes.delete on remove, secrets.reveal on inspect).
	Checker *rbac.Checker
	// SelfContainerID is Bayplan's own container ID; empty when it cannot tell.
	SelfContainerID string
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
	Targets []ContainerActionTarget `json:"targets" minItems:"1" maxItems:"25" doc:"The containers to act on, at most 25. Each is acted on and answered separately; one that fails never stops the others"`
}

// ContainerRemoveRequest is the body of POST /api/env/{envId}/containers/remove.
type ContainerRemoveRequest struct {
	BulkID        string                  `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request, as for the actions route"`
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
	NodeID string `json:"nodeId" doc:"The node ID the request named"`
	ID     string `json:"id" doc:"The container ID or name the request named"`
	Name   string `json:"name" doc:"The container name when the engine could be asked; otherwise the ID or name the request named"`
	Result string `json:"result" enum:"done,failed,skipped" doc:"done: the engine accepted the action. failed: it was refused or could not be sent; code says why"`
	Code   string `json:"code,omitempty" enum:"own_container,node_unreachable,not_found,host_read_only,agent_policy_denied,container_running,action_failed" doc:"Why a target failed. own_container: Bayplan's own server or an agent, refused until it is confirmed by typing its name. node_unreachable: the node is not a connected node of this environment, or its engine did not answer; nothing was sent. not_found: the engine has no such container. container_running: a remove of a running container without force; nothing was sent. host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the node's agent, so Bayplan refused before sending anything. agent_policy_denied: the node's agent refused the call by its own policy, which is what an agent older than this server does for a call it does not know. action_failed: the engine refused it; detail has its message"`
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

// RegisterContainers registers the container action route. Every target is routed
// by node ID through the aggregator's one write path (WriteOnNode), is checked against Bayplan's
// own containers from a fresh inspect, and gets exactly one audit row (D-14). A
// write is never retried (P2 D-42).
func RegisterContainers(r *httpx.Router, d ContainersDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	isSelf := func(id string) bool {
		return len(d.SelfContainerID) >= minSelfIDLen && strings.HasPrefix(id, d.SelfContainerID)
	}

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
			"Bayplan's own server container and any agent container are refused with own_container. " +
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
			if (in.Body.Timeout != nil || in.Body.Signal != "") && len(in.Body.Targets) != 1 {
				return nil, httpx.Problem(http.StatusUnprocessableEntity, problemOptionsSingleOnly,
					"timeout and signal can be set for one container at a time; a bulk request uses each container's own settings")
			}
			spec := verbSpecs[in.Body.Action]
			opts := dockerx.ActionOptions{Timeout: in.Body.Timeout, Signal: in.Body.Signal}
			op := containerOp{verb: spec.verb, opts: opts}

			bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
			if err != nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			results := runTargets(ctx, in.Body.Targets, func(ctx context.Context, t ContainerActionTarget) ContainerActionResult {
				return actOnOne(ctx, d, isSelf, env.ID, t, op)
			})
			extra := map[string]string{}
			if spec.usesTimeout && in.Body.Timeout != nil {
				extra["timeout"] = strconv.Itoa(*in.Body.Timeout)
			}
			if spec.usesSignal && in.Body.Signal != "" {
				extra["signal"] = in.Body.Signal
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
			"Bayplan's own server container and any agent container are refused with own_container. " +
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
			bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
			if err != nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			op := containerOp{remove: true, rm: dockerx.RemoveOptions{Force: in.Body.Force, Volumes: in.Body.RemoveVolumes}}
			results := runTargets(ctx, in.Body.Targets, func(ctx context.Context, t ContainerActionTarget) ContainerActionResult {
				return actOnOne(ctx, d, isSelf, env.ID, t, op)
			})
			extra := map[string]string{"force": strconv.FormatBool(in.Body.Force), "volumes": strconv.FormatBool(in.Body.RemoveVolumes)}
			auditTargets(ctx, d, p, env.ID, in.UserAgent, audit.ActionContainerRemoved, bulkID, extra, in.Body.Targets, results)
			return &ContainerActionOutput{CacheControl: "no-store", Body: ContainerActionResponse{BulkID: bulkID, Results: results}}, nil
		})
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

// auditTargets writes one audit row per target, in request order, once every target has
// answered, so the rows of one bulk read in the order the targets were sent. extra is
// added to every row.
func auditTargets(ctx context.Context, d ContainersDeps, p *rbac.Principal, envID, userAgent, action, bulkID string, extra map[string]string, targets []ContainerActionTarget, results []ContainerActionResult) {
	for i, t := range targets {
		res := results[i]
		result, reason, code := audit.ResultSuccess, "", actionDone
		if res.Result != actionDone {
			result, reason, code = audit.ResultFailure, res.Code, res.Code
		}
		details := map[string]string{"container": res.Name, "node": t.NodeID, "code": code}
		for k, v := range extra {
			details[k] = v
		}
		if bulkID != "" {
			details["bulk"] = bulkID
		}
		if err := d.Audit.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: action, TargetType: containerTargetType, TargetID: t.ID,
			EnvID: envID, Result: result, Reason: reason,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent,
			Details: details,
		}); err != nil {
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
		code, detail := actionError(err)
		return fail(code, detail)
	}
	if own.Name != "" {
		res.Name = own.Name
	}
	if isSelf(own.ID) || own.Labels[ownerLabelAgent] == ownerLabelAgentValue {
		return fail(codeOwnContainer, "This is Bayplan's own container. Acting on it would cut Bayplan off from the host, so it is refused.")
	}
	if op.remove && own.Running && !op.rm.Force {
		return fail(codeContainerRunning, "The container is running. Stop it first, or remove it with force.")
	}

	// A write is sent once. If its answer is lost the outcome is unknown, and the
	// caller refreshes the list to see it (P2 D-42).
	writeCtx, cancelWrite := context.WithTimeout(ctx, op.writeDeadline(own.StopTimeout))
	defer cancelWrite()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, t.NodeID, func(c *dockerx.Client) error {
		if op.remove {
			return c.ContainerRemove(writeCtx, own.ID, op.rm)
		}
		return c.ContainerAction(writeCtx, own.ID, op.verb, op.opts)
	})
	if err != nil {
		code, detail := actionError(err)
		return fail(code, detail)
	}
	res.Result = actionDone
	return res
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
