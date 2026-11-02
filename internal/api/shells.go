// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/audit"
	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/envs"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/rbac"
	"github.com/ianglennon/tierplan/internal/shells"
	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/tunnel"
)

// Problem types and limits of the browser shell.
const (
	problemShellSessionNotFound = "shell_session_not_found"
	problemShellFailed          = "shell_failed"
	problemShellUnavailable     = "shell_unavailable"
	problemInvalidShell         = "invalid_shell"
	problemContainerNotRunning  = "container_not_running"
	problemNoShell              = "no_shell"
	problemShellLimitReached    = "shell_limit_reached"
	problemShellNotFound        = "shell_not_found"

	// shellStartTimeout bounds creating the exec and attaching to it.
	shellStartTimeout = 30 * time.Second
	// shellAuditTimeout bounds the write of the row that ends a session, which runs
	// on whatever goroutine ended it.
	shellAuditTimeout = 10 * time.Second
)

// ShellsSource is what the shell routes use of an environment: the aggregate, to find
// the container's node and row, a read of the container from its engine (the fresh
// inspect that decides what may be opened), and the one write path to a node's engine. The exec
// is created through WriteOnNode because starting a process in a container changes
// the host (D-35): a read-only host refuses it, and the node's cached snapshot is
// dropped. *envs.Aggregator implements it.
type ShellsSource interface {
	snapshotReader
	OnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
	WriteOnNode(ctx context.Context, envID, nodeID string, fn func(*dockerx.Client) error) error
}

// PublicOriginSource says where the browser reaches this instance, read on every
// request. *instance.Instance implements it.
type PublicOriginSource interface {
	PublicOrigin() string
}

// ShellInstance is what the shell routes read of the instance, on every use: where the
// browser reaches it, how long a shell may sit unused and how many one person may hold
// (D-29, D-31). *instance.Instance implements it.
type ShellInstance interface {
	PublicOriginSource
	ShellIdle() time.Duration
	ShellCap() int
}

// ShellsDeps are the dependencies of the shell routes.
type ShellsDeps struct {
	Aggregator ShellsSource
	// Registry holds the open sessions. Without one the routes answer 503.
	Registry *shells.Registry
	// Audit records the start and the end of each session, never its contents.
	Audit *audit.Writer
	// Checker is the permission checker; the routes themselves are judged by the
	// route harness (containers.exec).
	Checker *rbac.Checker
	// SelfContainerID is TierPlan's own container ID; empty when it cannot tell. A shell
	// is never opened in that container, nor in an agent's (D-36).
	SelfContainerID string
	// Instance says which origin the WebSocket may be opened from, and the idle limit
	// and the per-person cap.
	Instance ShellInstance
	// Store keeps each user's last shell choice per image (D-26). Without one nothing is
	// remembered and the preference route answers 503.
	Store *store.Store
	// Now stamps a remembered choice. Nil means time.Now.
	Now func() time.Time
	// PendingTTL is how long a session waits for its browser. Zero means 30 s.
	PendingTTL time.Duration
	// Controls tune the byte bridge; tests shorten them.
	Controls shells.Controls
	// Log receives server-side failures. Nil means slog.Default(). It is never given
	// a byte typed into or printed by a shell.
	Log *slog.Logger
}

// ShellOpenInput is the path and body of POST .../containers/{containerId}/shell.
type ShellOpenInput struct {
	EnvironmentPathInput
	ContainerID string `path:"containerId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"The container ID or name"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ShellOpenBody
}

// ShellOpenBody says what to start in the container and how big the terminal is.
type ShellOpenBody struct {
	NodeID     string `json:"nodeId,omitempty" maxLength:"128" doc:"The node the container is on: the nodeId of its row in the containers list. May be left out only on an environment of one node that is not a swarm (422 node_required)"`
	Shell      string `json:"shell" enum:"auto,bash,sh,ash,custom" default:"auto" doc:"auto runs bash when the image has it and sh otherwise, in one exec. custom runs command"`
	Command    string `json:"command,omitempty" maxLength:"4096" doc:"With shell custom, the command to run, split into words the way a shell reads quotes and backslashes. It is never given to a shell, so nothing in it is expanded"`
	User       string `json:"user,omitempty" maxLength:"64" doc:"The user to run as, for example root, 0 or www-data. Empty means the container's own"`
	WorkingDir string `json:"workingDir,omitempty" maxLength:"4096" doc:"The working directory. Empty means the container's own"`
	Cols       int    `json:"cols" minimum:"1" maximum:"1000" default:"80" doc:"The terminal's width in characters"`
	Rows       int    `json:"rows" minimum:"1" maximum:"1000" default:"24" doc:"The terminal's height in lines"`
}

// ShellSessionBody is a pending session the browser attaches to.
type ShellSessionBody struct {
	SessionID  string   `json:"sessionId" doc:"The session's ID, single use and bound to the caller's sign-in"`
	WSPath     string   `json:"wsPath" doc:"The WebSocket to attach to within 30 seconds, as a path under the public URL's base path. Binary frames are the terminal's bytes; text frames are JSON control: resize from the browser, exit and closed from the server"`
	Command    []string `json:"command" doc:"The command that was started"`
	User       string   `json:"user" doc:"The user it runs as; empty means the container's own"`
	WorkingDir string   `json:"workingDir" doc:"Its working directory; empty means the container's own"`
}

// ShellOpenOutput wraps ShellSessionBody for huma.
type ShellOpenOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ShellSessionBody
}

// RegisterShells registers the browser shell: the POST that creates the exec and a
// pending session, and the WebSocket that attaches to it (OBS-03, RESEARCH Pattern 5).
//
// Two steps, because a browser cannot put a CSRF header on a WebSocket and the router
// checks Origin and the token only on mutating methods. The exec is made by the
// POST, which passes both checks; the GET upgrade claims the session it made, once,
// for the same user and the same sign-in, within 30 seconds, from the instance's own
// origin (T-04-76).
func RegisterShells(r *httpx.Router, d ShellsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	need := rbac.Requirement{Perm: rbac.PermContainersExec, Scope: rbac.ScopeEnvironment}
	if d.Registry != nil && d.Audit != nil {
		d.Registry.OnEnd(func(s *shells.Session, reason string, res shells.EndResult) {
			auditShellClosed(d, s, reason, res)
		})
	}

	httpx.Register(r, huma.Operation{
		OperationID: "openShell", Method: http.MethodPost, Path: "/api/env/{envId}/containers/{containerId}/shell",
		Summary: "Open a shell in a container",
		Description: "Starts an interactive TTY process in a running container and returns a pending session the browser attaches to over a WebSocket within 30 seconds, at wsPath. " +
			"The session is single use and bound to the caller and the caller's sign-in; the upgrade is accepted only from the instance's public origin. " +
			"The container lives on one node, named by nodeId, with the same rules and problems as the container detail route (422 node_required, 404 container_not_found, 503 node_unavailable). " +
			"422 invalid_shell when a custom command cannot be read. Refused before any exec is made: 429 shell_limit_reached when the caller already holds as many shells as the instance allows (the limit is the errors entry located at limit), " +
			"409 host_read_only on a read-only host, 409 own_container for TierPlan's own server or an agent's container (decided from a fresh inspect), 409 container_not_running, " +
			"502 agent_policy_denied when the node's agent is too old to run an exec, 409 no_shell when the engine cannot find the command, " +
			"503 stream_busy when the host's agent serves as many logs and shells as it can. " +
			"What is typed and printed is never recorded: the start is audited as shell.opened and the end as shell.closed. " +
			"Needs containers.exec on the environment (an extra permission: Operator only when granted; Admin always).",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated,
	}, need, func(ctx context.Context, in *ShellOpenInput) (*ShellOpenOutput, error) {
		return openShell(ctx, d, in)
	})

	r.Guarded(http.MethodGet, "/api/env/{envId}/shell-sessions/{sessionId}/ws", need, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveShell(w, req, d)
	}))

	registerShellPreference(r, d)
	registerShellSessions(r, d)
}

// userKey is the identity a session is bound to: the kind of principal and its ID.
func userKey(p *rbac.Principal) string { return string(p.Type) + ":" + p.ID }

// authKey is the sign-in a session is bound to: the hash of the session cookie, hex.
// A principal with no cookie (a token) binds to the user alone.
func authKey(p *rbac.Principal) string { return hex.EncodeToString(p.SessionIDHash) }

// hasControl reports whether s holds a control character, which no user name or
// directory has.
func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// commandLabel is how the audit log names the command: the kind for the built-in
// shells, and the words of a custom command.
func commandLabel(kind string, argv []string) string {
	if kind == "custom" {
		return strings.Join(argv, " ")
	}
	return kind
}

func openShell(ctx context.Context, d ShellsDeps, in *ShellOpenInput) (*ShellOpenOutput, error) {
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		return nil, huma.Error500InternalServerError("internal error")
	}
	if d.Registry == nil || d.Aggregator == nil || d.Audit == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
	}
	b := in.Body
	if hasControl(b.User) || hasControl(b.WorkingDir) {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValidationFailed, "The user and the working directory cannot hold control characters.")
	}
	argv, err := dockerx.ShellCommand(b.Shell, b.Command)
	if err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidShell, "That shell command cannot be run: "+err.Error()+".")
	}
	// The per-person cap is asked first, before anything is read from the engine; the
	// registry asks again, under its lock, when the session is made (D-31).
	userID := userKey(p)
	limit := 0
	if d.Instance != nil {
		limit = d.Instance.ShellCap()
	}
	if limit > 0 && d.Registry.CountForUser(userID) >= limit {
		return nil, shellLimitProblem(limit)
	}
	agg, nodeID, node, err := resolveContainerNode(ctx, d.Aggregator, d.Log, env.ID, b.NodeID)
	if err != nil {
		return nil, err
	}
	// A host whose owner set it read-only is refused from what is known of it, before
	// anything is sent (D-35). WriteOnNode refuses it again below.
	if node.ReadOnly {
		return nil, shellStartProblem(d.Log, env.ID, envs.ErrHostReadOnly)
	}
	fullID, name, image := shellContainerRow(agg, nodeID, in.ContainerID)

	// TierPlan's own server and agent containers are refused from a fresh inspect: the
	// list can be seconds old and the label can be set by anyone, so neither decides
	// (D-36, T-04-84). A container that is not running is refused here too.
	var own dockerx.ContainerOwnership
	inspectCtx, cancelInspect := context.WithTimeout(ctx, inspectTimeout)
	err = d.Aggregator.OnNode(inspectCtx, env.ID, nodeID, func(c *dockerx.Client) error {
		var e error
		own, e = c.ContainerOwnership(inspectCtx, in.ContainerID)
		return e
	})
	cancelInspect()
	if err != nil {
		return nil, shellStartProblem(d.Log, env.ID, err)
	}
	if own.ID != "" {
		fullID = own.ID
	}
	if own.Name != "" {
		name = own.Name
	}
	if (len(d.SelfContainerID) >= minSelfIDLen && strings.HasPrefix(own.ID, d.SelfContainerID)) ||
		own.Labels[ownerLabelAgent] == ownerLabelAgentValue {
		return nil, httpx.Problem(http.StatusConflict, codeOwnContainer, "TierPlan doesn't open shells in its own containers.")
	}
	if !own.Running {
		return nil, httpx.Problem(http.StatusConflict, problemContainerNotRunning, "The container is not running. Start it to open a shell.")
	}

	var exec *dockerx.ExecSession
	startCtx, cancel := context.WithTimeout(ctx, shellStartTimeout)
	defer cancel()
	err = d.Aggregator.WriteOnNode(startCtx, env.ID, nodeID, func(c *dockerx.Client) error {
		var e error
		exec, e = c.StartExec(startCtx, fullID, dockerx.ExecOptions{
			Cmd: argv, User: b.User, WorkingDir: b.WorkingDir, Cols: uint(b.Cols), Rows: uint(b.Rows),
		})
		return e
	})
	if err != nil {
		return nil, shellStartProblem(d.Log, env.ID, err)
	}

	s, err := d.Registry.CreateLimited(&shells.Session{
		UserID: userID, AuthSession: authKey(p), Actor: p.AuditActor(),
		IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
		EnvID: env.ID, NodeID: nodeID, Node: node.Name,
		ContainerID: fullID, ContainerName: name, Image: image,
		Command: argv, User: b.User, WorkingDir: b.WorkingDir,
	}, exec, d.PendingTTL, limit)
	if errors.Is(err, shells.ErrLimit) {
		return nil, shellLimitProblem(limit)
	}
	if s == nil {
		return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "TierPlan is stopping, so it cannot open a shell now.")
	}
	// The row is written before the browser can attach. A shell the log does not know
	// about is not allowed to run, so a failed write ends the session.
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: s.Actor, Action: audit.ActionShellOpened, TargetType: logTargetType, TargetID: fullID,
		EnvID: env.ID, Result: audit.ResultSuccess, IP: s.IP, UserAgent: s.UserAgent,
		Details: map[string]string{
			"container": name, "node": nodeID, "sid": s.ID, "command": commandLabel(b.Shell, argv),
			"user": b.User, "workdir": b.WorkingDir,
		},
	}); err != nil {
		d.Log.Error("could not audit the opening of a shell; the session was ended", "container", fullID, "error", err)
		d.Registry.End(s.ID, "audit_failed")
		return nil, huma.Error500InternalServerError("internal error")
	}
	s.MarkOpened()
	// How this person opened it is remembered for the next container of the image. A
	// failure to save is logged and never fails an open that has already happened.
	rememberShell(ctx, d, p, own.Image, image, b)
	return &ShellOpenOutput{CacheControl: "no-store", Body: ShellSessionBody{
		SessionID:  s.ID,
		WSPath:     "/api/env/" + env.ID + "/shell-sessions/" + s.ID + "/ws",
		Command:    argv,
		User:       b.User,
		WorkingDir: b.WorkingDir,
	}}, nil
}

// shellLimitProblem is the 429 for a person who holds as many shells as allowed. The limit
// is among the errors, as location "limit", for the page to say it.
func shellLimitProblem(limit int) error {
	return &huma.ErrorModel{
		Type: problemShellLimitReached, Title: http.StatusText(http.StatusTooManyRequests), Status: http.StatusTooManyRequests,
		Detail: "You already have " + strconv.Itoa(limit) + " shells open, which is the most allowed. Close one to open another.",
		Errors: []*huma.ErrorDetail{{Location: "limit", Message: strconv.Itoa(limit)}},
	}
}

// shellContainerRow finds the container the route named in the aggregate, for its full
// ID, name and image. Without a row the full ID and the name are what was asked.
func shellContainerRow(agg envs.Aggregate, nodeID, containerID string) (fullID, name, image string) {
	for _, row := range agg.Containers {
		if row.NodeID == nodeID && (row.ID == containerID || (len(containerID) >= minSelfIDLen && strings.HasPrefix(row.ID, containerID)) || row.Name == containerID) {
			return row.ID, row.Name, row.Image
		}
	}
	return containerID, containerID, ""
}

// shellStartProblem maps the error of creating and attaching an exec to the problem to
// answer. Nothing was registered and nothing is audited for these.
func shellStartProblem(log *slog.Logger, envID string, err error) error {
	var denied *envs.AgentPolicyDeniedError
	switch {
	case errors.Is(err, envs.ErrHostReadOnly):
		return httpx.Problem(http.StatusConflict, codeHostReadOnly, "This host is read-only, so TierPlan cannot open a shell on it. Logs still work.")
	case errors.Is(err, envs.ErrNodeUnavailable), errors.Is(err, envs.ErrNotFound):
		return httpx.Problem(http.StatusServiceUnavailable, problemNodeUnavailable, "The node is not connected to TierPlan.")
	case errors.Is(err, tunnel.ErrBusy):
		return httpx.Problem(http.StatusServiceUnavailable, problemStreamBusy,
			"This host's agent is serving as many logs and shells as it can. Close one and try again.")
	case errors.Is(err, dockerx.ErrNoShell):
		return httpx.Problem(http.StatusConflict, problemNoShell, "The container has no such shell. If the image has one somewhere else, enter its path.")
	case dockerx.IsNotFound(err):
		return httpx.Problem(http.StatusNotFound, problemContainerNotFound, "This container does not exist on that node. It may have been removed.")
	case dockerx.IsConflict(err):
		return httpx.Problem(http.StatusConflict, problemContainerNotRunning, "The container is not running. Start it to open a shell.")
	case errors.Is(err, envs.ErrOffline), errors.Is(err, envs.ErrUnsupported), errors.Is(err, tunnel.ErrAgentGone):
		return httpx.Problem(http.StatusConflict, codeNodeUnreachable, "The node did not answer.")
	case errors.As(envs.ClassifyDockerError(err), &denied):
		log.Warn("an agent refused to start a shell", "environment", envID, "endpoint", denied.Endpoint)
		return httpx.Problem(http.StatusBadGateway, problemAgentPolicyDenied, "The agent refused a request ("+denied.Endpoint+").")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return httpx.Problem(http.StatusServiceUnavailable, "", "The node took too long to answer.")
	}
	log.Error("starting a shell failed", "environment", envID, "error", err)
	return httpx.Problem(http.StatusBadGateway, problemShellFailed, "Docker could not open the shell.")
}

// escapePattern makes s a literal for the path.Match patterns of the WebSocket origin
// check, which read brackets, stars and question marks as wildcards.
func escapePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(s)
}

// originPatterns are the origins a shell WebSocket may be opened from besides the
// request's own host: the instance's public origin, scheme included, read on this
// request so a change of the public URL applies at once (T-04-76). With no public URL
// the request's own host is the only one allowed. The option that switches the origin
// check off is never used.
func originPatterns(in PublicOriginSource) []string {
	if in == nil {
		return nil
	}
	o := httpx.OriginOf(in.PublicOrigin())
	if o == "" {
		return nil
	}
	return []string{escapePattern(o)}
}

// serveShell attaches a browser to a pending session and carries bytes until one side
// ends. The policy has run when it is called, so the principal and the environment are
// on the context.
func serveShell(w http.ResponseWriter, req *http.Request, d ShellsDeps) {
	ctx := req.Context()
	p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
	if p == nil || env == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Registry == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		return
	}
	sid := chi.URLParam(req, "sessionId")
	notFound := func() {
		writeJSONProblem(w, http.StatusNotFound, problemShellSessionNotFound, "There is no such shell session. It may have expired or been used already; open a new one.")
	}
	// Asked before the upgrade, so a refusal is a plain 404, and a request that the
	// origin check then refuses does not use the session up.
	if err := d.Registry.Check(sid, env.ID, userKey(p), authKey(p)); err != nil {
		notFound()
		return
	}

	// The server's read timeout was set on the connection for the request; a shell
	// lives for hours.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		OriginPatterns:  originPatterns(d.Instance),
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept has answered the client (403 for another origin)
	}
	defer func() { _ = conn.CloseNow() }()

	s, err := d.Registry.Claim(sid, userKey(p), authKey(p))
	if err != nil {
		// A second attach that raced the first.
		_ = conn.Close(websocket.StatusPolicyViolation, "no such session")
		return
	}
	ctl := d.Controls
	if ctl.Clock == nil {
		ctl.Clock = d.Registry.Clock()
	}
	if ctl.Idle == nil && d.Instance != nil {
		ctl.Idle = d.Instance.ShellIdle // read on every tick, never captured
	}
	if ctl.StillAllowed == nil {
		// The policy that let the upgrade in, asked again on every tick: a shell does not
		// outlive its user's sign-in, their grant on the environment or the permission
		// itself (P3 D-10, T-04-83).
		ctl.StillAllowed = func() bool { return httpx.StillAuthorized(ctx) }
	}
	reason := shells.Bridge(ctx, conn, s, ctl)
	d.Registry.End(s.ID, reason)
}

// auditShellClosed writes the shell.closed row of a session that was opened: how long
// it lasted, how its process ended or that it was left running, and why the session
// ended. It runs on whichever goroutine ended the session.
func auditShellClosed(d ShellsDeps, s *shells.Session, reason string, res shells.EndResult) {
	if !s.Opened() {
		return
	}
	details := map[string]string{
		"container": s.ContainerName, "node": s.NodeID, "sid": s.ID,
		"duration_ms": strconv.FormatInt(res.Duration.Milliseconds(), 10),
		"reason":      reason,
	}
	if res.ExitCode != nil {
		details["exit_code"] = strconv.Itoa(*res.ExitCode)
	} else {
		details["still_running"] = "true"
	}
	ctx, cancel := context.WithTimeout(context.Background(), shellAuditTimeout)
	defer cancel()
	if err := d.Audit.Record(ctx, audit.Entry{
		Actor: s.Actor, Action: audit.ActionShellClosed, TargetType: logTargetType, TargetID: s.ContainerID,
		EnvID: s.EnvID, Result: audit.ResultSuccess, IP: s.IP, UserAgent: s.UserAgent, Details: details,
	}); err != nil {
		d.Log.Error("could not audit the end of a shell", "container", s.ContainerID, "error", err)
	}
}

var _ ShellsSource = (*envs.Aggregator)(nil)

// ShellItemBody is one open shell in a list.
type ShellItemBody struct {
	ID          string    `json:"id" doc:"The session's ID, the one to end it by"`
	User        string    `json:"user" doc:"The name of the person it belongs to"`
	ContainerID string    `json:"containerId" doc:"The container's full ID"`
	Container   string    `json:"container" doc:"The container's name"`
	Node        string    `json:"node" doc:"The name of the node the container is on"`
	StartedAt   time.Time `json:"startedAt" doc:"When the session was opened"`
	IdleSeconds int64     `json:"idleSeconds" doc:"Seconds since anything was typed into or printed by the shell"`
}

// ShellListBody is a list of open shells.
type ShellListBody struct {
	Items []ShellItemBody `json:"items" doc:"Oldest first; empty when there are none"`
}

// ShellListOutput wraps ShellListBody for huma.
type ShellListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ShellListBody
}

// ActiveShellsInput is the path of GET .../active-shells.
type ActiveShellsInput struct {
	EnvironmentPathInput
}

// MyShellsInput is the path and query of GET .../shells.
type MyShellsInput struct {
	EnvironmentPathInput
	ContainerID string `query:"containerId" pattern:"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$" maxLength:"128" doc:"Only the shells in this container: its ID or name. Left out, every shell the caller has open on the environment"`
}

// EndShellInput is the path of DELETE .../active-shells/{shellId} and of DELETE
// .../shells/{shellId}.
type EndShellInput struct {
	EnvironmentPathInput
	ShellID string `path:"shellId" pattern:"^[A-Za-z0-9_-]{1,64}$" maxLength:"64" doc:"The session's ID"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// shellItems turns the registry's facts into the list a route answers.
func shellItems(d ShellsDeps, infos []shells.Info) []ShellItemBody {
	now := d.Registry.Now()
	out := make([]ShellItemBody, 0, len(infos))
	for _, in := range infos {
		out = append(out, ShellItemBody{
			ID: in.ID, User: in.Owner, ContainerID: in.ContainerID, Container: in.ContainerName, Node: in.Node,
			StartedAt: in.CreatedAt.UTC(), IdleSeconds: max(int64(now.Sub(in.LastActivity)/time.Second), 0),
		})
	}
	return out
}

func shellNotFound() error {
	return httpx.Problem(http.StatusNotFound, problemShellNotFound, "This shell is already closed.")
}

// registerShellSessions registers the lists of open shells and the ends of them: the
// administrator's, of everyone's on the environment (shells.manage), each a route of
// its own so the route harness judges it by one requirement (D-37, T-04-94).
func registerShellSessions(r *httpx.Router, d ShellsDeps) {
	manage := rbac.Requirement{Perm: rbac.PermShellsManage, Scope: rbac.ScopeEnvironment}
	exec := rbac.Requirement{Perm: rbac.PermContainersExec, Scope: rbac.ScopeEnvironment}

	httpx.Register(r, huma.Operation{
		OperationID: "listMyShells", Method: http.MethodGet, Path: "/api/env/{envId}/shells",
		Summary: "List the caller's own open shells",
		Description: "The shells the caller has open on the environment, oldest first, and only those: other people's are never listed here, whatever the caller's role. " +
			"With containerId, only the ones in that container. Needs containers.exec on the environment.",
		Tags: []string{"environments"},
	}, exec, func(ctx context.Context, in *MyShellsInput) (*ShellListOutput, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if d.Registry == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		}
		mine := d.Registry.List(shells.Filter{EnvID: env.ID, UserID: userKey(p)})
		if want := in.ContainerID; want != "" {
			kept := mine[:0]
			for _, s := range mine {
				if s.ContainerID == want || s.ContainerName == want || (len(want) >= minSelfIDLen && strings.HasPrefix(s.ContainerID, want)) {
					kept = append(kept, s)
				}
			}
			mine = kept
		}
		return &ShellListOutput{CacheControl: "no-store", Body: ShellListBody{Items: shellItems(d, mine)}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "endMyShell", Method: http.MethodDelete, Path: "/api/env/{envId}/shells/{shellId}",
		Summary: "End one of the caller's own shells", DefaultStatus: http.StatusNoContent,
		Description: "Closes one of the caller's own open shells at once; its browser is told it was ended by the user (reason ended_by_user) and its shell.closed row says the same. " +
			"404 shell_not_found when it already closed, belongs to another environment, or belongs to someone else: the answer is the same for all three. Needs containers.exec on the environment.",
		Tags: []string{"environments"},
	}, exec, func(ctx context.Context, in *EndShellInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if d.Registry == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		}
		// Someone else's session answers as a missing one (T-04-95).
		info, ok := d.Registry.Get(in.ShellID)
		if !ok || info.EnvID != env.ID || info.UserID != userKey(p) {
			return nil, shellNotFound()
		}
		if _, ok := d.Registry.End(info.ID, shells.ReasonEndedByUser); !ok {
			return nil, shellNotFound()
		}
		return nil, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listActiveShells", Method: http.MethodGet, Path: "/api/env/{envId}/active-shells",
		Summary: "List every open shell on an environment",
		Description: "Every browser shell that is open on the environment, whoever opened it, oldest first: the person, the container, its node, when it was opened and how long it has sat unused. " +
			"A shell that is waiting for its browser to attach is listed too. Needs shells.manage on the environment (env Admin or instance admin).",
		Tags: []string{"environments"},
	}, manage, func(ctx context.Context, in *ActiveShellsInput) (*ShellListOutput, error) {
		env := httpx.EnvFrom(ctx)
		if env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if d.Registry == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		}
		return &ShellListOutput{CacheControl: "no-store", Body: ShellListBody{
			Items: shellItems(d, d.Registry.List(shells.Filter{EnvID: env.ID})),
		}}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "endShell", Method: http.MethodDelete, Path: "/api/env/{envId}/active-shells/{shellId}",
		Summary: "End someone's shell", DefaultStatus: http.StatusNoContent,
		Description: "Closes an open shell on the environment at once. Its browser is told it was ended by an administrator (reason ended_by_admin). " +
			"404 shell_not_found when it already closed, or belongs to another environment. A process the person left running in the container keeps running. " +
			"Needs shells.manage on the environment (env Admin or instance admin). Audited as shell.ended by the administrator, naming the owner, and the shell's own shell.closed row carries the reason ended_by_admin.",
		Tags: []string{"environments"},
	}, manage, func(ctx context.Context, in *EndShellInput) (*struct{}, error) {
		p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
		if p == nil || env == nil {
			return nil, huma.Error500InternalServerError("internal error")
		}
		if d.Registry == nil || d.Audit == nil {
			return nil, httpx.Problem(http.StatusServiceUnavailable, problemShellUnavailable, "Shells are not available on this server.")
		}
		// A session of another environment answers as a missing one (T-04-94).
		info, ok := d.Registry.Get(in.ShellID)
		if !ok || info.EnvID != env.ID {
			return nil, shellNotFound()
		}
		if _, ok := d.Registry.End(info.ID, shells.ReasonEndedByAdmin); !ok {
			return nil, shellNotFound() // it ended between the look and the end
		}
		// The shell is already ended, which is the safe side of a failed write.
		wctx, cancel := doneActionAuditContext(ctx)
		defer cancel()
		if err := d.Audit.Record(wctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionShellEnded, TargetType: logTargetType, TargetID: info.ContainerID,
			EnvID: env.ID, Result: audit.ResultSuccess, IP: httpx.ClientIP(ctx), UserAgent: in.UserAgent,
			Details: map[string]string{"container": info.ContainerName, "node": info.NodeID, "sid": info.ID, "owner": info.Owner},
		}); err != nil {
			d.Log.Error("could not audit an administrator ending a shell", "container", info.ContainerID, "error", err)
			return nil, huma.Error500InternalServerError("internal error")
		}
		return nil, nil
	})
}
