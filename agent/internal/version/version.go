// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package version holds the agent's version string. Releases set it with
//
//	-X github.com/ianglennon/bayplan/agent/internal/version.Version=v{{ .Version }}
//
// so the agent reports the same version as the server it ships with. The
// handshake sends it as agentVersion.
package version

// Version is "dev" in a plain build and "vX.Y.Z" in a release.
var Version = "dev"
