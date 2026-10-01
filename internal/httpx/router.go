// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package httpx builds the HTTP router and owns the one way to register an API
// operation: Register, which cannot be called without declaring who may call
// the operation (D-44, RESEARCH Pattern 1).
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ianglennon/bayplan/internal/rbac"
)

// requirementKey is the huma operation metadata key holding the Requirement.
const requirementKey = "bayplan.requirement"

// apiVersion is the constant OpenAPI info version. It is deliberately not the
// build version: the committed openapi.json must not change on every commit
// (RESEARCH Pitfall 4).
const apiVersion = "1"

// contentSecurityPolicy is strict for scripts; only style-src is relaxed
// because Radix ScrollArea and sonner inject <style> elements (RESEARCH
// Pitfall 11, D-37).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self'; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-ancestors 'none'"

// RouteRecord is one registered operation and what it requires.
type RouteRecord struct {
	Method string
	Path   string
	Req    rbac.Requirement
	// Exempt marks a plain route mounted with Router.Exempt. It has no
	// Requirement and is not in the OpenAPI contract. The router-walk test
	// allows only the D-47 exempt paths.
	Exempt bool
}

// Options configures New.
type Options struct {
	// Fallback serves every non-/api/ path that no registered route matched
	// (the embedded SPA). Nil means a plain 404.
	Fallback http.Handler
}

// Router is the chi mux, the huma API and the registry of registered routes.
type Router struct {
	mux      *chi.Mux
	api      huma.API
	fallback http.Handler

	mu     sync.Mutex
	routes []RouteRecord
}

// New builds a Router with request IDs, panic recovery and security headers.
// It does not use chi's RealIP middleware, which trusts forwarded headers
// unconditionally.
func New(opts Options) *Router {
	r := &Router{mux: chi.NewRouter(), fallback: opts.Fallback}

	r.mux.Use(middleware.RequestID)
	r.mux.Use(middleware.Recoverer)
	r.mux.Use(securityHeaders)
	r.mux.NotFound(r.notFound)
	r.mux.MethodNotAllowed(r.methodNotAllowed)

	cfg := huma.DefaultConfig("Bayplan API", apiVersion)
	// No unauthenticated spec, docs or schema routes, and no $schema link
	// transformer: the contract is the committed openapi.json (Pitfall 4).
	cfg.OpenAPIPath = ""
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil
	r.api = humachi.New(r.mux, cfg)
	return r
}

// Register adds one typed operation. The Requirement is mandatory: a zero
// Requirement panics at startup. The policy middleware is attached to this
// operation only, never through a global UseMiddleware, because huma reads the
// global chain at registration time (Pitfall 5).
func Register[I, O any](r *Router, op huma.Operation, req rbac.Requirement, h func(context.Context, *I) (*O, error)) {
	req.MustBeValid()
	if op.Metadata == nil {
		op.Metadata = map[string]any{}
	}
	op.Metadata[requirementKey] = req
	op.Middlewares = append(huma.Middlewares{r.policy(req)}, op.Middlewares...)

	r.mu.Lock()
	r.routes = append(r.routes, RouteRecord{Method: op.Method, Path: op.Path, Req: req})
	r.mu.Unlock()

	huma.Register(r.api, op, h)
}

// Exempt mounts a plain chi route that is outside the policy seam and the
// OpenAPI contract: the health probes (D-47, D-86). It is recorded in the route
// registry flagged Exempt so the router-walk test can check it against the
// short allowed list. It still passes through the security-header middleware.
func (r *Router) Exempt(method, path string, h http.HandlerFunc) {
	r.mu.Lock()
	r.routes = append(r.routes, RouteRecord{Method: method, Path: path, Exempt: true})
	r.mu.Unlock()

	r.mux.MethodFunc(method, path, h)
}

// policy is the per-operation gate. Until the policy engine evaluates real
// principals, every non-Public requirement is denied (default deny).
func (r *Router) policy(req rbac.Requirement) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if req.Public {
			next(ctx)
			return
		}
		_ = huma.WriteErr(r.api, ctx, http.StatusForbidden, "forbidden")
	}
}

// Routes returns a copy of the registry of registered operations.
func (r *Router) Routes() []RouteRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RouteRecord(nil), r.routes...)
}

// Handler is the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.mux }

// API exposes the huma API, for emitting the OpenAPI document.
func (r *Router) API() huma.API { return r.api }

func (r *Router) notFound(w http.ResponseWriter, req *http.Request) {
	if isAPIPath(req.URL.Path) || isReservedPath(req.URL.Path) || r.fallback == nil {
		writeProblem(w, http.StatusNotFound, "not found")
		return
	}
	r.fallback.ServeHTTP(w, req)
}

func (r *Router) methodNotAllowed(w http.ResponseWriter, req *http.Request) {
	if isAPIPath(req.URL.Path) {
		writeProblem(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

func isAPIPath(p string) bool { return p == "/api" || strings.HasPrefix(p, "/api/") }

// isReservedPath names the paths huma would serve by default (spec, docs,
// schemas). They are never SPA routes: they answer 404 instead of falling
// through to index.html, so a client probing for an API description gets an
// honest answer (T-01-02).
func isReservedPath(p string) bool {
	return p == "/openapi" || strings.HasPrefix(p, "/openapi.") ||
		p == "/docs" || strings.HasPrefix(p, "/docs/") ||
		p == "/schemas" || strings.HasPrefix(p, "/schemas/")
}

// writeProblem writes an RFC 9457 problem body, the same shape huma emits.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	body, _ := json.Marshal(&huma.ErrorModel{
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, req)
	})
}
