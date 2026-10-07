// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package httpx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/realclientip/realclientip-go"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// Authenticator resolves the caller of a request. A nil principal with a nil
// error means the caller is anonymous; an error means the caller could not be
// determined (a store failure), and the request is refused.
type Authenticator interface {
	Authenticate(r *http.Request) (*rbac.Principal, error)
}

// SessionRechecker is implemented by an Authenticator that can re-validate an
// already-authenticated principal without a request and without extending the
// session (no last-seen touch, no deletes). Long-lived routes (server-sent
// events now, WebSockets later) use it through StillAuthorized, so they end when
// the session, its user or its permission is revoked rather than at the next
// request that never comes.
type SessionRechecker interface {
	Recheck(ctx context.Context, p *rbac.Principal) (bool, error)
}

// AnonymousAuthenticator authenticates nobody. The server uses it until the
// session-backed authenticator exists (plan 12).
type AnonymousAuthenticator struct{}

// Authenticate implements Authenticator.
func (AnonymousAuthenticator) Authenticate(*http.Request) (*rbac.Principal, error) { return nil, nil }

// Env is a resolved environment.
type Env struct {
	ID, Name string
	Enabled  bool
}

// Environment lookup errors.
var (
	ErrEnvNotFound = errors.New("httpx: environment not found")
	ErrEnvDisabled = errors.New("httpx: environment disabled")
)

// EnvResolver looks an environment up by ID. It returns ErrEnvNotFound or
// ErrEnvDisabled; any other error is a failure of the lookup itself.
type EnvResolver interface {
	Resolve(ctx context.Context, envID string) (Env, error)
}

// NoEnvironments is the EnvResolver for a server with no environments yet:
// every lookup is ErrEnvNotFound. The DB-backed resolver replaces it in plan 16.
type NoEnvironments struct{}

// Resolve implements EnvResolver.
func (NoEnvironments) Resolve(context.Context, string) (Env, error) { return Env{}, ErrEnvNotFound }

// Auditor records audit entries. *audit.Writer implements it.
type Auditor interface {
	Record(ctx context.Context, e audit.Entry) error
}

// MutationGuard vets an authenticated, mutating request before the permission
// check; the session-bound CSRF token check plugs in here (plan 12). A non-nil
// error refuses the request with 403 csrf_failed. It is not called for
// anonymous callers, who have no session to forge a request against.
type MutationGuard interface {
	Check(r *http.Request, p *rbac.Principal) error
}

type (
	principalKey struct{}
	envKey       struct{}
	clientKey    struct{}
	recheckKey   struct{}
)

// client is what the clientIP middleware learned about the TCP peer.
type client struct {
	ip string
	// trustedPeer is true when the TCP peer is inside BAYPLAN_TRUSTED_PROXIES.
	trustedPeer bool
	// scheme is the scheme the caller used, as EffectiveScheme works it out.
	scheme string
}

// PrincipalFrom returns the authenticated principal of the request, nil when
// the caller is anonymous.
func PrincipalFrom(ctx context.Context) *rbac.Principal {
	p, _ := ctx.Value(principalKey{}).(*rbac.Principal)
	return p
}

// EnvFrom returns the environment the policy resolved for an environment-scoped
// route, nil for any other route.
func EnvFrom(ctx context.Context) *Env {
	e, _ := ctx.Value(envKey{}).(*Env)
	return e
}

// StillAuthorized re-runs the policy that let a Guarded request in, against the
// state of the world now: the session behind the principal must still be valid
// (when the Authenticator is a SessionRechecker), an environment-scoped route's
// environment must still exist and be enabled, and the Checker must still allow
// the requirement. A long-lived handler calls it periodically and returns when
// it is false; the client's reconnect then goes through the full policy and gets
// a 401 or 403. It writes nothing to the response, audits nothing and never
// extends the session. Any failure of a lookup counts as false, so a stream
// fails closed. On a context that did not come through Guarded it is true,
// because there is nothing to re-check.
func StillAuthorized(ctx context.Context) bool {
	fn, ok := ctx.Value(recheckKey{}).(func(context.Context) bool)
	if !ok {
		return true
	}
	return fn(ctx)
}

// recheck builds the closure Guarded stores for StillAuthorized: the same three
// checks authorize makes, for this principal, requirement and environment.
func (r *Router) recheck(route string, p *rbac.Principal, need rbac.Requirement, envID string) func(context.Context) bool {
	return func(ctx context.Context) bool {
		deny := func(why string, err error) bool {
			if err != nil {
				r.opts.Logger.Warn("a long-lived stream is no longer authorised", "route", route, "why", why, "error", err)
			}
			return false
		}
		if rc, ok := r.opts.Authenticator.(SessionRechecker); ok {
			valid, err := rc.Recheck(ctx, p)
			if err != nil || !valid {
				return deny("session", err)
			}
		}
		if need.Perm != "" && need.Scope == rbac.ScopeEnvironment {
			e, err := r.opts.EnvResolver.Resolve(ctx, envID)
			if err != nil || !e.Enabled {
				return deny("environment", ignoreEnvState(err))
			}
		}
		if r.opts.Checker == nil {
			return deny("no checker", nil)
		}
		d := r.opts.Checker.Check(ctx, p, need, envID)
		if d.Err != nil || !d.Allowed {
			return deny("permission", d.Err)
		}
		return true
	}
}

// ignoreEnvState hides the expected "gone" and "disabled" outcomes from the
// warning, which is for lookups that failed.
func ignoreEnvState(err error) error {
	if errors.Is(err, ErrEnvNotFound) || errors.Is(err, ErrEnvDisabled) {
		return nil
	}
	return err
}

// ClientIP returns the client address the audit log records: the TCP peer, or
// the address a trusted proxy forwarded (D-39, RESEARCH Pitfall 6).
func ClientIP(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(client)
	return c.ip
}

// EffectiveScheme is the scheme the caller used to reach Bayplan: https for a
// request that arrived over TLS, the scheme a trusted proxy reports in
// X-Forwarded-Proto, and http for everything else. The header of a peer that is
// not in BAYPLAN_TRUSTED_PROXIES is never read, and neither is any other
// forwarded header (D-51). Outside the client-address middleware it is http.
func EffectiveScheme(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(client)
	if c.scheme == "" {
		return schemeHTTP
	}
	return c.scheme
}

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// forwardedScheme is the scheme the last proxy in a chain put in
// X-Forwarded-Proto, or "" when the header says nothing usable. The last value is
// the one the trusted peer closest to Bayplan added; the ones before it were
// supplied further out.
func forwardedScheme(h http.Header) string {
	vals := h.Values("X-Forwarded-Proto")
	if len(vals) == 0 {
		return ""
	}
	last := vals[len(vals)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	switch p := strings.ToLower(strings.TrimSpace(last)); p {
	case schemeHTTP, schemeHTTPS:
		return p
	}
	return ""
}

func peerTrusted(ctx context.Context) bool {
	c, _ := ctx.Value(clientKey{}).(client)
	return c.trustedPeer
}

// PeerTrusted reports whether the TCP peer of the request is one of
// BAYPLAN_TRUSTED_PROXIES. The agent endpoint uses it to decide whether a
// forwarded scheme and a TLS exporter value can be believed.
func PeerTrusted(ctx context.Context) bool { return peerTrusted(ctx) }

// Problem types the policy emits. The SPA maps them to copy.
const (
	problemOriginMismatch = "origin_mismatch"
	problemEnvNotFound    = "environment_not_found"
	problemEnvDisabled    = "environment_disabled"
	problemCSRFFailed     = "csrf_failed"
)

// Audit reasons for the denials the policy records, besides the checker's own
// "unauthenticated" and "forbidden".
const (
	reasonOriginMismatch = "origin_mismatch"
	reasonCSRF           = "csrf"
	// reasonEnvNotVisible is audited when a signed-in caller with no grant on an
	// environment asks for it and is answered 404 (D-08).
	reasonEnvNotVisible = "environment_not_visible"
)

// detailEnvNotFound is the one detail string an unseen and a missing environment
// share, so the two answers are byte-identical.
const detailEnvNotFound = "environment not found"

// envPathPrefix is where D-48 puts environment-scoped routes.
const envPathPrefix = "/api/env/{envId}"

func inEnvTree(path string) bool {
	return path == envPathPrefix || strings.HasPrefix(path, envPathPrefix+"/")
}

// checkRoute panics at registration when a requirement and its path disagree:
// an environment-scoped permission must live under /api/env/{envId}/ (so the
// environment is resolved), and nothing else may (D-48).
func checkRoute(method, path string, need rbac.Requirement) {
	need.MustBeValid()
	if envScoped := need.Perm != "" && need.Scope == rbac.ScopeEnvironment; envScoped != inEnvTree(path) {
		panic(fmt.Sprintf("httpx: %s %s: environment-scoped routes, and only those, live under %s/ (D-48)", method, path, envPathPrefix))
	}
}

// clientIP builds the middleware that records the client address. Forwarded
// headers are consulted only when the TCP peer is a trusted proxy, and then the
// rightmost address that is not itself a trusted proxy is used, so a client
// cannot choose the address by prepending to X-Forwarded-For.
func clientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return ClientIPMiddleware(trusted)
}

// ClientIPMiddleware is the middleware that records the client address and
// whether the TCP peer is a trusted proxy. The router installs it; it is exported
// so a handler served outside the router, and the tests of one, can sit behind
// the same logic.
func ClientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	var strategy realclientip.Strategy
	if len(trusted) > 0 {
		ranges := make([]net.IPNet, 0, len(trusted))
		for _, p := range trusted {
			p = p.Masked()
			ranges = append(ranges, net.IPNet{
				IP:   net.IP(p.Addr().AsSlice()),
				Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen()),
			})
		}
		s, err := realclientip.NewRightmostTrustedRangeStrategy("X-Forwarded-For", ranges)
		if err != nil {
			// The ranges come from validated config, so this cannot happen.
			panic("httpx: trusted proxy ranges: " + err.Error())
		}
		strategy = s
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := client{ip: req.RemoteAddr, scheme: schemeHTTP}
			if req.TLS != nil {
				c.scheme = schemeHTTPS
			}
			if ap, err := netip.ParseAddrPort(req.RemoteAddr); err == nil {
				peer := ap.Addr().Unmap()
				c.ip = peer.String()
				if strategy != nil && inPrefixes(peer, trusted) {
					c.trustedPeer = true
					if req.TLS == nil {
						if p := forwardedScheme(req.Header); p != "" {
							c.scheme = p
						}
					}
					if fwd := strategy.ClientIP(req.Header, req.RemoteAddr); fwd != "" {
						c.ip = fwd
					}
				}
			}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), clientKey{}, c)))
		})
	}
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// OriginOf returns the "scheme://host[:port]" origin of a URL, lower-cased with
// a default port dropped, or "" when raw is not an absolute http(s) URL.
func OriginOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return canonicalOrigin(u.Scheme, u.Host)
}

func canonicalOrigin(scheme, host string) string {
	scheme, host = strings.ToLower(scheme), strings.ToLower(host)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	return scheme + "://" + host
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// expectedOrigin is the origin a browser-issued mutation must come from: the
// configured public origin, or before setup stores one, the origin the request
// itself arrived at. The scheme is the effective one: the connection, or what a
// trusted proxy says in X-Forwarded-Proto.
func (r *Router) expectedOrigin(req *http.Request) string {
	if r.opts.PublicOrigin != nil {
		if o := r.opts.PublicOrigin(); o != "" {
			return canonicalOrigin(schemeOf(o), hostOf(o))
		}
	}
	scheme := EffectiveScheme(req.Context())
	if req.TLS != nil {
		scheme = schemeHTTPS
	}
	return canonicalOrigin(scheme, req.Host)
}

func schemeOf(origin string) string {
	scheme, _, _ := strings.Cut(origin, "://")
	return scheme
}

func hostOf(origin string) string {
	_, host, _ := strings.Cut(origin, "://")
	return host
}

// originOK applies D-36: a mutation must come from the public origin. When the
// browser sent Origin it must match exactly; when it did not, Sec-Fetch-Site
// must not say the request is cross-site.
func (r *Router) originOK(req *http.Request) (ok bool, got, want string) {
	want = r.expectedOrigin(req)
	if origin := req.Header.Get("Origin"); origin != "" {
		got = origin
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
			return false, got, want
		}
		return canonicalOrigin(u.Scheme, u.Host) == want && want != "", got, want
	}
	switch req.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true, "", want
	}
	return false, "sec-fetch-site=" + req.Header.Get("Sec-Fetch-Site"), want
}

// authorize is the one policy gate every route passes through. In order it
// authenticates, applies the Origin guard and MutationGuard to mutations,
// answers 401 to an anonymous caller of a protected route, and then, for an
// environment-scoped route, decides visibility from the caller's grants alone
// (D-08): a user with no grant on the environment is answered 404
// environment_not_found, the same answer as for an ID that does not exist, and is
// audited as permission.denied with reason environment_not_visible. Only a
// caller who can see the environment learns whether it exists and is enabled (404
// for a gone one, 409 for a disabled one, D-48 as narrowed by D-08); then the
// Checker is asked and a role that is too low is answered 403. It writes the
// response itself when the request must not proceed and reports ok=false.
//
// Denials (401, 403, and the 404 for an unseen environment) are audited as
// permission.denied (D-49); if that write fails the answer is 500 and the request
// is still refused. Allowed requests are not audited here: allowed mutations are
// audited by their handlers.
func (r *Router) authorize(w http.ResponseWriter, req *http.Request, need rbac.Requirement, envID string) (*rbac.Principal, *Env, bool) {
	ctx := req.Context()

	p, err := r.opts.Authenticator.Authenticate(req)
	if err != nil {
		r.opts.Logger.Error("authentication failed", "error", err)
		if !need.Public {
			writeProblem(w, http.StatusInternalServerError, "internal error")
			return nil, nil, false
		}
		p = nil // a Public route needs no identity; the caller is anonymous
	}

	if isMutating(req.Method) {
		if ok, got, want := r.originOK(req); !ok {
			r.opts.Logger.Warn("mutating request refused: Origin does not match the public URL",
				"origin", truncate(got, 200), "expected", want,
				"public_url", r.publicOrigin(), "path", req.URL.Path, "ip", ClientIP(ctx))
			r.deny(w, req, p, http.StatusForbidden, problemOriginMismatch, reasonOriginMismatch, envID,
				"The request did not come from this Bayplan's public URL.")
			return nil, nil, false
		}
		if p != nil && r.opts.MutationGuard != nil {
			if err := r.opts.MutationGuard.Check(req, p); err != nil {
				r.deny(w, req, p, http.StatusForbidden, problemCSRFFailed, reasonCSRF, envID,
					"The request is missing a valid CSRF token.")
				return nil, nil, false
			}
		}
	}

	if need.Public {
		return p, nil, true
	}
	if p == nil {
		// Before the environment lookup, so an anonymous caller cannot probe
		// which environments exist.
		r.deny(w, req, nil, http.StatusUnauthorized, "", rbac.ReasonUnauthenticated, envID, "Sign in to continue.")
		return nil, nil, false
	}

	var env *Env
	if need.Perm != "" && need.Scope == rbac.ScopeEnvironment {
		// Visibility comes from the grant alone and is decided before any lookup,
		// so a caller cannot tell an environment they have no grant on from one
		// that does not exist (D-08). System principals hold no environment grants
		// and fall through to the Checker.
		if p.Type == rbac.PrincipalUser {
			if r.opts.Checker == nil {
				r.deny(w, req, p, http.StatusForbidden, "", rbac.ReasonForbidden, envID, "You do not have permission to do this.")
				return nil, nil, false
			}
			ea, err := r.opts.Checker.EnvAccess(ctx, p, envID)
			if err != nil {
				r.opts.Logger.Error("permission check failed", "error", err)
				writeProblem(w, http.StatusInternalServerError, "internal error")
				return nil, nil, false
			}
			if ea.Role == rbac.RoleNone {
				r.deny(w, req, p, http.StatusNotFound, problemEnvNotFound, reasonEnvNotVisible, envID, detailEnvNotFound)
				return nil, nil, false
			}
		}
		e, err := r.opts.EnvResolver.Resolve(ctx, envID)
		switch {
		case errors.Is(err, ErrEnvNotFound):
			writeProblemType(w, http.StatusNotFound, problemEnvNotFound, detailEnvNotFound)
			return nil, nil, false
		case errors.Is(err, ErrEnvDisabled) || (err == nil && !e.Enabled):
			writeProblemType(w, http.StatusConflict, problemEnvDisabled, "environment is disabled")
			return nil, nil, false
		case err != nil:
			r.opts.Logger.Error("environment lookup failed", "error", err)
			writeProblem(w, http.StatusInternalServerError, "internal error")
			return nil, nil, false
		}
		env = &e
	}

	d := rbac.Decision{Reason: rbac.ReasonForbidden}
	if r.opts.Checker != nil {
		d = r.opts.Checker.Check(ctx, p, need, envID)
	}
	if d.Err != nil {
		r.opts.Logger.Error("permission check failed", "error", d.Err)
		writeProblem(w, http.StatusInternalServerError, "internal error")
		return nil, nil, false
	}
	if !d.Allowed {
		status := http.StatusForbidden
		if d.Reason == rbac.ReasonUnauthenticated {
			status = http.StatusUnauthorized
		}
		r.deny(w, req, p, status, "", d.Reason, envID, "You do not have permission to do this.")
		return nil, nil, false
	}
	return p, env, true
}

func (r *Router) publicOrigin() string {
	if r.opts.PublicOrigin == nil {
		return ""
	}
	return r.opts.PublicOrigin()
}

// deny writes the refusal and records it. Without an Auditor (the openapi
// subcommand, which serves nothing) the denial is still written. With one, a
// failed write turns the response into a 500: a refusal nobody can see is not
// allowed to look like a normal one.
func (r *Router) deny(w http.ResponseWriter, req *http.Request, p *rbac.Principal, status int, ptype, reason, envID, detail string) {
	if r.opts.Auditor != nil {
		actor := audit.Actor{Type: audit.ActorAnonymous}
		if p != nil {
			actor = p.AuditActor()
		}
		pattern := req.URL.Path
		if rc := chi.RouteContext(req.Context()); rc != nil && rc.RoutePattern() != "" {
			pattern = rc.RoutePattern()
		}
		err := r.opts.Auditor.Record(req.Context(), audit.Entry{
			Actor:      actor,
			Action:     audit.ActionPermissionDenied,
			Result:     audit.ResultDenied,
			Reason:     reason,
			TargetType: "route",
			TargetID:   req.Method + " " + pattern,
			EnvID:      envID,
			IP:         ClientIP(req.Context()),
			UserAgent:  req.UserAgent(),
		})
		if err != nil {
			r.opts.Logger.Error("could not audit a denial; refusing with 500", "error", err, "reason", reason)
			writeProblem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	writeProblemType(w, status, ptype, detail)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// policy is the per-operation gate for huma operations. It runs before request
// parsing, so a denied request never reaches validation or the handler.
func (r *Router) policy(need rbac.Requirement) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		req, w := humachi.Unwrap(ctx)
		envID := ""
		if need.Scope == rbac.ScopeEnvironment {
			envID = ctx.Param("envId")
		}
		p, env, ok := r.authorize(w, req, need, envID)
		if !ok {
			return
		}
		ctx = huma.WithValue(ctx, principalKey{}, p)
		if env != nil {
			ctx = huma.WithValue(ctx, envKey{}, env)
		}
		next(ctx)
	}
}

// Guarded mounts a plain chi handler (SSE now, WebSocket later) behind the same
// policy as Register and records it in the route registry (D-47). Use it for
// anything huma cannot model; never mount on the mux directly.
func (r *Router) Guarded(method, path string, need rbac.Requirement, h http.Handler) {
	checkRoute(method, path, need)
	r.record(RouteRecord{Method: method, Path: path, Req: need})
	r.mux.Method(method, path, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		envID := ""
		if need.Scope == rbac.ScopeEnvironment {
			envID = chi.URLParam(req, "envId")
		}
		p, env, ok := r.authorize(w, req, need, envID)
		if !ok {
			return
		}
		ctx := context.WithValue(req.Context(), principalKey{}, p)
		if env != nil {
			ctx = context.WithValue(ctx, envKey{}, env)
		}
		// So a long-lived handler can re-validate what authorised it (WR-02).
		ctx = context.WithValue(ctx, recheckKey{}, r.recheck(method+" "+path, p, need, envID))
		h.ServeHTTP(w, req.WithContext(ctx))
	}))
}

// exemptPaths is the D-47 list of routes outside the policy: the two probes and
// the /setup page. /setup is exempt because it is a plain page route, not an API
// operation: it serves the SPA while setup is needed and redirects to /login
// once an admin exists (D-06). /login is an SPA route served by the fallback,
// and /api/version is registered as Public.
//
// /agent/connect is the fifth: the WebSocket an agent dials. It is exempt from
// user authentication because the agent handshake is its own authentication
// (join token or certificate, proof of possession, per-environment CA), and it
// is not in the OpenAPI contract.
var exemptPaths = map[string]bool{"/healthz": true, "/readyz": true, "/setup": true, "/setup/*": true, "/agent/connect": true}

// Problem builds the RFC 9457 error a huma handler returns when it must answer
// with a specific status and a stable machine-readable type the SPA maps to
// copy, for example invalid_setup_token.
func Problem(status int, ptype, detail string) error {
	return &huma.ErrorModel{Type: ptype, Title: http.StatusText(status), Status: status, Detail: detail}
}

// VerifyAllGuarded fails when any route on the mux was not registered through
// Register, Guarded or Exempt, or was Exempt without being on the D-47 list. The
// server calls it before listening, so an unguarded route stops startup (D-44).
// The error names every offender.
func VerifyAllGuarded(r *Router) error {
	known := map[string]RouteRecord{}
	for _, rec := range r.Routes() {
		known[rec.Method+" "+rec.Path] = rec
	}
	var problems []string
	walkErr := chi.Walk(r.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + route
		rec, ok := known[key]
		switch {
		case !ok:
			problems = append(problems, "unguarded route "+key+" (register it with httpx.Register or Router.Guarded)")
		case rec.Exempt && !exemptPaths[rec.Path]:
			problems = append(problems, "route "+key+" is exempt from the policy but is not on the D-47 list (/healthz, /readyz, /setup, /agent/connect)")
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("httpx: walk routes: %w", walkErr)
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New("httpx: " + strings.Join(problems, "; "))
}
