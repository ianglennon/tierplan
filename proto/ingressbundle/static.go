// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

import (
	_ "embed"
	"slices"
)

//go:embed static/traefik.yml
var staticTraefik []byte

// accessLogBlock is appended to the base file when the access log is on (D-31). The log goes to stdout (no
// filePath) as JSON, and every request and response header is dropped by default, so the value of an
// Authorization or Cookie header never reaches the log (D-32, PROH-24-02). The client address and the path
// stay, and the documentation says so.
const accessLogBlock = `# Access log, switched on by TIERPLAN_INGRESS_ACCESS_LOG=1. JSON on stdout; no header is recorded.
accessLog:
  format: json
  fields:
    defaultMode: keep
    headers:
      defaultMode: drop
`

// StaticTraefik is the fixed static Traefik configuration, as a copy the caller may change. It is the
// one source of the file: the renderer's Static, the real-Traefik harness and the supervisor, which
// writes it to its state volume at every start (D-10), all read these bytes. It is the form with no
// access log, and equals StaticTraefikWith(false).
func StaticTraefik() []byte { return slices.Clone(staticTraefik) }

// StaticTraefikWith is the static file with the access log on when accessLog is true: the base file and
// one accessLog block with headers dropped. It returns a copy the caller may change. The supervisor writes
// it at every start, so the file on the volume always matches the binary and the environment (D-10, D-31).
func StaticTraefikWith(accessLog bool) []byte {
	if !accessLog {
		return StaticTraefik()
	}
	out := make([]byte, 0, len(staticTraefik)+len(accessLogBlock)+1)
	out = append(out, staticTraefik...)
	if n := len(out); n > 0 && out[n-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, accessLogBlock...)
}
