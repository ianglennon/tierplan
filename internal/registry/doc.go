// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package registry owns image references, registry credentials, the resolver that
// chooses which credential an image uses, and the registry client. It never reads
// ~/.docker/config.json, a credential helper or a daemon setting: a credential is one
// Bayplan stored, chosen by Bayplan, and nothing on the host leaks into a pull
// (Phase 5, D-25).
package registry
