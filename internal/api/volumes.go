// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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

	// problemVolumeNotFound is the problem type of a volume page whose volume is not there.
	problemVolumeNotFound = "volume_not_found"

	// codeVolumeInUse is the result of a delete of a volume a container or a service
	// still mounts, found by the check or refused by the engine. It is also the problem
	// type the SPA words.
	codeVolumeInUse = "volume_in_use"
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
	// SizesTimeout is how long one node may take to calculate its volume sizes. Zero means
	// 60 seconds.
	SizesTimeout time.Duration
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

// sizesTimeout is how long one node may take to walk its volumes (T-04-60).
const sizesTimeout = 60 * time.Second

// sizesParallel is how many nodes calculate their sizes at once (T-04-60).
const sizesParallel = 4

// VolumeSizesInput is the path and query of the sizes route.
type VolumeSizesInput struct {
	EnvironmentPathInput
	NodeID string `query:"nodeId" maxLength:"128" doc:"Only this node: the nodeId of a row of the volumes list. Left out, every reporting node"`
}

// VolumeSizeEntry is the size of one volume on one node.
type VolumeSizeEntry struct {
	Name     string `json:"name" doc:"The volume name"`
	Size     int64  `json:"size" doc:"Bytes the volume takes, or -1 when the engine cannot say, as for a volume of a driver other than local"`
	RefCount int64  `json:"refCount" doc:"How many containers reference the volume, or -1 when the engine does not count them"`
}

// VolumeSizesNodeBody is what one node answered.
type VolumeSizesNodeBody struct {
	NodeID string            `json:"nodeId" doc:"The node ID, as in the volumes list"`
	Node   string            `json:"node" doc:"The node's display name"`
	OK     bool              `json:"ok" doc:"False when the node could not calculate its sizes; error says why and sizes is empty"`
	Error  string            `json:"error,omitempty" doc:"Why the node could not calculate, in a plain sentence"`
	Sizes  []VolumeSizeEntry `json:"sizes" doc:"The node's volumes with their sizes, sorted by name"`
}

// VolumeSizesBody is the answer to the sizes route.
type VolumeSizesBody struct {
	Nodes []VolumeSizesNodeBody `json:"nodes" doc:"One entry per node asked, sorted by display name then ID. One node failing never fails the others"`
}

// VolumeSizesOutput wraps VolumeSizesBody for huma.
type VolumeSizesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeSizesBody
}

// VolumeDetailInput is the path and query of the volume detail and inspect routes.
type VolumeDetailInput struct {
	EnvironmentPathInput
	VolumeName string `path:"volumeName" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$" maxLength:"255" doc:"The volume name"`
	NodeID     string `query:"nodeId" maxLength:"128" doc:"The node the volume is on: the nodeId of its row in the volumes list. Volumes are node-local, so it is required on a swarm and may be left out only on an environment of one node"`
}

// VolumeUsedByBody is one container or service that mounts the volume.
type VolumeUsedByBody struct {
	Kind      string `json:"kind" enum:"container,service" doc:"container: on the volume's node, running or not. service: a swarm service whose spec mounts a volume of this name, even with no running task"`
	ID        string `json:"id" doc:"The container ID or the service ID"`
	Name      string `json:"name" doc:"The container or service name"`
	Node      string `json:"node" doc:"The display name of the node a container is on; empty for a service, which can run on any node"`
	NodeID    string `json:"nodeId" doc:"The ID of that node; empty for a service"`
	MountedAt string `json:"mountedAt" doc:"Where the volume appears inside the container; empty when not known"`
	ReadOnly  bool   `json:"readOnly" doc:"True when it is mounted read-only"`
	State     string `json:"state" doc:"A container's state (running, exited ...). For a service: running when any task runs, stopped when it is scaled to zero, starting otherwise; empty when the cluster read does not have it"`
}

// VolumeDetailBody is one volume as its detail page shows it.
type VolumeDetailBody struct {
	Name       string `json:"name" doc:"The volume name"`
	Driver     string `json:"driver"`
	Mountpoint string `json:"mountpoint" doc:"Where the engine keeps the volume's data on the node"`
	Scope      string `json:"scope" doc:"local, or global for a volume a cluster driver shares"`
	Node       string `json:"node" doc:"The display name of the node that holds the volume"`
	NodeID     string `json:"nodeId" doc:"The ID of that node"`
	Created    string `json:"created,omitempty" format:"date-time"`
	Anonymous  bool   `json:"anonymous" doc:"True for a volume Docker made for a container without a name"`
	// Options can hold a share's credentials: their values are withheld on the server from
	// a caller without secrets.reveal.
	Options []redact.OptionVar `json:"options" doc:"The driver options the volume was created with, by name. The values can be credentials: a caller without secrets.reveal on the environment gets each with masked true and no value"`
	Labels  map[string]string  `json:"labels" doc:"Every label of the volume"`

	UsedBy           []VolumeUsedByBody `json:"usedBy" doc:"Every container on the volume's node, running or not, and every swarm service whose spec mounts a volume of this name. Containers first, by name, then services, by name"`
	UsedByContainers int                `json:"usedByContainers" doc:"How many entries of usedBy are containers"`
	UsedByServices   int                `json:"usedByServices" doc:"How many entries of usedBy are services"`
	NotReporting     []string           `json:"notReporting" doc:"Nodes that should have answered and did not, so usedBy may be incomplete"`
}

// VolumeDetailOutput wraps VolumeDetailBody for huma.
type VolumeDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeDetailBody
}

// VolumeInspectBody is the raw inspect document of one volume.
type VolumeInspectBody struct {
	Masked  bool           `json:"masked" doc:"True when a value was replaced by the placeholder <hidden> because the caller may not reveal secrets on this environment"`
	Inspect map[string]any `json:"inspect" doc:"The engine's inspect document, with the values of Options replaced by <hidden> unless the caller holds secrets.reveal"`
}

// VolumeInspectOutput wraps VolumeInspectBody for huma.
type VolumeInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         VolumeInspectBody
}

// RegisterVolumes registers the volume routes. A write goes to one node, is never
// retried (P2 D-42), and every attempt is audited.
func RegisterVolumes(r *httpx.Router, d VolumesDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "volumeSizes", Method: http.MethodGet, Path: "/api/env/{envId}/volumes/sizes",
		Summary: "Calculate the sizes of the volumes, per node",
		Description: "Asks the engine of every node that answered (or only the one named by nodeId) for the disk usage of its volumes and answers one entry per node. " +
			"This makes the engine walk every volume's files, which can take long on a host with large volumes, so the SPA asks only when the user presses Calculate sizes and never on a timer. " +
			"At most 4 nodes calculate at once, each for at most 60 seconds; a node that fails or runs out of time answers ok false with an error, and the others are unaffected. " +
			"A volume's size is -1 when the engine cannot say. " +
			"503 node_unavailable when nodeId is not a connected node of the environment. Needs volumes.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermVolumesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *VolumeSizesInput) (*VolumeSizesOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := volumeSizes(ctx, d, env.ID, in.NodeID)
			if err != nil {
				return nil, err
			}
			return &VolumeSizesOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getVolume", Method: http.MethodGet, Path: "/api/env/{envId}/volumes/{volumeName}",
		Summary: "One volume: its details, options and what uses it",
		Description: "Everything the volume detail page shows, read live from the volume's node: driver, mount point, scope, creation time, driver options and labels, " +
			"and what mounts it: every container on that node, running or not, with where and how it is mounted, and on a swarm every service whose spec mounts a volume of that name, even with no running task. " +
			"The volume lives on one node, named by nodeId (the nodeId of its row in the volumes list); it may be left out only on an environment of one node that is not a swarm (else 422 node_required). " +
			"The option values can be credentials of a remote share: they are withheld on the server and sent only to a caller who holds secrets.reveal on the environment; the others get each option's name with masked true and no value. " +
			"404 volume_not_found when the node has no such volume (it may have been removed meanwhile); 503 node_unavailable when the node is not a connected node of the environment. " +
			"The name sizes is the sizes route, not a volume of that name. Needs volumes.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermVolumesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *VolumeDetailInput) (*VolumeDetailOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getVolume(ctx, d, env.ID, redact.CanReveal(ctx, d.Checker, p, env.ID), in)
			if err != nil {
				return nil, err
			}
			return &VolumeDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectVolume", Method: http.MethodGet, Path: "/api/env/{envId}/volumes/{volumeName}/inspect",
		Summary: "One volume's raw inspect document",
		Description: "The engine's inspect document of the volume as it is, for the Inspect tab. " +
			"The volume is named as the detail route names it, with the same rules and problems (422 node_required, 404 volume_not_found, 503 node_unavailable). " +
			"The driver option values can be credentials of a remote share: for a caller without secrets.reveal on the environment each is replaced by the placeholder <hidden> on the server, and masked says something was. " +
			"Needs volumes.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermVolumesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *VolumeDetailInput) (*VolumeInspectOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			_, _, _, raw, err := readVolume(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			shown, masked, err := redact.MaskVolumeInspect(raw, redact.CanReveal(ctx, d.Checker, p, env.ID))
			if err != nil {
				d.Log.Error("masking a volume inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			doc, err := decodeInspect(shown)
			if err != nil {
				d.Log.Error("reading a volume inspect failed", "environment", env.ID, "error", err)
				return nil, huma.Error500InternalServerError("internal error")
			}
			return &VolumeInspectOutput{CacheControl: "no-store", Body: VolumeInspectBody{Masked: masked, Inspect: doc}}, nil
		})

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

// volumeSizes asks the nodes of the environment for the disk usage of their volumes, a few
// at once and each under its own time limit, and answers what each said. A node that
// fails is an entry of its own and never fails the request.
func volumeSizes(ctx context.Context, d VolumesDeps, envID, nodeID string) (VolumeSizesBody, error) {
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return VolumeSizesBody{}, snapshotProblem(d.Log, envID, err)
	}
	nodes := slices.Clone(agg.Nodes)
	if nodeID != "" {
		nodes = slices.DeleteFunc(nodes, func(n envs.NodeRef) bool { return n.ID != nodeID })
		if len(nodes) == 0 {
			return VolumeSizesBody{}, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
		}
	}
	slices.SortFunc(nodes, func(a, b envs.NodeRef) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	limit := d.SizesTimeout
	if limit <= 0 {
		limit = sizesTimeout
	}
	out := make([]VolumeSizesNodeBody, len(nodes))
	var g errgroup.Group
	g.SetLimit(sizesParallel)
	for i, n := range nodes {
		out[i] = VolumeSizesNodeBody{NodeID: n.ID, Node: n.Name, Sizes: []VolumeSizeEntry{}}
		g.Go(func() error {
			nodeCtx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			var usage map[string]dockerx.VolumeUsage
			err := d.Aggregator.OnNode(nodeCtx, envID, n.ID, func(c *dockerx.Client) error {
				var e error
				usage, e = c.VolumeSizes(nodeCtx)
				return e
			})
			if err != nil {
				d.Log.Warn("could not calculate volume sizes", "environment", envID, "node", n.ID, "error", err)
				out[i].Error = sizesError(err)
				return nil
			}
			out[i].OK = true
			for _, name := range slices.Sorted(maps.Keys(usage)) {
				out[i].Sizes = append(out[i].Sizes, VolumeSizeEntry{Name: name, Size: usage[name].Size, RefCount: usage[name].RefCount})
			}
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: a failure is an entry of its own
	return VolumeSizesBody{Nodes: out}, nil
}

// sizesError words why a node could not calculate its sizes.
func sizesError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "The node took too long to calculate its volume sizes."
	}
	_, detail := actionError(err)
	return detail
}

// readVolume finds the node the volume is on and reads the volume live from its engine. The
// errors are already problems: 422 node_required, 404 volume_not_found, 503 node_unavailable,
// or the environment's own.
func readVolume(ctx context.Context, d VolumesDeps, envID string, in *VolumeDetailInput) (agg envs.Aggregate, node envs.NodeRef, detail dockerx.VolumeDetail, raw []byte, err error) {
	agg, _, err = d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return agg, node, detail, nil, snapshotProblem(d.Log, envID, err)
	}
	nodeID := in.NodeID
	if nodeID == "" {
		// Without a node only an environment of one node that is not a swarm has an
		// answer: anywhere else the volume could be any of several.
		swarm := agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker
		if swarm || len(agg.Nodes) != 1 {
			return agg, node, detail, nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodeRequired,
				"A volume lives on one node. Say which one (nodeId).")
		}
		nodeID = agg.Nodes[0].ID
	}
	for _, n := range agg.Nodes {
		if n.ID == nodeID {
			node = n
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err = d.Aggregator.OnNode(readCtx, envID, nodeID, func(c *dockerx.Client) error {
		var e error
		detail, raw, e = c.VolumeDetail(readCtx, in.VolumeName)
		return e
	})
	switch {
	case err == nil:
		return agg, node, detail, raw, nil
	case dockerx.IsNotFound(err):
		return agg, node, detail, nil, httpx.Problem(http.StatusNotFound, problemVolumeNotFound, "This volume does not exist on that node. It may have been removed.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return agg, node, detail, nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to Bayplan.")
	}
	return agg, node, detail, nil, snapshotProblem(d.Log, envID, err)
}

// getVolume builds the detail page's answer: the volume read live, the containers of its node
// that mount it from the node's list, and the services whose specs mount a volume of that name
// from the cluster read. The option values go through the redact package here, the one place
// they can leave the server (T-04-59).
func getVolume(ctx context.Context, d VolumesDeps, envID string, reveal bool, in *VolumeDetailInput) (VolumeDetailBody, error) {
	agg, node, detail, _, err := readVolume(ctx, d, envID, in)
	if err != nil {
		return VolumeDetailBody{}, err
	}
	body := VolumeDetailBody{
		Name: detail.Name, Driver: detail.Driver, Mountpoint: detail.Mountpoint, Scope: detail.Scope,
		Node: node.Name, NodeID: node.ID, Created: rfc3339(detail.Created),
		Anonymous:    dockerx.IsAnonymousVolume(detail.Name, detail.Labels),
		Options:      redact.Options(detail.Options, reveal),
		Labels:       detail.Labels,
		UsedBy:       []VolumeUsedByBody{},
		NotReporting: agg.NotReporting,
	}
	body.UsedBy = append(body.UsedBy, volumeContainerUsers(agg, node.ID, detail.Name)...)
	body.UsedByContainers = len(body.UsedBy)
	services := d.volumeServiceUsers(ctx, envID, agg, detail.Name)
	body.UsedByServices = len(services)
	body.UsedBy = append(body.UsedBy, services...)
	return body, nil
}

// volumeContainerUsers are the containers of one node, running or not, that mount the named
// volume, by name. A bind mount spelled like the name is not a use, and a container that
// mounts the volume twice is listed once, at its first mount.
func volumeContainerUsers(agg envs.Aggregate, nodeID, name string) []VolumeUsedByBody {
	out := []VolumeUsedByBody{}
	for _, c := range agg.Containers {
		if c.NodeID != nodeID {
			continue
		}
		for _, m := range c.Mounts {
			if m.Type != mountTypeVolume || m.Name != name {
				continue
			}
			out = append(out, VolumeUsedByBody{
				Kind: "container", ID: c.ID, Name: c.Name, Node: c.Node, NodeID: c.NodeID,
				MountedAt: m.Destination, ReadOnly: !m.RW, State: c.State,
			})
			break
		}
	}
	slices.SortFunc(out, func(a, b VolumeUsedByBody) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// volumeServiceUsers are the swarm services whose specs mount a volume of that name, by name,
// from the cluster read. Off a swarm there are none.
func (d VolumesDeps) volumeServiceUsers(ctx context.Context, envID string, agg envs.Aggregate, name string) []VolumeUsedByBody {
	out := []VolumeUsedByBody{}
	if d.Clusters == nil || agg.Target.Probe.Type != dockerx.TypeSwarmManager {
		return out
	}
	// A failed read leaves what the last good one held.
	st, _ := d.Clusters.State(ctx, envID)
	rows := map[string]dockerx.ServiceRow{}
	for _, row := range st.Read.Services {
		rows[row.ID] = row
	}
	for _, ref := range st.ServiceRefs {
		for _, m := range ref.VolumeMounts {
			if m.Source != name {
				continue
			}
			out = append(out, VolumeUsedByBody{
				Kind: "service", ID: ref.ID, Name: ref.Name, MountedAt: m.Target, ReadOnly: m.ReadOnly,
				State: serviceState(rows[ref.ID], rows[ref.ID].ID != ""),
			})
			break
		}
	}
	slices.SortFunc(out, func(a, b VolumeUsedByBody) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// serviceState words a service's state from its task counts: running when any task runs,
// stopped when it is scaled to zero, starting otherwise. Empty when the cluster read did
// not have the service.
func serviceState(row dockerx.ServiceRow, known bool) string {
	switch {
	case !known:
		return ""
	case row.Running > 0:
		return "running"
	case row.Desired == 0:
		return "stopped"
	}
	return "starting"
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
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
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
	// Only a swarm has service specs that can mount a volume.
	agg, _, err := d.Aggregator.Snapshot(ctx, envID)
	if err != nil {
		return nil, snapshotProblem(d.Log, envID, err)
	}
	swarm := agg.Target.Probe.Type == dockerx.TypeSwarmManager || agg.Target.Probe.Type == dockerx.TypeSwarmWorker
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
			res, driver, attempted := deleteOneVolume(ctx, d, envID, swarm, t)
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

// volumeBusyError is what the delete closure answers when something mounts the volume:
// the containers and services that do, named for the refusal.
type volumeBusyError struct{ users []string }

func (e *volumeBusyError) Error() string {
	return "api: volume is in use by " + strings.Join(e.users, ", ")
}

// errServicesUnread is what a delete answers when the services of a swarm could not be
// read, so whether one of them mounts the volume is not known.
var errServicesUnread = errors.New("api: could not read the swarm services")

// deleteOneVolume removes one volume on its node and says what happened. It never
// returns an error: a failure is a result of its own. Nothing is removed on the strength of
// the list the caller saw: just before the removal the node's containers, running or not,
// are read again and, on a swarm, the service specs are read from a manager, and anything
// that mounts the volume refuses the delete without a call to remove it being made. force
// is never sent, so a container that starts in between is refused by the engine. driver
// and attempted are for the audit row: attempted is false for a volume that does not
// exist, which there is nothing to record about.
func deleteOneVolume(ctx context.Context, d VolumesDeps, envID string, swarm bool, t VolumeDeleteTarget) (res VolumeDeleteResult, driver string, attempted bool) {
	res = VolumeDeleteResult{NodeID: t.NodeID, Name: t.Name}
	fail := func(code, detail string) VolumeDeleteResult {
		res.Result, res.Code, res.Detail = actionFailed, code, detail
		return res
	}
	var serviceUsers []string
	if swarm {
		users, err := d.serviceUsers(ctx, envID, t.Name)
		if err != nil {
			return fail(codeNodeUnreachable, "Can't tell whether "+t.Name+" is in use: no manager answered. Try again when one is back."), "", true
		}
		serviceUsers = users
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
		containers, e := c.VolumeUsers(writeCtx, t.Name)
		if e != nil {
			return e
		}
		var users []string
		for _, name := range containers {
			users = append(users, "container "+name)
		}
		for _, name := range serviceUsers {
			users = append(users, "service "+name)
		}
		if len(users) > 0 {
			return &volumeBusyError{users: users}
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

// serviceUsers reads the service specs from a manager now and names the services that
// mount the volume. A service spec counts even when it has no running task (Pitfall 7).
func (d VolumesDeps) serviceUsers(ctx context.Context, envID, name string) ([]string, error) {
	if d.Clusters == nil {
		return nil, errServicesUnread
	}
	readCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	var refs []dockerx.ServiceRef
	if err := d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		refs, e = c.ServiceRefs(readCtx)
		return e
	}); err != nil {
		d.Log.Warn("could not read the services before a volume delete", "environment", envID, "error", err)
		return nil, errServicesUnread
	}
	var users []string
	for _, ref := range refs {
		if slices.Contains(ref.Volumes, name) {
			users = append(users, ref.Name)
		}
	}
	return users, nil
}

// volumeRemoveError maps the error of a removal to a result code and a sentence. An
// engine that says the volume is gone is not_found, and one that says something mounts it,
// which can happen when a container starts between the check and the call, is
// volume_in_use and never a generic failure.
func volumeRemoveError(name string, err error) (code, detail string) {
	var busy *volumeBusyError
	switch {
	case errors.As(err, &busy):
		return codeVolumeInUse, name + " is still in use by " + joinMore(busy.users, maxNamedUsers) + ". Remove what uses it, then try again."
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
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: audit.ActionVolumeRemoved, TargetType: volumeTargetType, TargetID: t.Name,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit a volume action", "action", audit.ActionVolumeRemoved, "volume", t.Name, "error", err)
	}
}

// volumeUsage is what mounts the volumes of an environment: containers per node and name,
// and the swarm services whose specs mount a name.
type volumeUsage struct {
	containers map[[2]string]int
	services   map[string]int
}

// volumeUsageOf counts the mounts in an aggregate. Services come from the cluster read of
// a swarm, whatever age it has: a delete reads them live and never trusts this.
func volumeUsageOf(ctx context.Context, clusters *envs.Clusters, envID string, agg envs.Aggregate) volumeUsage {
	u := volumeUsage{containers: map[[2]string]int{}, services: map[string]int{}}
	for _, c := range agg.Containers {
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			// A container that mounts one volume twice uses it once.
			if m.Type == mountTypeVolume && m.Name != "" && !seen[m.Name] {
				seen[m.Name] = true
				u.containers[[2]string{c.NodeID, m.Name}]++
			}
		}
	}
	if clusters != nil && agg.Target.Probe.Type == dockerx.TypeSwarmManager {
		// A failed read leaves what the last good one held.
		st, _ := clusters.State(ctx, envID)
		for _, ref := range st.ServiceRefs {
			for _, name := range ref.Volumes {
				u.services[name]++
			}
		}
	}
	return u
}

// mountTypeVolume is the type of a mount of a named volume.
const mountTypeVolume = "volume"

// of is the containers and services that mount one row. A container counts only on its own
// node, since another node's volume of the same name is a different volume; a service can
// run on any node, so it counts for every node's copy.
func (u volumeUsage) of(v envs.VolumeRow) (containers, services int) {
	return u.containers[[2]string{v.NodeID, v.Name}], u.services[v.Name]
}

// filterVolumes keeps the volumes whose name, driver, any label (name or value), stack or
// project contains q, ignoring case, and that the usage filter lets through. An empty q
// keeps them all; usage is all, used or unused.
func filterVolumes(rows []envs.VolumeRow, q, usage string, use volumeUsage) []envs.VolumeRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" && (usage == "" || usage == "all") {
		return rows
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	out := make([]envs.VolumeRow, 0, len(rows))
	for _, v := range rows {
		if q != "" && !has(v.Name) && !has(v.Driver) && !labelMatches(v.Labels, has) {
			continue
		}
		containers, services := use.of(v)
		switch used := containers+services > 0; {
		case usage == "used" && !used, usage == "unused" && used:
			continue
		}
		out = append(out, v)
	}
	return out
}

// labelMatches reports whether any label's name or value satisfies has.
func labelMatches(labels map[string]string, has func(string) bool) bool {
	for k, val := range labels {
		if has(k) || has(val) {
			return true
		}
	}
	return false
}
