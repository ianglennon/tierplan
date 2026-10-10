// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

const volumeTargetType = "volume"

// errVolumeExists is what the create closure answers when the node already has a
// volume of that name. The engine would answer such a create with the existing volume
// and no error, which would read as a success that changed nothing.
var errVolumeExists = errors.New("api: volume name is taken on the node")

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
