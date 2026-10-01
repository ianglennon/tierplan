// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Stable problem types of the environment data routes. The SPA maps each to a
// full-page state (D-58).
const (
	problemEnvironmentOffline     = "environment_offline"
	problemEnvironmentUnsupported = "environment_unsupported"
)

// maxListRows is the most rows a list answers with (D-56). The total is always
// the full count, so the page can say "Showing the first 500 of {n}".
const maxListRows = 500

// minSelfIDLen is the shortest container ID prefix trusted to identify
// Bayplan's own container: the 12 characters Docker uses as the default
// hostname.
const minSelfIDLen = 12

// SnapshotSource is what the dashboard routes read. *envs.Snapshots implements it.
type SnapshotSource interface {
	Get(ctx context.Context, envID string) (dockerx.Snapshot, envs.Status, error)
}

// DashboardDeps are the dependencies of the summary and list routes.
type DashboardDeps struct {
	Snapshots SnapshotSource
	// SelfContainerID is Bayplan's own container ID, for the isSelf flag. Empty
	// when Bayplan does not run in a container, or cannot tell.
	SelfContainerID string
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
	Since  string                    `json:"since,omitempty" format:"date-time" doc:"environment_offline: when the engine first stopped answering"`
	Reason string                    `json:"reason,omitempty" doc:"Why, as a stable code such as unreachable, api_too_old, podman, rootless or docker_desktop"`
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

// EnvironmentSummaryBody is the dashboard of one environment.
type EnvironmentSummaryBody struct {
	Environment     SummaryEnvironmentBody `json:"environment"`
	Engine          SummaryEngineBody      `json:"engine"`
	Counts          SummaryCountsBody      `json:"counts"`
	IsWorker        bool                   `json:"isWorker" doc:"True on a swarm worker, which can show only its own containers, images, volumes and networks (D-24)"`
	SelfContainerID string                 `json:"selfContainerId,omitempty" doc:"The ID of Bayplan's own container, when it can tell (D-27)"`
	FetchedAt       string                 `json:"fetchedAt" format:"date-time" doc:"When the counts were read from Docker; they are cached for a few seconds"`
}

// EnvironmentSummaryOutput wraps EnvironmentSummaryBody for huma.
type EnvironmentSummaryOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         EnvironmentSummaryBody
}

// ContainerItem is one row of the containers list. It carries no variables,
// labels or mounts (T-01-77).
type ContainerItem struct {
	ID      string   `json:"id" doc:"The full container ID"`
	Name    string   `json:"name" doc:"The container name without the leading slash"`
	State   string   `json:"state" enum:"created,running,paused,restarting,removing,exited,dead" doc:"The container state"`
	Status  string   `json:"status" doc:"The engine's status text, for example Up 3 hours"`
	Image   string   `json:"image" doc:"The image the container was created from"`
	Created string   `json:"created,omitempty" format:"date-time"`
	Ports   []string `json:"ports" doc:"Ports as 0.0.0.0:8080->80/tcp, or 80/tcp when not published"`
	IsSelf  bool     `json:"isSelf" doc:"True for the container Bayplan itself runs in (D-27)"`
}

// ImageItem is one row of the images list.
type ImageItem struct {
	ID       string   `json:"id" doc:"The full image ID"`
	ShortID  string   `json:"shortId" doc:"The first 12 characters of the ID, without the sha256: prefix"`
	RepoTags []string `json:"repoTags" doc:"repository:tag for each tag, or [\"<none>\"] for an untagged image"`
	Size     int64    `json:"size" doc:"Size in bytes"`
	Created  string   `json:"created,omitempty" format:"date-time"`
}

// VolumeItem is one row of the volumes list.
type VolumeItem struct {
	Name    string `json:"name"`
	Driver  string `json:"driver"`
	Created string `json:"created,omitempty" format:"date-time"`
}

// NetworkItem is one row of the networks list.
type NetworkItem struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Driver  string   `json:"driver"`
	Scope   string   `json:"scope" doc:"local, or swarm for a cluster-wide network"`
	Subnets []string `json:"subnets" doc:"The IPAM subnets in CIDR notation; empty when the network has none"`
	Created string   `json:"created,omitempty" format:"date-time"`
}

// ContainerListBody and its siblings are a capped list with the full total.
type ContainerListBody struct {
	Items     []ContainerItem `json:"items" doc:"At most 500 rows"`
	Total     int             `json:"total" doc:"How many there are, whatever the cap"`
	Truncated bool            `json:"truncated" doc:"True when total is more than the rows returned"`
}

// ImageListBody is a capped list of images.
type ImageListBody struct {
	Items     []ImageItem `json:"items" doc:"At most 500 rows"`
	Total     int         `json:"total" doc:"How many there are, whatever the cap"`
	Truncated bool        `json:"truncated" doc:"True when total is more than the rows returned"`
}

// VolumeListBody is a capped list of volumes.
type VolumeListBody struct {
	Items     []VolumeItem `json:"items" doc:"At most 500 rows"`
	Total     int          `json:"total" doc:"How many there are, whatever the cap"`
	Truncated bool         `json:"truncated" doc:"True when total is more than the rows returned"`
}

// NetworkListBody is a capped list of networks.
type NetworkListBody struct {
	Items     []NetworkItem `json:"items" doc:"At most 500 rows"`
	Total     int           `json:"total" doc:"How many there are, whatever the cap"`
	Truncated bool          `json:"truncated" doc:"True when total is more than the rows returned"`
}

// ContainerListOutput wraps ContainerListBody for huma.
type ContainerListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ContainerListBody
}

// ImageListOutput wraps ImageListBody for huma.
type ImageListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ImageListBody
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
		"503": {Description: "environment_offline: the engine does not answer. Carries since and reason.", Content: media},
		"409": {Description: "environment_unsupported: the engine answers but Bayplan does not manage it. Carries the engine and reason.", Content: media},
	}
}

// RegisterDashboard registers the environment summary and the four read-only
// lists under /api/env/{envId}/. Each needs its own permission on the
// environment (D-48); the counts and the lists come from one short-lived
// snapshot read live from Docker (D-57, Pitfall 22). An offline environment
// answers 503 environment_offline and an unsupported one 409
// environment_unsupported, so the SPA shows the full-page state (D-25, D-58).
func RegisterDashboard(r *httpx.Router, d DashboardDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	responses := problemResponses(r)

	// read fetches the snapshot, turning the monitor's two sentinel errors into
	// the problems the SPA understands.
	read := func(ctx context.Context, envID string) (dockerx.Snapshot, envs.Status, error) {
		snap, st, err := d.Snapshots.Get(ctx, envID)
		switch {
		case err == nil:
			return snap, st, nil
		case errors.Is(err, envs.ErrUnsupported):
			return snap, st, &EnvironmentProblem{
				ErrorModel: huma.ErrorModel{
					Type: problemEnvironmentUnsupported, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
					Detail: "Bayplan does not manage the Docker engine on this host.",
				},
				Reason: st.Reason,
				Engine: &EnvironmentProblemEngine{Name: st.Probe.EngineName, Version: st.Probe.EngineVersion, APIVersion: st.Probe.APIVersion},
			}
		case errors.Is(err, envs.ErrOffline):
			return snap, st, offlineProblem(st.Since, st.Reason)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// The caller gave up, or the read ran out of time. Say so without
			// blaming the engine's status.
			return snap, st, httpx.Problem(http.StatusServiceUnavailable, "", "The environment took too long to answer.")
		default:
			// The engine answered its last probe but not this read. The snapshot
			// cache has already asked the monitor to probe again.
			d.Log.Error("reading an environment failed", "environment", envID, "error", err)
			// There is no honest "offline since" yet, so it is left out.
			return snap, st, offlineProblem(time.Time{}, dockerx.ReasonUnreachable)
		}
	}
	isSelf := func(id string) bool {
		return len(d.SelfContainerID) >= minSelfIDLen && strings.HasPrefix(id, d.SelfContainerID)
	}
	need := func(p rbac.Perm) rbac.Requirement {
		return rbac.Requirement{Perm: p, Scope: rbac.ScopeEnvironment}
	}
	op := func(id, path, summary, description string) huma.Operation {
		return huma.Operation{
			OperationID: id, Method: http.MethodGet, Path: path, Summary: summary,
			Description: description + " 503 environment_offline (with since and reason) when the engine does not answer, " +
				"409 environment_unsupported (with the engine) when it is not one Bayplan manages.",
			Tags: []string{"environments"}, Responses: responses,
		}
	}

	httpx.Register(r, op("getEnvironmentSummary", "/api/env/{envId}/summary",
		"The dashboard of an environment",
		"The engine card and the counts of running and stopped containers, images, volumes and networks, plus nodes and "+
			"services on a swarm manager. Every count is the length of the list its tile links to, because both come from "+
			"the same snapshot, cached for a few seconds."),
		need(rbac.PermEnvironmentsRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*EnvironmentSummaryOutput, error) {
			snap, st, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			p, c := st.Probe, snap.Counts
			return &EnvironmentSummaryOutput{CacheControl: "no-store", Body: EnvironmentSummaryBody{
				Environment: SummaryEnvironmentBody{
					ID: env.ID, Name: env.Name, Type: p.Type,
					Status: EnvironmentStatusBody{
						State: string(st.State), Since: rfc3339(st.Since), Reason: st.Reason, LastAttempt: rfc3339(st.LastAttempt),
					},
				},
				Engine: SummaryEngineBody{
					Name: p.EngineName, Version: p.EngineVersion, APIVersion: p.APIVersion, OS: p.OS, Arch: p.Arch,
					StorageDriver: p.StorageDriver, NCPU: p.NCPU, MemTotal: p.MemTotal,
				},
				Counts: SummaryCountsBody{
					ContainersRunning: c.ContainersRunning, ContainersStopped: c.ContainersStopped, Containers: c.Containers,
					Images: c.Images, Volumes: c.Volumes, Networks: c.Networks, Nodes: c.Nodes, Services: c.Services,
				},
				IsWorker:        p.Type == dockerx.TypeSwarmWorker,
				SelfContainerID: d.SelfContainerID,
				FetchedAt:       rfc3339(snap.FetchedAt),
			}}, nil
		})

	httpx.Register(r, op("listContainers", "/api/env/{envId}/containers",
		"List the containers of an environment",
		"Every container, stopped ones included, by name. Read-only rows: no variables, labels or mounts. At most 500 rows with the total."),
		need(rbac.PermContainersRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*ContainerListOutput, error) {
			snap, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			items, total, truncated := capRows(snap.Containers, func(c dockerx.ContainerRow) ContainerItem {
				return ContainerItem{
					ID: c.ID, Name: c.Name, State: c.State, Status: c.Status, Image: c.Image,
					Created: rfc3339(c.Created), Ports: c.Ports, IsSelf: isSelf(c.ID),
				}
			})
			return &ContainerListOutput{CacheControl: "no-store", Body: ContainerListBody{Items: items, Total: total, Truncated: truncated}}, nil
		})

	httpx.Register(r, op("listImages", "/api/env/{envId}/images",
		"List the images of an environment",
		"Newest first. An untagged image is listed as <none>. At most 500 rows with the total."),
		need(rbac.PermImagesRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*ImageListOutput, error) {
			snap, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			items, total, truncated := capRows(snap.Images, func(i dockerx.ImageRow) ImageItem {
				return ImageItem{ID: i.ID, ShortID: shortImageID(i.ID), RepoTags: i.RepoTags, Size: i.Size, Created: rfc3339(i.Created)}
			})
			return &ImageListOutput{CacheControl: "no-store", Body: ImageListBody{Items: items, Total: total, Truncated: truncated}}, nil
		})

	httpx.Register(r, op("listVolumes", "/api/env/{envId}/volumes",
		"List the volumes of an environment",
		"By name. At most 500 rows with the total."),
		need(rbac.PermVolumesRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*VolumeListOutput, error) {
			snap, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			items, total, truncated := capRows(snap.Volumes, func(v dockerx.VolumeRow) VolumeItem {
				return VolumeItem{Name: v.Name, Driver: v.Driver, Created: rfc3339(v.Created)}
			})
			return &VolumeListOutput{CacheControl: "no-store", Body: VolumeListBody{Items: items, Total: total, Truncated: truncated}}, nil
		})

	httpx.Register(r, op("listNetworks", "/api/env/{envId}/networks",
		"List the networks of an environment",
		"By name. At most 500 rows with the total."),
		need(rbac.PermNetworksRead),
		func(ctx context.Context, in *EnvironmentPathInput) (*NetworkListOutput, error) {
			snap, _, err := read(ctx, in.EnvID)
			if err != nil {
				return nil, err
			}
			items, total, truncated := capRows(snap.Networks, func(n dockerx.NetworkRow) NetworkItem {
				return NetworkItem{ID: n.ID, Name: n.Name, Driver: n.Driver, Scope: n.Scope, Subnets: n.Subnets, Created: rfc3339(n.Created)}
			})
			return &NetworkListOutput{CacheControl: "no-store", Body: NetworkListBody{Items: items, Total: total, Truncated: truncated}}, nil
		})
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
		Since:  rfc3339(since),
		Reason: reason,
	}
}

// shortImageID is the first 12 characters of an image ID, as `docker images`
// shows it.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(len(id), 12)]
}
