// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/redact"
	"github.com/ianglennon/bayplan/internal/registry"
)

const (
	problemServiceNotFound = "service_not_found"
	problemNoQuorum        = "no_quorum"
)

// ServiceDetailInput is the path of one service: its ID or its name.
type ServiceDetailInput struct {
	EnvironmentPathInput
	ServiceID string `path:"serviceId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The service ID or name: the id of a row of the services list"`
}

// ServiceImageBody is the image a service's spec names.
type ServiceImageBody struct {
	Ref    string `json:"ref" doc:"The image as the spec wrote it; empty when the spec has no container spec"`
	Tag    string `json:"tag" doc:"The tag, latest when the reference names none; empty for a reference by digest alone or one that is not a valid reference"`
	Digest string `json:"digest" doc:"The digest the reference is pinned to (sha256:...); empty when it is not pinned"`
	Pinned bool   `json:"pinned" doc:"True when the reference carries a digest"`
}

// ServicePortBody is one port the service publishes.
type ServicePortBody struct {
	Published uint32 `json:"published" doc:"The port on the swarm, or on the node for host mode; 0 while the swarm has not assigned one"`
	Target    uint32 `json:"target" doc:"The port in the container"`
	Protocol  string `json:"protocol" doc:"tcp, udp or sctp"`
	Mode      string `json:"mode" doc:"ingress (the routing mesh) or host (only the node that runs the task)"`
}

// ServiceMountBody is one mount of the service's container spec.
type ServiceMountBody struct {
	Type     string `json:"type" doc:"volume, bind, tmpfs, npipe, cluster or image"`
	Source   string `json:"source" doc:"The volume name or host path; empty for a tmpfs"`
	Target   string `json:"target" doc:"Where it appears in the container"`
	ReadOnly bool   `json:"readOnly" doc:"True when mounted read-only"`
}

// ServiceFileRefBody is a secret or a config the service uses. Neither value is ever sent:
// this is the reference only.
type ServiceFileRefBody struct {
	ID   string `json:"id" doc:"The secret's or config's ID"`
	Name string `json:"name" doc:"Its name"`
	File string `json:"file" doc:"The file name in the container; empty for a config used at runtime rather than as a file"`
	UID  string `json:"uid" doc:"The file's owner"`
	GID  string `json:"gid" doc:"The file's group"`
	Mode string `json:"mode" doc:"The file's permissions in octal (0444); empty when there is no file"`
}

// ServiceNetworkBody is a network the service is attached to.
type ServiceNetworkBody struct {
	ID      string   `json:"id" doc:"The network's ID; what the spec wrote when no network of the swarm has that name or ID"`
	Name    string   `json:"name" doc:"The network's name; the ID when the swarm has no such network"`
	Aliases []string `json:"aliases" doc:"The service's aliases on the network"`
}

// ServiceResourcesBody is the CPU and memory a service asks for. 0 means none: Docker's own
// way of saying no limit and no reservation.
type ServiceResourcesBody struct {
	CPULimit          float64 `json:"cpuLimit" doc:"CPU limit in cores; 0 for none"`
	CPUReservation    float64 `json:"cpuReservation" doc:"CPU reservation in cores; 0 for none"`
	MemoryLimit       int64   `json:"memoryLimit" doc:"Memory limit in bytes; 0 for none"`
	MemoryReservation int64   `json:"memoryReservation" doc:"Memory reservation in bytes; 0 for none"`
}

// ServicePlacementBody is where the swarm may put the service's tasks.
type ServicePlacementBody struct {
	Constraints []string `json:"constraints" doc:"The placement constraints, as written (node.role==worker)"`
	Preferences []string `json:"preferences" doc:"The spread preferences: the label each spreads over (node.labels.zone)"`
	MaxReplicas uint64   `json:"maxReplicas" doc:"The most tasks of the service on one node; 0 for no limit"`
}

// ServiceRestartPolicyBody is the restart policy of the service's tasks.
type ServiceRestartPolicyBody struct {
	Condition   string `json:"condition" doc:"none, on-failure or any"`
	Delay       string `json:"delay" doc:"How long to wait between restarts, as a Go duration (5s); empty for the engine's default"`
	MaxAttempts uint64 `json:"maxAttempts" doc:"The most restart attempts before giving up; 0 for no limit"`
	Window      string `json:"window" doc:"How long to wait before a restart counts as having worked, as a Go duration; empty for the engine's default"`
}

// ServiceUpdatePolicyBody is how the swarm rolls an update out.
type ServiceUpdatePolicyBody struct {
	Parallelism     uint64  `json:"parallelism" doc:"How many tasks are updated at once; 0 for all at once"`
	Delay           string  `json:"delay" doc:"The pause between batches, as a Go duration; empty for none"`
	Order           string  `json:"order" doc:"stop-first or start-first; empty for the engine's default"`
	FailureAction   string  `json:"failureAction" doc:"pause, continue or rollback; empty for the engine's default"`
	Monitor         string  `json:"monitor" doc:"How long each task is watched for failure, as a Go duration; empty for the engine's default"`
	MaxFailureRatio float64 `json:"maxFailureRatio" doc:"The share of failed tasks that is tolerated, 0 to 1"`
}

// ServiceDetailBody is everything the service detail page shows. Every value that can be a
// secret has gone through the redact package before it is put here: without secrets.reveal
// the environment values are withheld (D-39).
type ServiceDetailBody struct {
	ID        string `json:"id" doc:"The service ID"`
	Name      string `json:"name" doc:"The service name"`
	Mode      string `json:"mode" enum:"replicated,global,replicated-job,global-job" doc:"How the swarm runs it"`
	Running   uint64 `json:"running" doc:"Tasks that are running now, from the last cluster read"`
	Desired   uint64 `json:"desired" doc:"Tasks the swarm wants running, from the last cluster read; for a replicated service whose row was not read yet, the spec's replica count"`
	Stack     string `json:"stack" doc:"The stack the service belongs to (com.docker.stack.namespace); empty for a service made on its own"`
	Agent     bool   `json:"agent" doc:"True for a Bayplan agent service"`
	CreatedAt string `json:"createdAt" format:"date-time" doc:"When the service was created"`
	UpdatedAt string `json:"updatedAt" format:"date-time" doc:"When the service's spec last changed"`

	Image    ServiceImageBody     `json:"image" doc:"The image the spec names"`
	Ports    []ServicePortBody    `json:"ports" doc:"The ports the service publishes, sorted by published port; empty when it publishes none"`
	Mounts   []ServiceMountBody   `json:"mounts" doc:"The mounts of the container spec"`
	Secrets  []ServiceFileRefBody `json:"secrets" doc:"The swarm secrets the service uses, by reference; no value is ever sent"`
	Configs  []ServiceFileRefBody `json:"configs" doc:"The swarm configs the service uses, by reference"`
	Networks []ServiceNetworkBody `json:"networks" doc:"The networks the service is attached to"`

	Env       []redact.EnvVar `json:"env" doc:"The environment variables in spec order. Without secrets.reveal each is the name with masked true and no value"`
	EnvMasked bool            `json:"envMasked" doc:"True when the values were withheld because the caller may not reveal secrets on this environment"`

	Resources     ServiceResourcesBody      `json:"resources" doc:"CPU and memory limits and reservations"`
	Placement     ServicePlacementBody      `json:"placement" doc:"Where tasks may run"`
	RestartPolicy *ServiceRestartPolicyBody `json:"restartPolicy,omitempty" doc:"The restart policy; absent when the spec sets none (the engine's default, restart on any exit)"`
	UpdatePolicy  *ServiceUpdatePolicyBody  `json:"updatePolicy,omitempty" doc:"The update policy; absent when the spec sets none (the engine's defaults)"`
	Labels        map[string]string         `json:"labels" doc:"The service's own labels, as Docker reports them"`

	UpdateStatus    string `json:"updateStatus" doc:"The state of the last update or rollback (updating, paused, completed, rollback_started, rollback_paused, rollback_completed); empty when it has none"`
	UpdateMessage   string `json:"updateMessage" doc:"The swarm's message about the last update; empty when it has none"`
	HasPreviousSpec bool   `json:"hasPreviousSpec" doc:"True when the swarm still holds the spec before the last update, so a rollback has something to go back to"`
}

// ServiceDetailOutput wraps ServiceDetailBody for huma.
type ServiceDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceDetailBody
}

// ServiceInspectBody is the raw inspect document of one service.
type ServiceInspectBody struct {
	Masked   bool           `json:"masked" doc:"True when a value was replaced by the placeholder <hidden> because the caller may not reveal secrets on this environment"`
	Document map[string]any `json:"document" doc:"The manager's inspect document of the service, with the values of the environment variables, the command line, the health check command and the log driver's options replaced by <hidden>, in Spec and in PreviousSpec, unless the caller holds secrets.reveal"`
}

// ServiceInspectOutput wraps ServiceInspectBody for huma.
type ServiceInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ServiceInspectBody
}

// registerServiceDetail mounts the service detail and inspect routes.
func registerServiceDetail(r *httpx.Router, d ServicesDeps) {
	httpx.Register(r, huma.Operation{
		OperationID: "getService", Method: http.MethodGet, Path: "/api/env/{envId}/services/{serviceId}",
		Summary: "One swarm service: its settings",
		Description: "Everything the service detail page shows, read live from a manager: mode, task counts, image (tag, digest, pinned), published ports, mounts, secrets and configs by reference, networks with their names, environment variables, resources, placement, restart and update policy and labels. " +
			"The values of the environment variables can be credentials: they are withheld on the server, and sent only to a caller who holds secrets.reveal on the environment. env then lists the names with masked true and no value, and envMasked says so. " +
			"The command line, the health check and the log driver's options are not part of this answer; they are in the inspect document, where they are withheld the same way. " +
			"404 service_not_found when the swarm has no such service; 409 not_a_swarm for a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceDetailInput) (*ServiceDetailOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getService(ctx, d, p, env.ID, in.ServiceID)
			if err != nil {
				return nil, err
			}
			return &ServiceDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectService", Method: http.MethodGet, Path: "/api/env/{envId}/services/{serviceId}/inspect",
		Summary: "One swarm service's raw inspect document",
		Description: "The manager's inspect document of the service, for the Inspect tab. " +
			"The environment variables' values, the command line (Command and Args), the command of the health check and the log driver's option values, in Spec and in PreviousSpec, can be credentials: for a caller without secrets.reveal on the environment each is replaced by the placeholder <hidden> on the server, and masked says something was. " +
			"The same problems as the service detail route (404 service_not_found, 409 not_a_swarm, 503 no_manager_connected). " +
			"Needs services.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermServicesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ServiceDetailInput) (*ServiceInspectOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getServiceInspect(ctx, d, p, env.ID, in.ServiceID)
			if err != nil {
				return nil, err
			}
			return &ServiceInspectOutput{CacheControl: "no-store", Body: body}, nil
		})
}

// readService reads one service from a manager, and the swarm's networks too when
// withNetworks, in one cluster read. The errors are already problems.
func readService(ctx context.Context, d ServicesDeps, envID, serviceID string, withNetworks bool) (detail dockerx.ServiceDetail, raw []byte, nets []dockerx.NetworkName, err error) {
	if err := requireSwarm(ctx, d, envID); err != nil {
		return detail, nil, nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	err = d.Clusters.ClusterRead(readCtx, envID, func(c *dockerx.Client) error {
		var e error
		if detail, raw, e = c.ServiceInspect(readCtx, serviceID); e != nil {
			return e
		}
		if withNetworks {
			nets, e = c.SwarmNetworkNames(readCtx)
		}
		return e
	})
	if err != nil {
		return detail, nil, nil, serviceReadProblem(d, envID, err)
	}
	return detail, raw, nets, nil
}

// getService answers the detail of one service. The spec leaves through MaskServiceSpec with
// redact.CanReveal, and the body is built from the masked copy only (D-39).
func getService(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, serviceID string) (ServiceDetailBody, error) {
	detail, _, nets, err := readService(ctx, d, envID, serviceID, true)
	if err != nil {
		return ServiceDetailBody{}, err
	}
	var spec swarm.ServiceSpec
	if err := json.Unmarshal(detail.Spec, &spec); err != nil {
		d.Log.Error("reading a service spec failed", "environment", envID, "service", serviceID, "error", err)
		return ServiceDetailBody{}, huma.Error500InternalServerError("internal error")
	}
	reveal := redact.CanReveal(ctx, d.Checker, p, envID)
	shown, _ := redact.MaskServiceSpec(spec, reveal)

	row, haveRow := serviceRow(ctx, d, envID, detail.ID)
	return serviceDetailBody(detail, shown, nets, row, haveRow, reveal), nil
}

// serviceRow finds the service in the last cluster read: its task counts and ports are not in
// an inspect. A read that fails, or a service that read does not hold yet, is no row.
func serviceRow(ctx context.Context, d ServicesDeps, envID, id string) (dockerx.ServiceRow, bool) {
	st, err := d.Clusters.State(ctx, envID)
	if err != nil && st.FetchedAt.IsZero() {
		return dockerx.ServiceRow{}, false
	}
	i := slices.IndexFunc(st.Read.Services, func(r dockerx.ServiceRow) bool { return r.ID == id })
	if i < 0 {
		return dockerx.ServiceRow{}, false
	}
	return st.Read.Services[i], true
}

// getServiceInspect answers the raw inspect document of one service, masked.
func getServiceInspect(ctx context.Context, d ServicesDeps, p *rbac.Principal, envID, serviceID string) (ServiceInspectBody, error) {
	_, raw, _, err := readService(ctx, d, envID, serviceID, false)
	if err != nil {
		return ServiceInspectBody{}, err
	}
	shown, masked, err := redact.MaskServiceInspect(raw, redact.CanReveal(ctx, d.Checker, p, envID))
	if err != nil {
		d.Log.Error("masking a service inspect failed", "environment", envID, "error", err)
		return ServiceInspectBody{}, huma.Error500InternalServerError("internal error")
	}
	doc, err := decodeInspect(shown)
	if err != nil {
		d.Log.Error("reading a service inspect failed", "environment", envID, "error", err)
		return ServiceInspectBody{}, huma.Error500InternalServerError("internal error")
	}
	return ServiceInspectBody{Masked: masked, Document: doc}, nil
}

// serviceDetailBody turns a service into the detail page's body. shown is the spec after
// MaskServiceSpec, the only form of it this function sees.
func serviceDetailBody(detail dockerx.ServiceDetail, shown swarm.ServiceSpec, nets []dockerx.NetworkName, row dockerx.ServiceRow, haveRow, reveal bool) ServiceDetailBody {
	b := ServiceDetailBody{
		ID: detail.ID, Name: detail.Name, Mode: detail.Mode, Stack: detail.Stack, Agent: detail.Agent,
		CreatedAt: rfc3339(detail.CreatedAt), UpdatedAt: rfc3339(detail.UpdatedAt),
		Image: serviceImageBody(detail.Image),
		Ports: []ServicePortBody{}, Mounts: []ServiceMountBody{}, Secrets: []ServiceFileRefBody{}, Configs: []ServiceFileRefBody{},
		Networks: []ServiceNetworkBody{}, Env: []redact.EnvVar{},
		Placement: ServicePlacementBody{Constraints: []string{}, Preferences: []string{}},
		Labels:    map[string]string{},
	}
	for k, v := range shown.Labels {
		b.Labels[k] = v
	}
	if haveRow {
		b.Running, b.Desired = row.Running, row.Desired
		for _, p := range row.Ports {
			b.Ports = append(b.Ports, ServicePortBody{Published: p.Published, Target: p.Target, Protocol: p.Protocol, Mode: p.Mode})
		}
	} else {
		b.Desired = detail.Replicas
		if shown.EndpointSpec != nil {
			for _, p := range shown.EndpointSpec.Ports {
				b.Ports = append(b.Ports, ServicePortBody{Published: p.PublishedPort, Target: p.TargetPort, Protocol: string(p.Protocol), Mode: string(p.PublishMode)})
			}
		}
	}
	if u := detail.UpdateStatus; u != nil {
		b.UpdateStatus, b.UpdateMessage = u.State, u.Message
	}
	b.HasPreviousSpec = detail.PreviousSpec != nil

	if cs := shown.TaskTemplate.ContainerSpec; cs != nil {
		for _, m := range cs.Mounts {
			b.Mounts = append(b.Mounts, ServiceMountBody{Type: string(m.Type), Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
		}
		for _, s := range cs.Secrets {
			if s == nil {
				continue
			}
			ref := ServiceFileRefBody{ID: s.SecretID, Name: s.SecretName}
			if f := s.File; f != nil {
				ref.File, ref.UID, ref.GID, ref.Mode = f.Name, f.UID, f.GID, fileMode(f.Mode)
			}
			b.Secrets = append(b.Secrets, ref)
		}
		for _, c := range cs.Configs {
			if c == nil {
				continue
			}
			ref := ServiceFileRefBody{ID: c.ConfigID, Name: c.ConfigName}
			if f := c.File; f != nil {
				ref.File, ref.UID, ref.GID, ref.Mode = f.Name, f.UID, f.GID, fileMode(f.Mode)
			}
			b.Configs = append(b.Configs, ref)
		}
		// shown is already masked: for a caller who may not reveal, each entry is NAME=<hidden>,
		// and Env keeps the name only. With reveal the values are the spec's.
		b.Env = redact.Env(cs.Env, reveal)
		b.EnvMasked = !reveal && len(b.Env) > 0
	}

	tt := shown.TaskTemplate
	for _, n := range tt.Networks {
		b.Networks = append(b.Networks, serviceNetworkBody(n, nets))
	}
	if r := tt.Resources; r != nil {
		if l := r.Limits; l != nil {
			b.Resources.CPULimit, b.Resources.MemoryLimit = cores(l.NanoCPUs), l.MemoryBytes
		}
		if rs := r.Reservations; rs != nil {
			b.Resources.CPUReservation, b.Resources.MemoryReservation = cores(rs.NanoCPUs), rs.MemoryBytes
		}
	}
	if pl := tt.Placement; pl != nil {
		b.Placement.Constraints = append(b.Placement.Constraints, pl.Constraints...)
		b.Placement.MaxReplicas = pl.MaxReplicas
		for _, pref := range pl.Preferences {
			if pref.Spread != nil {
				b.Placement.Preferences = append(b.Placement.Preferences, pref.Spread.SpreadDescriptor)
			}
		}
	}
	if rp := tt.RestartPolicy; rp != nil {
		out := &ServiceRestartPolicyBody{Condition: string(rp.Condition), Delay: durationText(rp.Delay), Window: durationText(rp.Window)}
		if rp.MaxAttempts != nil {
			out.MaxAttempts = *rp.MaxAttempts
		}
		b.RestartPolicy = out
	}
	if u := shown.UpdateConfig; u != nil {
		b.UpdatePolicy = &ServiceUpdatePolicyBody{
			Parallelism: u.Parallelism, Delay: durationText(&u.Delay), Order: string(u.Order),
			FailureAction: string(u.FailureAction), Monitor: durationText(&u.Monitor), MaxFailureRatio: float64(u.MaxFailureRatio),
		}
	}
	return b
}

// serviceImageBody splits the image a spec names into tag, digest and whether it is pinned.
func serviceImageBody(image string) ServiceImageBody {
	b := ServiceImageBody{Ref: image}
	if ref, err := registry.ParseRef(image); err == nil {
		b.Tag, b.Digest = ref.Tag, ref.Digest
		b.Pinned = ref.Digest != ""
	}
	return b
}

// serviceNetworkBody names a network attachment: its target is an ID or a name as the spec
// wrote it, and the swarm's networks say which.
func serviceNetworkBody(n swarm.NetworkAttachmentConfig, nets []dockerx.NetworkName) ServiceNetworkBody {
	out := ServiceNetworkBody{ID: n.Target, Name: n.Target, Aliases: append([]string{}, n.Aliases...)}
	for _, known := range nets {
		if known.ID == n.Target || known.Name == n.Target {
			out.ID, out.Name = known.ID, known.Name
			break
		}
	}
	return out
}

// cores turns Docker's nano CPUs into cores.
func cores(nano int64) float64 { return float64(nano) / 1e9 }

// durationText writes a duration as Go does (5s, 2m0s); nil and zero are empty, which is how
// an unset value reads in the spec.
func durationText(d *time.Duration) string {
	if d == nil || *d == 0 {
		return ""
	}
	return d.String()
}

// fileMode writes a file's permissions in octal with a leading zero (0444); no file mode is
// empty.
func fileMode(m os.FileMode) string {
	if m == 0 {
		return ""
	}
	return fmt.Sprintf("%04o", uint32(m.Perm()))
}
