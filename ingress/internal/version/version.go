// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package version holds the ingress supervisor's version string. Releases set it
// with
//
//	-X github.com/ianglennon/tierplan/ingress/internal/version.Version=v{{ .Version }}
//
// so the supervisor reports the same version as the server it ships with.
package version

// Version is "dev" in a plain build and "vX.Y.Z" in a release.
var Version = "dev"
