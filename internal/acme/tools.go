// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

//go:build tools

// Keeps github.com/go-acme/lego/v5 in go.mod until Phase 14 imports it for real (go mod tidy removes
// a requirement nothing imports). Not part of any build: the tag is never set, so the binary, the
// licence scan and the notices are unchanged.

package acme

import (
	_ "github.com/go-acme/lego/v5/lego"
	_ "github.com/go-acme/lego/v5/providers/dns/cloudflare"
)
