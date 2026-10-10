// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
)

const (
	volumeTargetType = "volume"

	// codeVolumeInUse is the result of a delete of a volume a container or a service
	// still mounts, found by the check or refused by the engine. It is also the problem
	// type the SPA words.
	codeVolumeInUse = "volume_in_use"
	// maxVolumeTargets is how many volumes one delete request names.
	maxVolumeTargets = 25
)

// bulkPhrase is the confirmation of a delete of several volumes: delete {n} volumes. The
// number is the size of the whole selection, which a request of one chunk may be only a
// part of.
var bulkPhrase = regexp.MustCompile(`^delete ([0-9]{1,9}) volumes?$`)

// errVolumeExists is what the create closure answers when the node already has a
// volume of that name. The engine would answer such a create with the existing volume
// and no error, which would read as a success that changed nothing.
var errVolumeExists = errors.New("api: volume name is taken on the node")

// errVolumeGone is what the delete closure answers when the node has no volume of that name.
var errVolumeGone = errors.New("api: no such volume on the node")

// VolumesDeps are the dependencies of the volume routes.
type VolumesDeps struct {
	// Aggregator reaches an engine: volumes are node-local, so every write goes
	// through WriteOnNode.
	Aggregator *envs.Aggregator
	// Clusters reads the services of a swarm live, for the routes that ask what uses a
	// volume.
	Clusters *envs.Clusters
	Audit    *audit.Writer
	// Checker decides whether a caller may see option values.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// VolumeCreateRequest is the body of POST /api/env/{envId}/volumes.
type VolumeCreateRequest struct {
	Name          string            `json:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$" maxLength:"255" doc:"The volume name"`
	NodeID        string            `json:"nodeId,omitempty" maxLength:"128" doc:"The node to create the volume on: the nodeId of a row of a list. Volumes are node-local. Left out only on an environment of one node that is not a swarm"`
	Driver        string            `json:"driver,omitempty" pattern:"^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$" maxLength:"128" doc:"The volume driver. Left out it is local"`
	DriverOptions map[string]string `json:"driverOptions,omitempty" maxProperties:"64" doc:"Driver options, at most 64. For the local driver: type, o and device. They can hold a share password, so they are never audited as they are"`
	Labels        map[string]string `json:"labels,omitempty" maxProperties:"64" doc:"Labels, at most 64"`
}

// VolumeCreateInput is the path and body of the create route.
type VolumeCreateInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      VolumeCreateRequest
}

// VolumeCreateBody is the answer to a create.
type VolumeCreateBody struct {
	Name   string `json:"name" doc:"The volume name"`
	NodeID string `json:"nodeId" doc:"The ID of the node the volume was made on"`
}

// VolumeCreateOutput wraps VolumeCreateBody for huma.
type VolumeCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeCreateBody
}

// VolumeDeleteTarget names one volume: the node it is on and its name. Volumes are
// node-local, so the same name on two nodes is two volumes.
type VolumeDeleteTarget struct {
	NodeID string `json:"nodeId" minLength:"1" maxLength:"128" doc:"The node the volume is on: the nodeId of its row in the volumes list. Never the display name"`
	Name   string `json:"name" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$" maxLength:"255" doc:"The volume name"`
}

// VolumeDeleteRequest is the body of POST /api/env/{envId}/volumes/delete.
type VolumeDeleteRequest struct {
	Confirm string               `json:"confirm" maxLength:"256" doc:"The confirmation, typed: the volume's name for a single target, else the phrase delete {n} volumes where n is at least the number of targets (a bulk of more than 25 is sent in chunks, each carrying the phrase for the whole selection). Anything else is 422 confirmation_required and nothing is sent"`
	BulkID  string               `json:"bulkId,omitempty" format:"uuid" doc:"Groups the audit rows of one bulk request. Send a UUID to name the bulk yourself; with more than one target and none sent, the server makes one (a UUIDv7) and answers it"`
	Targets []VolumeDeleteTarget `json:"targets" minItems:"1" maxItems:"25" doc:"The volumes to delete, at most 25. Each is answered separately; one that fails never stops the others"`
}

// VolumeDeleteInput is the path and body of the delete route.
type VolumeDeleteInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      VolumeDeleteRequest
}

// VolumeDeleteResult is the answer for one target.
type VolumeDeleteResult struct {
	NodeID string `json:"nodeId" doc:"The node ID the request named"`
	Name   string `json:"name" doc:"The volume name the request named"`
	Result string `json:"result" enum:"done,failed" doc:"done: the engine removed the volume. failed: it was refused or could not be sent; code says why"`
	Code   string `json:"code,omitempty" enum:"volume_in_use,not_found,node_unreachable,host_read_only,agent_policy_denied,action_failed" doc:"Why a target failed. volume_in_use: a container on the node, running or not, or a swarm service, mounts the volume; nothing was removed and detail names up to 5 of them. not_found: the node has no such volume, so it is already gone. node_unreachable: the node is not a connected node of this environment, its engine did not answer, or on a swarm no manager could be asked for the services; nothing was removed. host_read_only: the host owner set BAYPLAN_AGENT_READ_ONLY on the node's agent. agent_policy_denied: the node's agent refused the call by its own policy. action_failed: the engine refused it; detail has its message"`
	Detail string `json:"detail,omitempty" doc:"A plain sentence, or for action_failed the engine's message, at most 300 characters"`
}

// VolumeDeleteResponse is the per-target outcome, in request order.
type VolumeDeleteResponse struct {
	BulkID  string               `json:"bulkId,omitempty" doc:"The bulk ID recorded in the audit rows of this request: the one sent, or the one the server made for a request with more than one target. Absent for a single target with none sent"`
	Results []VolumeDeleteResult `json:"results" doc:"One entry per requested target, in request order"`
}

// VolumeDeleteOutput wraps VolumeDeleteResponse for huma.
type VolumeDeleteOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeDeleteResponse
}

// RegisterVolumes registers the volume routes. A write goes to one node, is never
// retried (P2 D-42), and every attempt is audited.
func RegisterVolumes(r *httpx.Router, d VolumesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "createVolume", Method: http.MethodPost, Path: "/api/env/{envId}/volumes",
		Summary: "Create a volume",
		Description: "Creates a volume on one node, once and never retried. " +
			"Volumes are node-local: nodeId names the node (the nodeId of a row of a list) and may be left out only on an environment of one node that is not a swarm (else 422 node_required). " +
			"The driver is local unless one is named. " +
			"409 name_taken when the node already has a volume of that name (the engine would silently reuse it), 409 host_read_only on a read-only host, 503 node_unavailable when the node is not a connected node of the environment. " +
			"Needs volumes.create (Admin) on the environment. Every attempt is audited as volume.created, with result failure and the code when it was refused. " +
			"The audit row carries the driver option names and values with any password-like part replaced.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated,
	}, rbac.Requirement{Perm: rbac.PermVolumesCreate, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *VolumeCreateInput) (*VolumeCreateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return createVolume(ctx, d, p, env.ID, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteVolumes", Method: http.MethodPost, Path: "/api/env/{envId}/volumes/delete",
		Summary: "Delete volumes",
		Description: "Deletes up to 25 volumes, each named by its node ID and name, and answers one result per target in request order. " +
			"This is irreversible: the data in the volume is gone. The caller types the confirmation: the volume's name for a single target, or delete {n} volumes for several, " +
			"where n is at least the number of targets in the request (else 422 confirmation_required and nothing is sent). " +
			"Volumes are node-local: a volume is deleted on the node named, and the same name on another node is left alone. " +
			"A volume that any container on its node, running or stopped, or any swarm service mounts is never deleted: the result is failed with code volume_in_use and the detail names up to 5 users. " +
			"The users are read again from the node and from a manager just before each removal, and force is never sent, so a container that mounts the volume between the check and the call makes the engine refuse and is answered volume_in_use too. " +
			"A volume the node does not have is failed with code not_found. One that fails never stops the others. " +
			"Needs volumes.delete (an Operator or above with the delete volumes permission) on the environment; a caller without it is refused 403 and the refusal is audited as permission.denied. " +
			"With more than one target (or a bulkId sent) every audit row of the request carries the same bulk detail, a UUIDv7 the server makes when the client sends none, and the response returns it. " +
			"Every attempt on a volume that exists is audited as volume.removed, failures included.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusOK,
	}, rbac.Requirement{Perm: rbac.PermVolumesDelete, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *VolumeDeleteInput) (*VolumeDeleteOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return deleteVolumes(ctx, d, p, env.ID, in)
		})
}

// createVolume checks the request, finds the node the volume belongs on, checks that the
// name is free there, creates the volume once and audits the attempt.
func createVolume(ctx context.Context, d VolumesDeps, p *rbac.Principal, envID string, in *VolumeCreateInput) (*VolumeCreateOutput, error) {
	body := in.Body
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	swarm := agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker
	nodeID := body.NodeID
	if nodeID == "" {
		if swarm || len(agg.Nodes) != 1 {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
				"A volume lives on one node. Say which one to create it on (nodeId).")
		}
		nodeID = agg.Nodes[0].ID
	}
	driver := body.Driver
	if driver == "" {
		driver = dockerx.DriverLocal
	}
	spec := dockerx.VolumeCreateSpec{Name: body.Name, Driver: driver, DriverOpts: body.DriverOptions, Labels: body.Labels}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnNode(writeCtx, envID, nodeID, func(c *dockerx.Client) error {
		exists, e := c.VolumeExists(writeCtx, spec.Name)
		if e != nil {
			return e
		}
		if exists {
			return errVolumeExists
		}
		return c.VolumeCreate(writeCtx, spec)
	})
	if err != nil {
		code, problem := volumeCreateProblem(d.Log, envID, body.Name, err)
		auditVolume(ctx, d, p, envID, in.UserAgent, nodeID, spec, code)
		return nil, problem
	}
	auditVolume(ctx, d, p, envID, in.UserAgent, nodeID, spec, codeCreated)
	return &VolumeCreateOutput{CacheControl: "no-store", Body: VolumeCreateBody{Name: body.Name, NodeID: nodeID}}, nil
}

// volumeCreateProblem turns the error of a create into the audit code and the problem
// the SPA understands.
func volumeCreateProblem(log *slog.Logger, envID, name string, err error) (string, error) {
	if errors.Is(err, errVolumeExists) || dockerx.IsConflict(err) {
		return problemNameTaken, httpx.Problem(http.StatusConflict, problemNameTaken, "A volume named "+name+" already exists on that node.")
	}
	return networkWriteProblem(log, envID, err)
}

// auditVolume writes the row of one volume create attempt. A row that cannot be written
// is logged: the volume has been made or refused, and the caller must still be told which.
func auditVolume(ctx context.Context, d VolumesDeps, p *rbac.Principal, envID, userAgent, nodeID string, spec dockerx.VolumeCreateSpec, code string) {
	details := map[string]string{"name": spec.Name, "node": nodeID, "driver": spec.Driver, "code": code}
	// The options can hold a share's password: the row carries them with any
	// password-like part replaced (T-04-55).
	if opts := redact.AuditOptions(spec.DriverOpts); opts != "" {
		details["options"] = opts
	}
	result, reason := audit.ResultSuccess, ""
	if code != codeCreated {
		result, reason = audit.ResultFailure, code
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionVolumeCreated, TargetType: volumeTargetType, TargetID: spec.Name,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a volume action", "action", audit.ActionVolumeCreated, "volume", spec.Name, "error", err)
	}
}

// confirmedVolumes reports whether what was typed confirms the request. A single target
// is confirmed by its name, typed; several by delete {n} volumes with n at least their
// number. The last chunk of a bulk can hold one target, so a single target that belongs
// to a bulk (it carries a bulk ID) is confirmed by the phrase too.
func confirmedVolumes(in VolumeDeleteRequest) bool {
	typed := strings.TrimSpace(in.Confirm)
	single := len(in.Targets) == 1
	if single && typed == in.Targets[0].Name {
		return true
	}
	if single && in.BulkID == "" {
		return false
	}
	m := bulkPhrase.FindStringSubmatch(typed)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err == nil && n >= len(in.Targets)
}

// deleteVolumes checks the confirmation, then removes each volume on its own node, once
// and never retried, answering each target on its own. A target that fails never stops the
// others.
func deleteVolumes(ctx context.Context, d VolumesDeps, p *rbac.Principal, envID string, in *VolumeDeleteInput) (*VolumeDeleteOutput, error) {
	if !confirmedVolumes(in.Body) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired,
			"Type the volume's name, or for several volumes delete {n} volumes, to confirm this.")
	}
	bulkID, err := bulkIDFor(in.Body.BulkID, len(in.Body.Targets))
	if err != nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	results := make([]VolumeDeleteResult, len(in.Body.Targets))
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	// What the audit row of each target needs, written once every target has answered so
	// the rows of one bulk read in the order the targets were sent.
	type attempt struct {
		driver    string
		attempted bool
	}
	attempts := make([]attempt, len(results))
	for i, t := range in.Body.Targets {
		g.Go(func() error {
			release, err := lim.acquire(ctx, t.NodeID)
			if err != nil {
				results[i] = VolumeDeleteResult{NodeID: t.NodeID, Name: t.Name, Result: actionFailed, Code: codeActionFailed, Detail: "The request was cancelled before this target was reached."}
				return nil
			}
			defer release()
			res, driver, attempted := deleteOneVolume(ctx, d, envID, t)
			results[i], attempts[i] = res, attempt{driver: driver, attempted: attempted}
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: failures are results
	for i, t := range in.Body.Targets {
		if attempts[i].attempted {
			auditVolumeRemoved(ctx, d, p, envID, in.UserAgent, bulkID, t, attempts[i].driver, results[i])
		}
	}
	return &VolumeDeleteOutput{CacheControl: "no-store", Body: VolumeDeleteResponse{BulkID: bulkID, Results: results}}, nil
}

// deleteOneVolume removes one volume on its node and says what happened. It never
// returns an error: a failure is a result of its own. driver and attempted are for the
// audit row: attempted is false for a volume that does not exist, which there is nothing
// to record about.
func deleteOneVolume(ctx context.Context, d VolumesDeps, envID string, t VolumeDeleteTarget) (res VolumeDeleteResult, driver string, attempted bool) {
	res = VolumeDeleteResult{NodeID: t.NodeID, Name: t.Name}
	fail := func(code, detail string) VolumeDeleteResult {
		res.Result, res.Code, res.Detail = actionFailed, code, detail
		return res
	}
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	exists := false
	err := d.Aggregator.WriteOnNode(writeCtx, envID, t.NodeID, func(c *dockerx.Client) error {
		var e error
		driver, exists, e = c.VolumeDriver(writeCtx, t.Name)
		if e != nil {
			return e
		}
		if !exists {
			return errVolumeGone
		}
		return c.VolumeRemove(writeCtx, t.Name)
	})
	if err != nil {
		code, detail := volumeRemoveError(t.Name, err)
		// A volume that is not there has nothing to record about.
		return fail(code, detail), driver, exists || code != codeNotFound
	}
	res.Result = actionDone
	return res, driver, true
}

// volumeRemoveError maps the error of a removal to a result code and a sentence. An
// engine that says the volume is gone is not_found, and one that says something mounts it,
// which can happen when a container starts between the check and the call, is
// volume_in_use and never a generic failure.
func volumeRemoveError(name string, err error) (code, detail string) {
	switch {
	case dockerx.IsNotFound(err), errors.Is(err, errVolumeGone):
		return codeNotFound, name + " is already gone."
	case dockerx.IsConflict(err):
		return codeVolumeInUse, name + " is still in use. Remove what uses it, then try again."
	}
	return actionError(err)
}

// auditVolumeRemoved writes the row of one delete attempt on a volume that exists. A row
// that cannot be written is logged: the volume has been removed or refused, and the caller
// must still be told which.
func auditVolumeRemoved(ctx context.Context, d VolumesDeps, p *rbac.Principal, envID, userAgent, bulkID string, t VolumeDeleteTarget, driver string, res VolumeDeleteResult) {
	result, reason, code := audit.ResultSuccess, "", codeRemoved
	if res.Result != actionDone {
		result, reason, code = audit.ResultFailure, res.Code, res.Code
	}
	details := map[string]string{"name": t.Name, "node": t.NodeID, "code": code}
	if driver != "" {
		details["driver"] = driver
	}
	if bulkID != "" {
		details["bulk"] = bulkID
	}
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionVolumeRemoved, TargetType: volumeTargetType, TargetID: t.Name,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a volume action", "action", audit.ActionVolumeRemoved, "volume", t.Name, "error", err)
	}
}
