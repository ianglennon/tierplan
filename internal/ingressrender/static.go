// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	_ "embed"
	"slices"
)

//go:embed static/traefik.yml
var staticTraefik []byte

// Static is the fixed static Traefik configuration, as a copy the caller may change. The proxy image
// ships these bytes and the real-Traefik harness starts with them.
func Static() []byte { return slices.Clone(staticTraefik) }
