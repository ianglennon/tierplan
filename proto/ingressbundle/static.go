// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	_ "embed"
	"slices"
)

//go:embed static/traefik.yml
var staticTraefik []byte

// StaticTraefik is the fixed static Traefik configuration, as a copy the caller may change. It is the
// one source of the file: the renderer's Static, the real-Traefik harness and the supervisor, which
// writes it to its state volume at every start (D-10), all read these bytes.
func StaticTraefik() []byte { return slices.Clone(staticTraefik) }
