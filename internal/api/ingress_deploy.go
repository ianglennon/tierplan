// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingressdeploy"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// The ingress deploy routes (Phase 24, DEP-01, DEP-07): an Admin previews what switching ingress on would create and what would
// stop it, switches it on as a job, previews and runs the switch-off the same way, and follows the job. Nothing here changes a stack: stacks are redeployed by their
// own deploy to join the ingress network.

const (
	problemIngressPreflightRefused = "ingress_preflight_refused"
	problemIngressJobNotFound      = "ingress_job_not_found"
	problemIngressRoutesLive       = ingressdeploy.CodeRoutesLive
	problemIngressPaused           = ingressdeploy.CodePaused

	// ingressJobKindPrefix is what every kind of ingress job in the jobs hub starts with (ingress_enable, ingress_disable and ingress_ensure).
	ingressJobKindPrefix = "ingress_"

	ingressJobGone = "This ingress job is no longer here. Jobs are kept for 10 minutes after they end."
)

// IngressStatusObjects says which of the ingress objects the swarm or the standalone host holds.
type IngressStatusObjects struct {
	Network   bool `json:"network" doc:"True when the ingress network exists"`
	Service   bool `json:"service" doc:"True when the proxy service exists; always false on a standalone host, whose proxy is a container"`
	Secret    bool `json:"secret" doc:"True when a join token secret exists; always false on a standalone host, whose join token is a file"`
	Container bool `json:"container" doc:"True when the proxy container exists on a standalone host; always false on a swarm"`
	Volume    bool `json:"volume" doc:"True when the certificate cache volume exists on a standalone host; always false on a swarm, whose volumes are on its nodes"`
}

// IngressStatusNodes counts the proxy nodes.
type IngressStatusNodes struct {
	Expected int `json:"expected" doc:"The swarm's nodes, each of which runs a proxy"`
	Enrolled int `json:"enrolled" doc:"The proxies that enrolled and were not revoked"`
	Ready    int `json:"ready" doc:"The enrolled proxies whose health is ok and whose applied bundle is the current one"`
}

// IngressStatusEnsure is the last pass of the loop that keeps the ingress objects as wanted.
type IngressStatusEnsure struct {
	At    string `json:"at" doc:"When the last pass ended, RFC 3339 UTC; empty when none has run"`
	Class string `json:"class" doc:"How it ended: empty for success, otherwise unreachable, locked, paused, write_failed, enable_failed (switching ingress on failed and ingress is still off), url_unset (TierPlan has no usable ingress address, so nothing was written), update_failed (the swarm rolled back the last update of the proxy service, which is not tried again for an hour or until the wanted spec changes) or not_enrolled (the standalone proxy has run for five minutes without enrolling: check that it can reach TierPlan's address)"`
}

// IngressStatusBody is an environment's ingress status. It carries counts and names, never a token or a secret.
type IngressStatusBody struct {
	Enabled             bool                  `json:"enabled" doc:"True while ingress is switched on"`
	Paused              bool                  `json:"paused" doc:"True after a restore, until an Admin resumes the environment: nothing is written to the swarm while it is"`
	AccessLogs          bool                  `json:"accessLogs" doc:"True while the proxy writes an access log"`
	ObjectsKnown        bool                  `json:"objectsKnown" doc:"False when the swarm could not be read, so objects says nothing"`
	Objects             IngressStatusObjects  `json:"objects" doc:"Which ingress objects the swarm holds"`
	Nodes               IngressStatusNodes    `json:"nodes" doc:"How many proxy nodes are expected, enrolled and ready"`
	LastEnsure          IngressStatusEnsure   `json:"lastEnsure" doc:"The last pass that keeps the ingress objects as wanted"`
	PendingWipes        int                   `json:"pendingWipes" doc:"Certificate cache volumes still to be wiped on nodes that could not be reached"`
	DisablingIncomplete bool                  `json:"disablingIncomplete" doc:"True when ingress is off but an ingress object remains in the swarm after a switch-off that did not finish. Switch ingress off again to remove it"`
	EnableFailed        bool                  `json:"enableFailed" doc:"True when ingress is off because switching it on failed, and the objects that attempt made remain in the swarm. Switch ingress on again to carry on, or off to remove them"`
	TokenDisabled       bool                  `json:"tokenDisabled" doc:"True when the join token is disabled, so no further node can enrol"`
	Redeploy            []IngressRedeployBody `json:"redeploy" doc:"The stacks that must be redeployed to be attached to the ingress network"`
}

// IngressStatusOutput wraps IngressStatusBody for huma.
type IngressStatusOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressStatusBody
}

// IngressAccessLogsRequest is the body of the access-log switch.
type IngressAccessLogsRequest struct {
	Enabled bool `json:"enabled" doc:"True to make the proxies write an access log, false to stop"`
}

// IngressAccessLogsInput is the path and the body of the access-log switch.
type IngressAccessLogsInput struct {
	EnvironmentPathInput
	Body IngressAccessLogsRequest
}

// IngressAccessLogsBody is the answer to the access-log switch.
type IngressAccessLogsBody struct {
	AccessLogs bool   `json:"accessLogs" doc:"The value now stored"`
	Applies    string `json:"applies" enum:"rolling,recreate,when_enabled,when_resumed" doc:"rolling: it applies as each node's proxy restarts. recreate: it applies when the next pass recreates the standalone host's proxy container. when_enabled: it is stored and applies when ingress is switched on. when_resumed: it is stored and applies once the environment is resumed"`
}

// IngressAccessLogsOutput wraps IngressAccessLogsBody for huma.
type IngressAccessLogsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressAccessLogsBody
}

// IngressResumeOutput is the answer to the resume of a restored environment.
type IngressResumeOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Resumed bool `json:"resumed" doc:"True when the environment was paused and is now resumed; false when it was not paused"`
	}
}

// IngressDeployDeps are the dependencies of the ingress deploy routes.
type IngressDeployDeps struct {
	// Service runs the preview and the enable job. Nil answers 503 ingress_unavailable, so the routes are always
	// registered.
	Service *ingressdeploy.Service
	// Jobs is the hub the enable job runs in, the same one the Service was built with. Nil answers 503 jobs_unavailable
	// to the job routes.
	Jobs *jobs.Hub
	// Checker decides whether a caller who did not start a job may still follow it.
	Checker *rbac.Checker
	// Heartbeat is how often an idle job stream gets a ping, MaxLifetime how long one stream may live and
	// MaxStreamsPerUser how many one user may have open. Zero is each one's default.
	Heartbeat, MaxLifetime time.Duration
	MaxStreamsPerUser      int
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// IngressEnableRequest is the optional body of the preview and the enable.
type IngressEnableRequest struct {
	Subnet string `json:"subnet,omitempty" maxLength:"43" doc:"The CIDR block of the ingress network, for example 10.0.8.0/22. Leave it out to let TierPlan choose the lowest free /22 of the swarm's address pools"`
}

// IngressEnableInput is the path and the optional body of the preview and the enable.
type IngressEnableInput struct {
	EnvironmentPathInput
	Body *IngressEnableRequest
}

// IngressFindingBody is one thing the preflight found.
type IngressFindingBody struct {
	Code     string   `json:"code" doc:"The stable name of the finding, for example ingress_no_manager"`
	Severity string   `json:"severity" enum:"error,warning" doc:"error refuses the enable; warning tells you something and lets it go on"`
	Message  string   `json:"message" doc:"The sentence to show"`
	Items    []string `json:"items" doc:"What the finding is about (a port, a stack, a hostname); empty for none"`
}

// IngressPlanBody is what an enable would create, or reuse when it is already there.
type IngressPlanBody struct {
	Network IngressPlanNetwork `json:"network"`
	Service IngressPlanService `json:"service"`
	Secret  IngressPlanSecret  `json:"secret"`
	Volume  IngressPlanVolume  `json:"volume"`
	// Standalone is true for a standalone host's plan.
	Standalone bool `json:"standalone" doc:"True when the plan is a standalone host's: the proxy is one container of the compose project tierplan-ingress, its network is a bridge, its join token is a file and its volume is the host's. False on a swarm"`
}

// IngressPlanNetwork is the ingress network of the plan.
type IngressPlanNetwork struct {
	Name   string `json:"name" doc:"The network's name"`
	Subnet string `json:"subnet" doc:"The block it has or would get; empty when no free block could be chosen yet"`
}

// IngressPlanService is the proxy service of the plan.
type IngressPlanService struct {
	Name  string `json:"name" doc:"The global proxy service's name"`
	Image string `json:"image" doc:"The image it runs, tagged with this server's version"`
}

// IngressPlanSecret is the swarm secret that carries the join token.
type IngressPlanSecret struct {
	Name string `json:"name" doc:"The secret's name; empty while the token is yet to be made, because the name carries the token's ID"`
	New  bool   `json:"new" doc:"True when the enable makes a new join token and secret"`
}

// IngressPlanVolume is the volume the proxy keeps its state in.
type IngressPlanVolume struct {
	Name string `json:"name" doc:"The volume's name"`
}

// IngressRedeployBody is a stack that holds routes and has to be redeployed to join the ingress network.
type IngressRedeployBody struct {
	StackID     string `json:"stackId" doc:"The stack's ID"`
	StackName   string `json:"stackName" doc:"The stack's name"`
	HoldsClaims bool   `json:"holdsClaims" doc:"True when the stack holds hostname claims"`
}

// IngressEnablePreviewBody is the answer to the preview.
type IngressEnablePreviewBody struct {
	Findings []IngressFindingBody  `json:"findings" doc:"Everything the preflight found, errors and warnings, in order"`
	Creates  IngressPlanBody       `json:"creates" doc:"What the enable would create, or reuse where it is there already"`
	Redeploy []IngressRedeployBody `json:"redeploy" doc:"The stacks that hold routes and must be redeployed to be attached to the ingress network; the enable does not change them"`
}

// IngressEnablePreviewOutput wraps IngressEnablePreviewBody for huma.
type IngressEnablePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressEnablePreviewBody
}

// IngressEnableStartBody is the answer to the enable: the job that does it.
type IngressEnableStartBody struct {
	JobID string `json:"jobId" doc:"The enable job: read it at GET /api/env/{envId}/ingress/jobs/{jobId} or follow its stream"`
}

// IngressEnableStartOutput wraps IngressEnableStartBody for huma.
type IngressEnableStartOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressEnableStartBody
}

// IngressDisableNodeBody is a node whose certificate cache volume a switch-off wipes.
type IngressDisableNodeBody struct {
	NodeID string `json:"nodeId" doc:"The ID the node's agent connects as"`
	Name   string `json:"name" doc:"The node's name"`
}

// IngressDisableRemovesBody is what a switch-off would remove.
type IngressDisableRemovesBody struct {
	Service   string                   `json:"service" doc:"The proxy service's name; empty when the swarm was read and does not hold it"`
	Network   string                   `json:"network" doc:"The ingress network's name; empty when the swarm or the host was read and does not hold it"`
	Container string                   `json:"container" doc:"The proxy container's name on a standalone host; empty when the host does not hold it and on a swarm"`
	Secrets   []string                 `json:"secrets" doc:"The names of the join token secrets the swarm holds"`
	Volumes   []IngressDisableNodeBody `json:"volumes" doc:"The nodes whose certificate cache volume is wiped: every node of a swarm, the one host of a standalone environment"`
}

// IngressDisablePreviewBody is the answer to the preview of a switch-off.
type IngressDisablePreviewBody struct {
	Findings    []IngressFindingBody      `json:"findings" doc:"What stands in the way, each an error: ingress_paused first, then ingress_routes_live listing every hostname and path with its stack. Empty when nothing does"`
	Removes     IngressDisableRemovesBody `json:"removes" doc:"What the switch-off would remove"`
	Unreachable []string                  `json:"unreachable" doc:"The nodes whose agent is not connected now: their certificate cache volume is wiped when the agent connects"`
}

// IngressDisablePreviewOutput wraps IngressDisablePreviewBody for huma.
type IngressDisablePreviewOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressDisablePreviewBody
}

// IngressDisableStartBody is the answer to the switch-off: the job that does it.
type IngressDisableStartBody struct {
	JobID string `json:"jobId" doc:"The disable job: read it at GET /api/env/{envId}/ingress/jobs/{jobId} or follow its stream"`
}

// IngressDisableStartOutput wraps IngressDisableStartBody for huma.
type IngressDisableStartOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressDisableStartBody
}

// IngressJobPathInput carries {envId} and {jobId}.
type IngressJobPathInput struct {
	EnvironmentPathInput
	JobID string `path:"jobId" doc:"The job ID the enable answered with"`
}

// IngressJobBody is an ingress job.
type IngressJobBody struct {
	ID         string               `json:"id" doc:"The job ID"`
	Kind       string               `json:"kind" enum:"ingress_enable,ingress_disable,ingress_ensure" doc:"What the job does: ingress_enable, ingress_disable, or ingress_ensure (the pass that keeps a standalone host's proxy project as wanted, run by the server and not by a person)"`
	StartedBy  string               `json:"startedBy" doc:"Who started it, as the hub records its owner"`
	State      string               `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every step has ended, then done when every step is"`
	CreatedAt  string               `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string               `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Items      []StackJobItemBody   `json:"items" doc:"The steps, in the order they run: token, network, service and switch for ingress_enable on a swarm; switch_off, service, volumes, network, secret, token and nodes for ingress_disable on a swarm; on a standalone host the steps of the proxy project's bring-up come between the token and the switch for ingress_enable and for ingress_ensure, and switch_off, proxy, network, volumes, token and nodes make ingress_disable"`
	Output     []StackJobOutputLine `json:"output" doc:"The output log, oldest first"`
}

// IngressJobOutput wraps IngressJobBody for huma.
type IngressJobOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         IngressJobBody
}

func ingressJobBody(j jobs.Job, output []jobs.Output) IngressJobBody {
	b := IngressJobBody{
		ID: j.ID, Kind: j.Kind, StartedBy: j.Owner, State: j.State, CreatedAt: rfc3339(j.CreatedAt), FinishedAt: rfc3339(j.FinishedAt),
		Items: make([]StackJobItemBody, 0, len(j.Items)), Output: make([]StackJobOutputLine, 0, len(output)),
	}
	for _, it := range j.Items {
		b.Items = append(b.Items, stackJobItemBody(it))
	}
	for _, o := range output {
		b.Output = append(b.Output, stackJobOutputLine(o))
	}
	return b
}

// ingressJobVisible reports whether the caller may follow the job: it must be an ingress job of this environment, and the
// caller must have started it or hold ingress.manage on the environment. Anything else is the same 404 as a job that is
// gone.
func (d IngressDeployDeps) ingressJobVisible(ctx context.Context, p *rbac.Principal, envID string, job jobs.Job) bool {
	if !strings.HasPrefix(job.Kind, ingressJobKindPrefix) || job.EnvID != envID {
		return false
	}
	if job.Owner == stackJobOwner(p) {
		return true
	}
	if d.Checker == nil {
		return false
	}
	dec := d.Checker.Check(ctx, p, rbac.Requirement{Perm: rbac.PermIngressManage, Scope: rbac.ScopeEnvironment}, envID)
	return dec.Err == nil && dec.Allowed
}

func ingressFindings(fs []ingressdeploy.Finding) []IngressFindingBody {
	out := make([]IngressFindingBody, 0, len(fs))
	for _, f := range fs {
		items := f.Items
		if items == nil {
			items = []string{}
		}
		out = append(out, IngressFindingBody{Code: f.Code, Severity: f.Severity, Message: f.Message, Items: items})
	}
	return out
}

// RegisterIngressDeploy registers the preview and the enable of an environment's ingress, and the read and the stream of
// the enable job. All of them need ingress.manage on the environment (its Admin). The routes are always registered;
// without a Service they answer 503 ingress_unavailable.
func RegisterIngressDeploy(r *httpx.Router, d IngressDeployDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	unavailable := func() error {
		return httpx.Problem(http.StatusServiceUnavailable, problemIngressUnavailable, "Ingress is not available on this server.")
	}
	request := func(b *IngressEnableRequest) ingressdeploy.EnableRequest {
		if b == nil {
			return ingressdeploy.EnableRequest{}
		}
		return ingressdeploy.EnableRequest{Subnet: strings.TrimSpace(b.Subnet)}
	}
	fail := func(ctx context.Context, what string, err error) error {
		var pre *ingressdeploy.PreflightError
		var refused *ingressdeploy.DisableRefusedError
		var locked *ingressdeploy.LockedError
		switch {
		case errors.As(err, &pre):
			return ingressPreflightProblem(pre)
		case errors.As(err, &refused):
			return ingressDisableProblem(refused)
		case errors.As(err, &locked):
			name := ""
			if env := httpx.EnvFrom(ctx); env != nil {
				name = env.Name
			}
			return operationLocked(name, locked.Holder)
		case errors.Is(err, envs.ErrNotFound):
			return httpx.Problem(http.StatusNotFound, problemEnvironmentNotFound, "environment not found")
		}
		d.Log.Error("ingress deploy request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressEnable",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/enable/preview",
		Summary:     "Preview switching ingress on for a swarm or standalone environment",
		Description: "Says what switching ingress on would create (the ingress network and its subnet, the global proxy service and its image, the swarm secret " +
			"that carries the join token, the proxy's volume) and what would stop it, and writes nothing. Findings of severity error refuse the enable; warnings " +
			"do not. redeploy lists the stacks that hold routes and must be redeployed to join the ingress network: switching ingress on never changes a stack. " +
			"On a standalone host plan.standalone is true: the proxy is one container of the compose project tierplan-ingress with a bridge network and a cache volume, and the host is read only when its agent is connected. " +
			"A standalone host whose agent is not connected is a finding, ingress_host_unreachable. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressEnableInput) (*IngressEnablePreviewOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		res, err := d.Service.Preview(ctx, in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "preview ingress", err)
		}
		body := IngressEnablePreviewBody{Findings: ingressFindings(res.Findings), Redeploy: make([]IngressRedeployBody, 0, len(res.Redeploy))}
		body.Creates.Network = IngressPlanNetwork{Name: res.Plan.Network.Name, Subnet: res.Plan.Network.Subnet}
		body.Creates.Service = IngressPlanService{Name: res.Plan.Service.Name, Image: res.Plan.Service.Image}
		body.Creates.Secret = IngressPlanSecret{Name: res.Plan.Secret.Name, New: res.Plan.Secret.New}
		body.Creates.Volume = IngressPlanVolume{Name: res.Plan.Volume.Name}
		body.Creates.Standalone = res.Plan.Standalone
		for _, st := range res.Redeploy {
			body.Redeploy = append(body.Redeploy, IngressRedeployBody{StackID: st.ID, StackName: st.Name, HoldsClaims: st.HoldsClaims})
		}
		return &IngressEnablePreviewOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "enableIngress",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/enable",
		Summary:       "Switch ingress on for a swarm or standalone environment",
		DefaultStatus: http.StatusAccepted,
		Description: "Runs the preflight again, then, as a job answered with 202 and its ID at once, makes the join token's swarm secret, the ingress network " +
			"and the global proxy service and switches ingress on. Existing stacks are not changed: a stack must be redeployed to be attached to the ingress network. " +
			"Follow the job at GET /api/env/{envId}/ingress/jobs/{jobId} or its stream; enrolment and readiness of the proxy nodes are read through the ingress status and the node list. " +
			"On a standalone host the job makes the join token, brings up the compose project tierplan-ingress (one proxy container, a bridge network and the cache volume) through the host's one node and switches ingress on. " +
			"409 " + problemIngressPreflightRefused + " when the preflight refuses, with each finding's code and items in errors (ingress_host_unreachable on a standalone host whose agent is not connected); " +
			"409 " + problemOperationLocked + " while another operation holds the environment, naming it. Nothing is written when it is refused. Needs ingress.manage on the environment (its Admin). " +
			"Audited as ingress.enabled when the job switches it on.",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressEnableInput) (*IngressEnableStartOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		id, err := d.Service.Enable(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, request(in.Body))
		if err != nil {
			return nil, fail(ctx, "enable ingress", err)
		}
		return &IngressEnableStartOutput{CacheControl: "no-store", Body: IngressEnableStartBody{JobID: id}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "previewIngressDisable",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/disable/preview",
		Summary:     "Preview switching ingress off for a swarm or standalone environment",
		Description: "Says what switching ingress off would remove (the proxy service, the ingress network, the join token secrets and the certificate cache volume on each node) " +
			"and what would stop it, and writes nothing. A finding is an error that refuses the switch-off: ingress_paused while a restore's pause stands, and ingress_routes_live " +
			"while any claim with routes exists in the environment, a stopped stack's included, which lists each hostname and path with its stack ordered by hostname then path. " +
			"A stack in an environment the caller cannot see is not named. Remove the labels and redeploy, or remove the stack, to clear it. unreachable lists the nodes whose agent is not connected: " +
			"their cache volume, which holds the private keys, is wiped when the agent connects. " +
			"On a standalone host removes.container names the proxy container, and a host whose agent is not connected is the finding ingress_host_unreachable. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressDisablePreviewOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		canView, err := ingressCanView(ctx, d.Checker, p, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "read the access for the ingress switch-off preview", err)
		}
		res, err := d.Service.PreviewDisable(ctx, in.EnvID, canView)
		if err != nil {
			return nil, fail(ctx, "preview the ingress switch-off", err)
		}
		body := IngressDisablePreviewBody{
			Findings: ingressFindings(res.Findings), Unreachable: res.Unreachable,
			Removes: IngressDisableRemovesBody{
				Service: res.Removes.Service, Network: res.Removes.Network, Container: res.Removes.Container, Secrets: res.Removes.Secrets,
				Volumes: make([]IngressDisableNodeBody, 0, len(res.Removes.Volumes)),
			},
		}
		for _, n := range res.Removes.Volumes {
			body.Removes.Volumes = append(body.Removes.Volumes, IngressDisableNodeBody{NodeID: n.ID, Name: n.Name})
		}
		return &IngressDisablePreviewOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID:   "disableIngress",
		Method:        http.MethodPost,
		Path:          "/api/env/{envId}/ingress/disable",
		Summary:       "Switch ingress off for a swarm or standalone environment",
		DefaultStatus: http.StatusAccepted,
		Description: "Refused while routes are live; otherwise answers 202 with a job's ID at once. The job runs in this order: stop serving routes (ingress is switched off and the proxies' sessions are closed), " +
			"remove the proxy service and wait for its tasks to be gone, wipe the certificate cache volume on every node through that node's agent, remove the ingress network, remove the join token secrets, " +
			"disable the join token (its hash and audit stay) and remove the proxy node rows. A node whose agent is not connected, or whose volume could not be removed, is named in the volumes step's result and is wiped " +
			"when its agent reconnects. Claims and certificates are not touched. Running it again after a failure finishes what is left. " +
			"409 " + problemIngressRoutesLive + " while a claim with routes exists, listing each hostname and path with its stack in errors; 409 " + problemIngressPaused + " while a restore's pause stands; " +
			"On a standalone host the job removes the proxy container, the ingress network and the cache volume through the host's one node; a network another container still uses fails the job, naming it. " +
			"409 ingress_host_unreachable on a standalone host whose agent is not connected; 409 " + problemOperationLocked + " while another operation holds the environment. Nothing is written when it is refused. " +
			"Audited as ingress.disabled, ingress.join_token_disabled, ingress.node_removed, ingress.wipe_queued and ingress.wipe_done. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressDisableStartOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		canView, err := ingressCanView(ctx, d.Checker, p, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "read the access for the ingress switch-off", err)
		}
		id, err := d.Service.Disable(ctx, p.AuditActor(), stackJobOwner(p), in.EnvID, canView)
		if err != nil {
			return nil, fail(ctx, "switch ingress off", err)
		}
		return &IngressDisableStartOutput{CacheControl: "no-store", Body: IngressDisableStartBody{JobID: id}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressJob",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/jobs/{jobId}",
		Summary:     "One ingress job and its steps",
		Description: "The job as it stands now. A job is kept for 10 minutes after it ends. A job of another environment, one that is not an ingress job, one you " +
			"did not start and may not follow, or one that is gone is 404 " + problemIngressJobNotFound + ". Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressJobPathInput) (*IngressJobOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Jobs == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Ingress jobs are not available right now.")
		}
		job, ok := d.Jobs.Get(in.JobID)
		if !ok || !d.ingressJobVisible(ctx, p, in.EnvID, job) {
			return nil, httpx.Problem(http.StatusNotFound, problemIngressJobNotFound, ingressJobGone)
		}
		return &IngressJobOutput{CacheControl: "no-store", Body: ingressJobBody(job, outputOf(d.Jobs, job.ID))}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getIngressStatus",
		Method:      http.MethodGet,
		Path:        "/api/env/{envId}/ingress/status",
		Summary:     "Read the ingress status of a swarm or standalone environment",
		Description: "Says whether ingress is on, paused after a restore, and logging access; which of the ingress network, proxy service and join token secret the swarm holds, or on a standalone host which of the network, proxy container and cache volume it holds; " +
			"how many proxy nodes are expected, enrolled and ready (ready means healthy on the current bundle); when the objects were last checked and how that ended; " +
			"how many certificate cache wipes are pending; whether switching off left an object behind; whether the join token is disabled; and the stacks to redeploy. " +
			"It carries no token and no secret. Answered with Cache-Control no-store. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressStatusOutput, error) {
		if httpx.PrincipalFrom(ctx) == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		st, err := d.Service.Status(ctx, in.EnvID)
		if err != nil {
			return nil, fail(ctx, "read the ingress status", err)
		}
		body := IngressStatusBody{
			Enabled: st.Enabled, Paused: st.Paused, AccessLogs: st.AccessLogs, ObjectsKnown: st.ObjectsKnown,
			Objects: IngressStatusObjects{
				Network: st.Objects.Network, Service: st.Objects.Service, Secret: st.Objects.Secret,
				Container: st.Objects.Container, Volume: st.Objects.Volume,
			},
			Nodes:        IngressStatusNodes{Expected: st.Nodes.Expected, Enrolled: st.Nodes.Enrolled, Ready: st.Nodes.Ready},
			LastEnsure:   IngressStatusEnsure{At: rfc3339(st.LastEnsure.At), Class: st.LastEnsure.Class},
			PendingWipes: st.PendingWipes, DisablingIncomplete: st.DisablingIncomplete, EnableFailed: st.EnableFailed, TokenDisabled: st.TokenDisabled,
			Redeploy: make([]IngressRedeployBody, 0, len(st.Redeploy)),
		}
		for _, r := range st.Redeploy {
			body.Redeploy = append(body.Redeploy, IngressRedeployBody{StackID: r.ID, StackName: r.Name, HoldsClaims: r.HoldsClaims})
		}
		return &IngressStatusOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "setIngressAccessLogs",
		Method:      http.MethodPut,
		Path:        "/api/env/{envId}/ingress/access-logs",
		Summary:     "Switch the proxy access log of a swarm or standalone environment",
		Description: "Stores whether the proxies write an access log; it is off by default. Switching it restarts each node's proxy in turn, as the proxy service is rolled with " +
			"the setting changed; on a standalone host the next pass recreates the proxy container. The log carries the client address and the request path of every request, but never a request or response header (so no cookie or authorization value). " +
			"The answer says when it applies: rolling, recreate (a standalone host), or stored for when ingress is switched on or the environment is resumed. Asking for the value already stored changes nothing and is not audited. " +
			"Audited as ingress.access_logs_changed. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *IngressAccessLogsInput) (*IngressAccessLogsOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		res, err := d.Service.SetAccessLogs(ctx, p.AuditActor(), in.EnvID, in.Body.Enabled)
		if err != nil {
			return nil, fail(ctx, "switch the ingress access log", err)
		}
		return &IngressAccessLogsOutput{CacheControl: "no-store", Body: IngressAccessLogsBody{AccessLogs: res.AccessLogs, Applies: res.Applies}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "resumeIngress",
		Method:      http.MethodPost,
		Path:        "/api/env/{envId}/ingress/resume",
		Summary:     "Resume the ingress of an environment a restore paused",
		Description: "After a restore from a backup, TierPlan only observes each environment's ingress and writes nothing to its swarm until an Admin resumes it here. " +
			"Resuming clears the marker and the next pass writes the ingress objects again. Resuming an environment that is not paused answers 200 with resumed false and is not audited. " +
			"Audited as ingress.deploy_resumed. Needs ingress.manage on the environment (its Admin).",
		Tags: []string{"ingress"},
	}, rbac.Require(rbac.PermIngressManage), func(ctx context.Context, in *EnvironmentPathInput) (*IngressResumeOutput, error) {
		p := httpx.PrincipalFrom(ctx)
		if p == nil {
			return nil, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
		}
		if d.Service == nil {
			return nil, unavailable()
		}
		changed, err := d.Service.Resume(ctx, p.AuditActor(), in.EnvID)
		if err != nil {
			return nil, fail(ctx, "resume ingress", err)
		}
		out := &IngressResumeOutput{CacheControl: "no-store"}
		out.Body.Resumed = changed
		return out, nil
	})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/env/{envId}/ingress/jobs/{jobId}/stream",
		rbac.Requirement{Perm: rbac.PermIngressManage, Scope: rbac.ScopeEnvironment},
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			serveIngressJobStream(w, req, d, lim)
		}))
}

// serveIngressJobStream follows one ingress job through the shared job stream: the job as it stands, an item frame for each
// change and a done frame at the end, resumed from Last-Event-ID. The job does not depend on the stream.
func serveIngressJobStream(w http.ResponseWriter, req *http.Request, d IngressDeployDeps, lim *streamLimiter) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Jobs == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemJobsUnavail, "Ingress jobs are not available right now.")
		return
	}
	jobID := chi.URLParam(req, "jobId")
	if job, ok := d.Jobs.Get(jobID); !ok || !d.ingressJobVisible(ctx, p, env.ID, job) {
		writeJSONProblem(w, http.StatusNotFound, problemIngressJobNotFound, ingressJobGone)
		return
	}
	serveJobStream(w, req, jobStream{
		Hub: d.Jobs, JobID: jobID, Key: string(p.Type) + ":" + p.ID, Limiter: lim, What: "ingress job",
		GoneType: problemIngressJobNotFound, GoneDetail: ingressJobGone, TooManyType: problemTooManyStreams,
		Heartbeat: d.Heartbeat, MaxLifetime: d.MaxLifetime, Log: d.Log,
		Snapshot: func(j jobs.Job) any { return ingressJobBody(j, outputOf(d.Jobs, j.ID)) },
		Encode: func(ev jobs.Event) (any, bool) {
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				return stackJobItemBody(*ev.Item), true
			case ev.Type == jobs.EventOutput && ev.Output != nil:
				return stackJobOutputLine(*ev.Output), true
			case ev.Type == jobs.EventDone && ev.Job != nil:
				return ingressJobBody(*ev.Job, outputOf(d.Jobs, ev.Job.ID)), true
			}
			return nil, false
		},
	})
}

// ingressDisableProblem is the 409 for a switch-off that was refused: its type is the code of the first finding
// (ingress_paused or ingress_routes_live), and each finding is listed with its code as the location, its sentence as the
// message and its items, the routes and their stacks, as the value. Each sentence already says that ingress cannot be
// switched off, so the detail is the sentences alone.
func ingressDisableProblem(e *ingressdeploy.DisableRefusedError) error {
	var details []*huma.ErrorDetail
	var msgs []string
	for _, f := range e.Findings {
		items := f.Items
		if items == nil {
			items = []string{}
		}
		details = append(details, &huma.ErrorDetail{Location: f.Code, Message: f.Message, Value: items})
		msgs = append(msgs, f.Message)
	}
	typ := ""
	if len(e.Findings) > 0 {
		typ = e.Findings[0].Code
	}
	return &huma.ErrorModel{
		Type: typ, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: strings.Join(msgs, " "), Errors: details,
	}
}

// ingressPreflightProblem is the 409 for an enable the preflight refused: each finding that refuses is listed with its code
// as the location, its sentence as the message and its items as the value.
func ingressPreflightProblem(e *ingressdeploy.PreflightError) error {
	var details []*huma.ErrorDetail
	var msgs []string
	for _, f := range e.Findings {
		if f.Severity != ingressdeploy.SeverityError {
			continue
		}
		items := f.Items
		if items == nil {
			items = []string{}
		}
		details = append(details, &huma.ErrorDetail{Location: f.Code, Message: f.Message, Value: items})
		msgs = append(msgs, f.Message)
	}
	return &huma.ErrorModel{
		Type: problemIngressPreflightRefused, Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
		Detail: "Ingress cannot be switched on: " + strings.Join(msgs, " "), Errors: details,
	}
}
