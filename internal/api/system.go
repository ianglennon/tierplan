// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package api holds the typed HTTP operations, grouped by area.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/buildinfo"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
)

// VersionBody is the public build identity (AGPL section 13, D-61, D-62).
type VersionBody struct {
	Version       string `json:"version" doc:"Release version, or dev for an untagged build"`
	Commit        string `json:"commit" doc:"Full source commit, empty when unknown"`
	Display       string `json:"display" doc:"Human readable version, for example v0.1.0 (abc1234)"`
	SourceURL     string `json:"sourceUrl" doc:"Root URL of the source code"`
	SourceTreeURL string `json:"sourceTreeUrl" doc:"URL of the exact source tree this build was made from"`
}

// VersionOutput wraps VersionBody for huma.
type VersionOutput struct {
	Body VersionBody
}

// SystemDeps are the dependencies of the system routes.
type SystemDeps struct {
	// Ready returns nil when the server can do useful work: the data directory
	// is prepared and writable (plan 06 adds the database). Nil means always ready.
	Ready func(context.Context) error
}

// RegisterSystem registers the unauthenticated system operations: the version
// operation, and the /healthz and /readyz probes as exempt plain routes.
func RegisterSystem(r *httpx.Router, deps SystemDeps) {
	r.Exempt(http.MethodGet, "/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeProbe(w, http.StatusOK, "ok")
	})
	r.Exempt(http.MethodGet, "/readyz", func(w http.ResponseWriter, req *http.Request) {
		if deps.Ready != nil {
			if err := deps.Ready(req.Context()); err != nil {
				// The reason goes to the log, never to the caller (D-86).
				slog.Warn("readiness check failed", "error", err)
				writeProbe(w, http.StatusServiceUnavailable, "not ready")
				return
			}
		}
		writeProbe(w, http.StatusOK, "ready")
	})

	httpx.Register(r, huma.Operation{
		OperationID: "getVersion",
		Method:      http.MethodGet,
		Path:        "/api/version",
		Summary:     "Get the running Bayplan version and source link",
		Tags:        []string{"system"},
	}, rbac.Public, func(_ context.Context, _ *struct{}) (*VersionOutput, error) {
		info := buildinfo.Get()
		return &VersionOutput{Body: VersionBody{
			Version:       info.Version,
			Commit:        info.Commit,
			Display:       info.Display,
			SourceURL:     info.SourceURL,
			SourceTreeURL: info.SourceTreeURL,
		}}, nil
	})
}

// writeProbe writes a fixed plain-text body: no version, path or error detail
// (D-86, T-01-16).
func writeProbe(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
