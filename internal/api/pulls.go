// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/errgroup"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/jobs"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

const (
	// pullNodeTimeout is how long one node may take to pull. A pull of a large image over
	// a slow link takes many minutes, so the 30 second write timeout of a container action
	// does not apply (D-26).
	pullNodeTimeout = 30 * time.Minute

	pullJobKind    = "pull"
	imageTargetTyp = "image"
	// credentialAnonymous is what a pull records as its credential until one is chosen.
	credentialAnonymous = "anonymous"

	problemRefInvalid     = "ref_invalid"
	problemNodesUnknown   = "nodes_unknown"
	problemNodesRequired  = "nodes_required"
	problemTooManyPulls   = "too_many_pulls"
	problemPullNotFound   = "pull_not_found"
	problemJobsUnavail    = "jobs_unavailable"
	problemTooManyStreams = "too_many_streams"

	// maxPullNodes bounds the nodes one pull may name.
	maxPullNodes = 128

	// maxPullLayers bounds the layers one item lists; an image has a few dozen.
	maxPullLayers = 256
	// maxPullText bounds a layer ID or status taken from the engine's progress.
	maxPullText = 80
)

// pullMessages are the sentences an item carries for a failure code. They are built here
// from the code, never from the engine's or the registry's answer, so nothing a remote
// system said reaches a browser through a job (T-05-02, T-05-06). The SPA words a code
// itself and uses these only as a fallback.
var pullMessages = map[string]string{
	dockerx.PullCodeDenied:              "The registry refused the pull.",
	dockerx.PullCodeUnauthorized:        "The registry asked for a sign-in the pull did not have.",
	dockerx.PullCodeNotFound:            "The registry has no such image.",
	dockerx.PullCodeInvalidReference:    "The engine did not accept this image reference.",
	dockerx.PullCodeRateLimited:         "The registry is limiting pulls. Try again later.",
	dockerx.PullCodeRegistryUnreachable: "The registry could not be reached from this node.",
	dockerx.PullCodeRegistryTLS:         "The registry's certificate was not accepted by this node.",
	dockerx.PullCodeRegistryHTTP:        "The registry does not speak HTTPS and this node requires it.",
	dockerx.PullCodeCancelled:           "The pull was cancelled.",
	dockerx.PullCodeFailed:              "The pull failed.",
	codeNodeUnreachable:                 "The node is not connected to Bayplan.",
	codeHostReadOnly:                    "This host is read-only, so Bayplan cannot pull on it.",
	codeAgentPolicyDeny:                 "The node's agent refused the request.",
}

// PullsDeps are the dependencies of the pull routes.
type PullsDeps struct {
	// Aggregator reaches a node's engine: every pull is a write, through WriteOnNode.
	Aggregator *envs.Aggregator
	// Jobs holds the pulls in progress. Nil answers 503 jobs_unavailable, so the routes
	// are always registered.
	Jobs  *jobs.Hub
	Audit *audit.Writer
	// Checker decides whether a caller is an instance admin, who may see every pull.
	Checker *rbac.Checker
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
	// Heartbeat is how often an idle stream gets a ping. Zero means 15 seconds.
	Heartbeat time.Duration
	// MaxLifetime is the longest one stream stays open. Zero means one hour.
	MaxLifetime time.Duration
	// MaxStreamsPerUser caps concurrent pull streams per user. Zero means 4.
	MaxStreamsPerUser int
	// NodeTimeout is how long one node may pull. Zero means pullNodeTimeout.
	NodeTimeout time.Duration
}

// PullStartRequest is the body of POST /api/env/{envId}/images/pull.
type PullStartRequest struct {
	Ref     string   `json:"ref" minLength:"1" maxLength:"512" doc:"The image to pull, like nginx:1.27 or ghcr.io/acme/app:latest. No tag means latest; a bare name is a Docker Hub library image"`
	NodeIDs []string `json:"nodeIds,omitempty" maxItems:"128" doc:"The nodes to pull on, by the nodeId that pull-targets lists. Leave it out to pull on every node. An empty list is 422 nodes_required and an ID that is not a node of this environment is 422 nodes_unknown"`
}

// PullStartInput is the path and body of the start route.
type PullStartInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      PullStartRequest
}

// PullNodeBody names a node a pull runs on.
type PullNodeBody struct {
	NodeID string `json:"nodeId" doc:"The node's ID: the nodeId of its rows in the lists"`
	Name   string `json:"name" doc:"The node's name"`
}

// PullCredentialBody is the credential a pull used.
type PullCredentialBody struct {
	ID    string `json:"id" doc:"The credential's ID"`
	Name  string `json:"name" doc:"The credential's name"`
	Layer string `json:"layer" enum:"instance,environment" doc:"Where the credential is stored"`
}

// PullStartBody is the answer to a start.
type PullStartBody struct {
	JobID      string                       `json:"jobId" doc:"The ID of the pull job. GET /api/pulls/{jobId} reads it and GET /api/pulls/{jobId}/stream follows it"`
	Ref        string                       `json:"ref" doc:"The normalised reference being pulled: nginx:1.27 is docker.io/library/nginx:1.27"`
	Credential Nullable[PullCredentialBody] `json:"credential" doc:"The credential the pull uses, or null for an anonymous pull"`
	Nodes      []PullNodeBody               `json:"nodes" doc:"The nodes the pull runs on, one item each"`
}

// PullStartOutput wraps PullStartBody for huma.
type PullStartOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         PullStartBody
}

// PullLayerBody is one layer of an image on one node.
type PullLayerBody struct {
	ID      string `json:"id" doc:"The layer's short ID"`
	Status  string `json:"status" doc:"What the engine last said about the layer: Pulling fs layer, Downloading, Download complete, Extracting, Pull complete or Already exists"`
	Current int64  `json:"current" doc:"Bytes downloaded so far, 0 when the engine has not counted"`
	Total   int64  `json:"total" doc:"Bytes to download, 0 when not known"`
}

// PullItemBody is the progress of a pull on one node.
type PullItemBody struct {
	NodeID   string          `json:"nodeId" doc:"The node's ID"`
	Name     string          `json:"name" doc:"The node's name"`
	State    string          `json:"state" enum:"waiting,running,done,failed,cancelled" doc:"Where the pull on this node is"`
	Code     string          `json:"code" doc:"A stable word for how a failed pull failed, empty otherwise"`
	Message  string          `json:"message" doc:"A sentence for the code. The SPA words the code itself"`
	Current  int64           `json:"current" doc:"Bytes downloaded so far across the layers counted"`
	Total    int64           `json:"total" doc:"Bytes to download across the layers counted, 0 until known"`
	Unit     string          `json:"unit" doc:"What current and total count: bytes, or empty"`
	Done     int             `json:"done" doc:"Layers finished"`
	Count    int             `json:"count" doc:"Layers seen so far"`
	Layers   []PullLayerBody `json:"layers" doc:"The layers seen, in the order the engine named them"`
	Digest   string          `json:"digest" doc:"The digest the reference resolved to, once the pull is done"`
	UpToDate bool            `json:"upToDate" doc:"True when the node already had this image"`
}

// PullJobBody is a pull job.
type PullJobBody struct {
	ID         string                       `json:"id" doc:"The job ID"`
	EnvID      string                       `json:"envId" doc:"The environment the pull runs on"`
	Ref        string                       `json:"ref" doc:"The normalised reference"`
	Credential Nullable[PullCredentialBody] `json:"credential" doc:"The credential the pull uses, or null for an anonymous pull"`
	State      string                       `json:"state" enum:"running,done,partial,failed,cancelled" doc:"running until every node has ended, then done when every node is, partial when some are, failed when none is and one failed, and cancelled otherwise"`
	CreatedAt  string                       `json:"createdAt" doc:"When the job started, RFC 3339 UTC"`
	FinishedAt string                       `json:"finishedAt" doc:"When the job ended, RFC 3339 UTC, empty while it runs"`
	Items      []PullItemBody               `json:"items" doc:"One entry per node, in the order the nodes were named"`
}

// PullTargetBody is a node a pull can run on.
type PullTargetBody struct {
	NodeID    string `json:"nodeId" doc:"The node's ID: what nodeIds in a pull names, and the nodeId of its rows in the lists"`
	Name      string `json:"name" doc:"The node's name"`
	Role      string `json:"role" doc:"manager or worker on a swarm, standalone on a single host"`
	Arch      string `json:"arch" doc:"The node's CPU architecture by the registry's name: amd64, arm64, arm/v7. Empty when the node has not said"`
	ReadOnly  bool   `json:"readOnly" doc:"True when the host's owner made it read-only: a pull there fails with host_read_only"`
	Reachable bool   `json:"reachable" doc:"False when the node's agent is not connected to Bayplan now"`
}

// PullTargetsBody is the answer to the targets route.
type PullTargetsBody struct {
	Swarm bool             `json:"swarm" doc:"True when the environment is a swarm and a pull may name nodes"`
	Nodes []PullTargetBody `json:"nodes" doc:"Every node, by name"`
}

// PullTargetsOutput wraps PullTargetsBody for huma.
type PullTargetsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         PullTargetsBody
}

// PullJobInput is the path of the read route.
type PullJobInput struct {
	JobID string `path:"jobId" doc:"The pull job ID"`
}

// PullJobOutput wraps PullJobBody for huma.
type PullJobOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         PullJobBody
}

// RegisterPulls registers the pull routes: start a pull, read a job, and follow it. A
// pull is a job on the server (D-26): the request answers as soon as the job is started
// and the pull goes on without it.
func RegisterPulls(r *httpx.Router, d PullsDeps) {
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
	if d.NodeTimeout <= 0 {
		d.NodeTimeout = pullNodeTimeout
	}

	httpx.Register(r, huma.Operation{
		OperationID: "startPull", Method: http.MethodPost, Path: "/api/env/{envId}/images/pull",
		Summary: "Pull an image on the nodes of an environment",
		Description: "Starts a pull of the image on every node of the environment, or on the nodes named in nodeIds, and answers 202 with the job ID at once; the pull goes on if the browser closes. " +
			"A node that is not connected to Bayplan gets an item of its own that fails with node_unreachable, and a node whose host is read-only fails with host_read_only without a call to its engine: neither stops the others. " +
			"The reference is normalised first: nginx:1.27 becomes docker.io/library/nginx:1.27 and no tag means latest. " +
			"An unparsable reference is 422 ref_invalid and starts nothing. At most 5 pulls of one user run at once (429 too_many_pulls). " +
			"Each node pulls once, is never retried, and writes one image.pulled audit row. Needs images.pull (Operator) on the environment.",
		Tags: []string{"images"}, DefaultStatus: http.StatusAccepted,
	}, rbac.Requirement{Perm: rbac.PermImagesPull, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *PullStartInput) (*PullStartOutput, error) {
			return startPull(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "listPullTargets", Method: http.MethodGet, Path: "/api/env/{envId}/images/pull-targets",
		Summary: "The nodes a pull can run on",
		Description: "Every node of the environment by name, with its role, its architecture by the registry's names (amd64, arm64), whether its host is read-only and whether it is connected to Bayplan now. " +
			"A node whose agent is not connected is listed with reachable false. swarm is true when the environment is a swarm. Needs images.read.",
		Tags: []string{"images"},
	}, rbac.Requirement{Perm: rbac.PermImagesRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *EnvironmentPathInput) (*PullTargetsOutput, error) {
			return listPullTargets(ctx, d, in)
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getPull", Method: http.MethodGet, Path: "/api/pulls/{jobId}",
		Summary: "One pull job and its progress on every node",
		Description: "The pull as it stands now, for a browser that returns to a pull it started. A pull is kept for 10 minutes after it ends. " +
			"Only the user who started it, or an instance admin, can read it; anyone else gets 404 pull_not_found.",
		Tags: []string{"images"},
	}, rbac.SelfService,
		func(ctx context.Context, in *PullJobInput) (*PullJobOutput, error) {
			p := httpx.PrincipalFrom(ctx)
			if p == nil || d.Jobs == nil {
				return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Pulls are not available right now.")
			}
			job, ok := d.Jobs.Get(in.JobID)
			if !ok || !d.pullVisible(ctx, p, job) {
				return nil, httpx.Problem(http.StatusNotFound, problemPullNotFound, "This pull is no longer here. Pulls are kept for 10 minutes after they end.")
			}
			return &PullJobOutput{CacheControl: "no-store", Body: pullJobBody(job)}, nil
		})

	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	r.Guarded(http.MethodGet, "/api/pulls/{jobId}/stream", rbac.SelfService, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		servePullStream(w, req, d, lim)
	}))
}

// pullOwner is how a job records who started it.
func pullOwner(p *rbac.Principal) string { return string(p.Type) + ":" + p.ID }

// pullVisible reports whether the caller may read the job: the user who started it, or an
// instance admin.
func (d PullsDeps) pullVisible(ctx context.Context, p *rbac.Principal, job jobs.Job) bool {
	if job.Kind != pullJobKind {
		return false
	}
	if job.Owner == pullOwner(p) {
		return true
	}
	if d.Checker == nil {
		return false
	}
	a, err := d.Checker.Access(ctx, p)
	return err == nil && a.Active && a.InstanceAdmin
}

// startPull checks the reference, reads the environment's nodes and starts the job.
func startPull(ctx context.Context, d PullsDeps, in *PullStartInput) (*PullStartOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if d.Jobs == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemJobsUnavail, "Pulls are not available right now.")
	}
	ref, err := registry.ParseRef(in.Body.Ref)
	if err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemRefInvalid, "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.")
	}
	if in.Body.NodeIDs != nil && len(in.Body.NodeIDs) == 0 {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodesRequired, "Choose at least one node.")
	}
	agg, _, err := d.Aggregator.Snapshot(ctx, env.ID)
	if err != nil {
		return nil, snapshotProblem(d.Log, env.ID, err)
	}
	nodes := pullCandidates(agg)
	if len(nodes) == 0 {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "No node of this environment is connected to Bayplan.")
	}
	if in.Body.NodeIDs != nil {
		// Only this environment's own nodes may be named, and only by ID (T-05-13).
		chosen := make(map[string]bool, len(in.Body.NodeIDs))
		for _, id := range in.Body.NodeIDs {
			chosen[id] = true
		}
		var picked []envs.NodeRef
		for _, n := range nodes {
			if chosen[n.ID] {
				picked = append(picked, n)
				delete(chosen, n.ID)
			}
		}
		if len(chosen) > 0 {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemNodesUnknown, "One of the chosen nodes is not a node of this environment. Reload the page and choose again.")
		}
		nodes = picked
	}

	spec := jobs.Spec{
		Kind: pullJobKind, EnvID: env.ID, Owner: pullOwner(p),
		Meta: map[string]string{"ref": ref.String(), "credential": credentialAnonymous},
	}
	body := PullStartBody{Ref: ref.String(), Nodes: make([]PullNodeBody, 0, len(nodes))}
	for _, n := range nodes {
		spec.Items = append(spec.Items, jobs.ItemSpec{ID: n.ID, Label: n.Name})
		body.Nodes = append(body.Nodes, PullNodeBody{NodeID: n.ID, Name: n.Name})
	}

	// The audit rows keep the request's values (the caller's address) but not its
	// cancellation: a browser that closes must not stop the pull or lose its record.
	reqCtx := context.WithoutCancel(ctx)
	run := &pullRun{
		d: d, actor: p.AuditActor(), envID: env.ID, ref: ref, userAgent: in.UserAgent,
		ip: httpx.ClientIP(ctx), nodes: nodes, audit: reqCtx,
	}
	job, err := d.Jobs.Start(spec, run.run)
	if errors.Is(err, jobs.ErrTooMany) {
		return nil, httpx.Problem(http.StatusTooManyRequests, problemTooManyPulls, "You already have 5 pulls running. Wait for one to finish.")
	}
	if err != nil {
		d.Log.Error("could not start a pull job", "error", err)
		return nil, huma.Error500InternalServerError("internal error")
	}
	body.JobID = job.ID
	return &PullStartOutput{CacheControl: "no-store", Body: body}, nil
}

// pullCandidates are the nodes a pull may run on, by name: those that answered and those
// that did not, so a node that is down gets an item of its own that fails honestly and a
// pull that names every node leaves none out.
func pullCandidates(agg envs.Aggregate) []envs.NodeRef {
	nodes := slices.Clone(agg.Nodes)
	for _, u := range agg.Unreachable {
		if !slices.ContainsFunc(nodes, func(n envs.NodeRef) bool { return n.ID == u.ID }) {
			nodes = append(nodes, u)
		}
	}
	slices.SortFunc(nodes, func(a, b envs.NodeRef) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return nodes
}

// listPullTargets lists the nodes of the environment with what the node chooser shows.
func listPullTargets(ctx context.Context, d PullsDeps, in *EnvironmentPathInput) (*PullTargetsOutput, error) {
	agg, _, err := d.Aggregator.Snapshot(ctx, in.EnvID)
	if err != nil {
		return nil, snapshotProblem(d.Log, in.EnvID, err)
	}
	reachable := make(map[string]bool, len(agg.Nodes))
	for _, n := range agg.Nodes {
		reachable[n.ID] = true
	}
	nodes := pullCandidates(agg)
	body := PullTargetsBody{Nodes: make([]PullTargetBody, 0, len(nodes))}
	for _, n := range nodes {
		body.Swarm = body.Swarm || n.Role == "manager" || n.Role == "worker" || strings.HasPrefix(n.Role, "swarm")
		body.Nodes = append(body.Nodes, PullTargetBody{
			NodeID: n.ID, Name: n.Name, Role: n.Role, Arch: n.Arch, ReadOnly: n.ReadOnly, Reachable: reachable[n.ID],
		})
	}
	return &PullTargetsOutput{CacheControl: "no-store", Body: body}, nil
}

// pullRun is one pull job: the same reference on each of the nodes.
type pullRun struct {
	d         PullsDeps
	actor     audit.Actor
	envID     string
	ref       registry.Ref
	userAgent string
	ip        string
	nodes     []envs.NodeRef
	// audit carries the request's values with its cancellation removed.
	audit context.Context
}

// run pulls on every node, at most bulkParallel at once and perNodeCalls per node. A node
// that fails never stops the others.
func (pr *pullRun) run(ctx context.Context, r *jobs.Reporter) {
	lim := &nodeLimiter{sems: map[string]chan struct{}{}}
	var g errgroup.Group
	g.SetLimit(bulkParallel)
	for _, n := range pr.nodes {
		g.Go(func() error {
			if n.ReadOnly {
				// Named at once and never called: WriteOnNode would refuse it too, as the
				// backstop (D-30).
				pr.finish(r, n, dockerx.PullResult{}, codeHostReadOnly)
				return nil
			}
			release, err := lim.acquire(ctx, n.ID)
			if err != nil {
				pr.finish(r, n, dockerx.PullResult{}, dockerx.PullCodeCancelled)
				return nil
			}
			defer release()
			pr.pullOnNode(ctx, r, n)
			return nil
		})
	}
	_ = g.Wait() // every goroutine returns nil: failures are item states
}

// pullOnNode pulls on one node, once, and ends its item.
func (pr *pullRun) pullOnNode(ctx context.Context, r *jobs.Reporter, n envs.NodeRef) {
	nctx, cancel := context.WithTimeout(ctx, pr.d.NodeTimeout)
	defer cancel()
	r.Update(n.ID, func(it *jobs.Item) { it.State = jobs.ItemRunning })

	index := map[string]int{}
	var res dockerx.PullResult
	err := pr.d.Aggregator.WriteOnNode(nctx, pr.envID, n.ID, func(c *dockerx.Client) error {
		var e error
		res, e = c.ImagePull(nctx, pr.ref.String(), dockerx.PullOptions{}, func(ev dockerx.PullEvent) {
			r.Update(n.ID, func(it *jobs.Item) { applyPullEvent(it, index, ev) })
		})
		return e
	})
	if err != nil {
		code := pullErrorCode(err)
		if code == dockerx.PullCodeFailed || code == codeNodeUnreachable {
			// The text stays in the server log; the job carries the code only.
			pr.d.Log.Warn("a pull failed", "environment", pr.envID, "node", n.ID, "ref", pr.ref.String(), "code", code, "error", err)
		}
		pr.finish(r, n, res, code)
		return
	}
	pr.finish(r, n, res, "")
}

// finish writes the audit row of a node and then ends its item, so a browser that sees the
// node done or failed can find the row in the log.
func (pr *pullRun) finish(r *jobs.Reporter, n envs.NodeRef, res dockerx.PullResult, code string) {
	details := map[string]string{
		"ref": pr.ref.String(), "node": n.ID, "credential": credentialAnonymous, "bulk": r.JobID(),
	}
	result, reason := audit.ResultSuccess, ""
	switch {
	case code != "":
		result, reason = audit.ResultFailure, code
		details["code"] = code
	case res.UpToDate:
		details["code"] = "up_to_date"
	default:
		details["code"] = "pulled"
	}
	if res.Digest != "" {
		details["digest"] = res.Digest
	}
	wctx, cancel := doneActionAuditContext(pr.audit)
	if err := pr.d.Audit.Record(wctx, audit.Entry{
		Actor: pr.actor, Action: audit.ActionImagePulled, TargetType: imageTargetTyp, TargetID: pr.ref.String(),
		EnvID: pr.envID, Result: result, Reason: reason, IP: pr.ip, UserAgent: pr.userAgent, Details: details,
	}); err != nil {
		pr.d.Log.Error("could not audit a pull", "ref", pr.ref.String(), "node", n.ID, "error", err)
	}
	cancel()

	if code == "" {
		r.Update(n.ID, func(it *jobs.Item) {
			it.Done = it.Count
			if it.Total > 0 {
				it.Current = it.Total
			}
		})
		r.Finish(n.ID, jobs.ItemDone, "", "", map[string]string{"digest": res.Digest, "upToDate": strconv.FormatBool(res.UpToDate)})
		return
	}
	state := jobs.ItemFailed
	if code == dockerx.PullCodeCancelled {
		state = jobs.ItemCancelled
	}
	r.Finish(n.ID, state, code, pullMessages[code], nil)
}

// pullErrorCode maps the error of one node's pull to an item code.
func pullErrorCode(err error) string {
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrHostReadOnly):
		return codeHostReadOnly
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound), errors.Is(err, envs.ErrOffline),
		errors.Is(err, envs.ErrUnsupported), errors.Is(err, tunnel.ErrAgentGone):
		return codeNodeUnreachable
	case errors.As(envs.ClassifyDockerError(err), &denied):
		return codeAgentPolicyDeny
	}
	return dockerx.PullFailure(err)
}

// applyPullEvent folds one line of the engine's progress into the item: the layer it is
// about, and the totals across the layers. The item is the hub's own, held under its lock.
func applyPullEvent(it *jobs.Item, index map[string]int, ev dockerx.PullEvent) {
	it.State = jobs.ItemRunning
	if ev.Layer == "" {
		return
	}
	id := clipPullText(ev.Layer)
	i, ok := index[id]
	if !ok {
		if len(it.Subs) >= maxPullLayers {
			return
		}
		it.Subs = append(it.Subs, jobs.Sub{ID: id})
		i = len(it.Subs) - 1
		index[id] = i
	}
	s := &it.Subs[i]
	s.Status = clipPullText(ev.Status)
	if ev.Bytes {
		s.Current, s.Total = ev.Current, ev.Total
	}
	if layerFinished(s.Status) && s.Total > 0 {
		s.Current = s.Total
	}
	it.Count = len(it.Subs)
	it.Done, it.Current, it.Total = 0, 0, 0
	for _, l := range it.Subs {
		if layerFinished(l.Status) {
			it.Done++
		}
		it.Current += l.Current
		it.Total += l.Total
	}
	it.Unit = ""
	if it.Total > 0 {
		it.Unit = "bytes"
	}
}

func layerFinished(status string) bool {
	return status == "Pull complete" || status == "Already exists"
}

// clipPullText cuts a layer ID or status to maxPullText bytes on a rune boundary and drops
// control characters: it is shown to a browser as text.
func clipPullText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxPullText {
		s = strings.ToValidUTF8(s[:maxPullText], "")
	}
	return s
}

// pullItemBody maps a job item to the body the routes send.
func pullItemBody(it jobs.Item) PullItemBody {
	b := PullItemBody{
		NodeID: it.ID, Name: it.Label, State: it.State, Code: it.Code, Message: it.Message,
		Current: it.Current, Total: it.Total, Unit: it.Unit, Done: it.Done, Count: it.Count,
		Layers: make([]PullLayerBody, 0, len(it.Subs)), Digest: it.Result["digest"], UpToDate: it.Result["upToDate"] == "true",
	}
	for _, s := range it.Subs {
		b.Layers = append(b.Layers, PullLayerBody{ID: s.ID, Status: s.Status, Current: s.Current, Total: s.Total})
	}
	return b
}

// pullJobBody maps a job to the body the routes send.
func pullJobBody(j jobs.Job) PullJobBody {
	b := PullJobBody{
		ID: j.ID, EnvID: j.EnvID, Ref: j.Meta["ref"], State: j.State,
		CreatedAt: rfc3339(j.CreatedAt), FinishedAt: rfc3339(j.FinishedAt), Items: make([]PullItemBody, 0, len(j.Items)),
	}
	for _, it := range j.Items {
		b.Items = append(b.Items, pullItemBody(it))
	}
	return b
}

// servePullStream follows one pull: the job as it stands as a snapshot frame, then an item
// frame for each change and a done frame at the end, each with the hub's event ID. A
// browser that reconnects sends the last ID it saw in Last-Event-ID (or lastEventId in the
// query) and is sent a new
// snapshot and what it missed after that ID. An idle stream gets a comment line every
// Heartbeat, and each one re-checks that the caller is still allowed in.
//
// The pull does not depend on this stream: it runs on the server, and closing the stream
// does not stop it.
func servePullStream(w http.ResponseWriter, req *http.Request, d PullsDeps, lim *streamLimiter) {
	ctx := req.Context()
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Jobs == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemJobsUnavail, "Pulls are not available right now.")
		return
	}
	id := chi.URLParam(req, "jobId")
	if job, ok := d.Jobs.Get(id); !ok || !d.pullVisible(ctx, p, job) {
		writeJSONProblem(w, http.StatusNotFound, problemPullNotFound, "This pull is no longer here. Pulls are kept for 10 minutes after they end.")
		return
	}
	release, ok := lim.acquire(string(p.Type) + ":" + p.ID)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, problemTooManyStreams,
			fmt.Sprintf("You already have %d pull streams open. Close one and try again.", lim.max))
		return
	}
	defer release()

	// A browser's EventSource sends Last-Event-ID itself when it reconnects on its own. The
	// SPA reopens the stream by hand, which cannot set a header, so it says the same in
	// the query.
	var after uint64
	last := req.Header.Get("Last-Event-ID")
	if last == "" {
		last = req.URL.Query().Get("lastEventId")
	}
	if last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			after = n
		}
	}
	snap, ch, cancel, err := d.Jobs.Subscribe(ctx, id, after)
	if err != nil {
		writeJSONProblem(w, http.StatusNotFound, problemPullNotFound, "This pull is no longer here. Pulls are kept for 10 minutes after they end.")
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	frame := func(id, event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			d.Log.Error("encode a pull frame", "error", err)
			return false
		}
		if id == "" {
			return write("event: %s\ndata: %s\n\n", event, data)
		}
		return write("id: %s\nevent: %s\ndata: %s\n\n", id, event, data)
	}
	// The snapshot has no id: it is not an event.
	if !frame("", "snapshot", pullJobBody(snap)) {
		return
	}

	beat := time.NewTicker(d.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(d.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			return
		case ev, open := <-ch:
			if !open {
				// The channel closed without a done event: the job had already finished when
				// the stream opened and nothing newer than Last-Event-ID was left to replay.
				if job, ok := d.Jobs.Get(id); ok && job.Finished() {
					_ = frame("", "done", pullJobBody(job))
				}
				return
			}
			evID := strconv.FormatUint(ev.ID, 10)
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				if !frame(evID, "item", pullItemBody(*ev.Item)) {
					return
				}
			case ev.Type == jobs.EventDone && ev.Job != nil:
				_ = frame(evID, "done", pullJobBody(*ev.Job))
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				return
			}
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
