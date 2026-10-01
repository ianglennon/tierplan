// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package api holds the typed HTTP operations, grouped by area.
package api

import (
	"context"
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

// RegisterSystem registers the unauthenticated system operations.
func RegisterSystem(r *httpx.Router) {
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
