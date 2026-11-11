// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

//go:build tools

// The acme packages are linked for real from Phase 14 (client.go and the code that follows it), so the
// licence scan and the third-party notices cover them. This file only keeps github.com/go-acme/lego/v5/lego
// in go.mod until it is imported outside the tools tag (go mod tidy removes a requirement nothing imports).
// The Cloudflare DNS provider import is gone: TierPlan's own client (internal/acme/dns/cloudflare) replaces
// it. Not part of any build: the tag is never set.

package acme

import (
	_ "github.com/go-acme/lego/v5/lego"
)
