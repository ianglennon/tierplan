//go:build !devspa

// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package web embeds the built single-page app. dist/ is produced by
// `make web`; dist/.gitkeep is committed so a fresh clone still compiles
// (RESEARCH Pitfall 12). Release builds always use this file; the devspa build
// tag swaps it for embed_dev.go.
package web

import (
	"embed"
	"io/fs"
)

// Dist is the embedded build output, rooted at "dist".
//
//go:embed all:dist
var Dist embed.FS

// FS returns the dist sub-filesystem so callers see the site root.
func FS() fs.FS {
	sub, err := fs.Sub(Dist, "dist")
	if err != nil {
		// Unreachable: "dist" is always present because of the go:embed directive.
		panic(err)
	}
	return sub
}
