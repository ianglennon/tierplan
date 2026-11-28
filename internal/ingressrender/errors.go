// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressrender

import (
	"fmt"
	"strconv"
)

// The codes a RefusedError carries. They are stable: the API and the logs key on them.
const (
	// CodeUnsafeValue: a stored value holds a backtick or a brace (D-08), or a stack or service name holds
	// a character compose does not allow there, which could change the backend URL (WR-02).
	CodeUnsafeValue = "ingress_render_unsafe_value"
	// CodeStripRoot: a route strips the root prefix, which would remove every path (D-17).
	CodeStripRoot = "ingress_render_strip_root"
	// CodeTemplateDelimiter: a rendered file holds two consecutive opening braces, which Traefik's
	// template engine would execute (D-08, correction 5).
	CodeTemplateDelimiter = "ingress_render_template_delimiter"
	// CodeNameCollision: two routers, services or middlewares would share a Traefik name (D-18).
	CodeNameCollision = "ingress_render_name_collision"
	// CodeSelfInput: the self input cannot be rendered: a base path is set (the self route is
	// host-rooted, Pitfall 9), or an attached input has no backend host, port in range, a blank ACME
	// host (D-12), or names a host and has no server name or trust certificate (D-14).
	CodeSelfInput = "ingress_render_self_input"
)

// CodeSchemaUnsupported is the code of an UnsupportedSchemaError (D-05).
const CodeSchemaUnsupported = "ingress_schema_unsupported"

// RefusedError is a render that was refused, with no bundle. It names the route and the field (or the
// file or name) that caused it and never holds certificate or key material. A refusal means the stored
// data is not what the parser would have let in, so the caller keeps the node's last good bundle.
type RefusedError struct {
	// Code is one of the Code constants above.
	Code string
	// Hostname and PathPrefix name the route; both are empty when the refusal is about a whole file.
	Hostname   string
	PathPrefix string
	// Field names what was refused: a route field, a file path, or a kind and name of a Traefik object.
	Field string
}

// Error names the route (hostname and prefix, quoted) and the field.
func (e *RefusedError) Error() string {
	if e.Hostname == "" && e.PathPrefix == "" {
		return fmt.Sprintf("ingressrender: refused (%s): %s", e.Code, e.Field)
	}
	return fmt.Sprintf("ingressrender: refused (%s): route %q %q: %s", e.Code, e.Hostname, e.PathPrefix, e.Field)
}

// UnsupportedSchemaError is returned when a bundle schema is asked for that this renderer cannot
// produce, or when a supervisor and TierPlan share no schema. Offered is what was asked for or
// reported; Supported is what TierPlan renders.
type UnsupportedSchemaError struct {
	Offered   []int
	Supported []int
}

// Error lists both sets of schema numbers.
func (e *UnsupportedSchemaError) Error() string {
	return "ingressrender: " + CodeSchemaUnsupported + ": offered " + intList(e.Offered) + ", supported " + intList(e.Supported)
}

func intList(v []int) string {
	out := "["
	for i, n := range v {
		if i > 0 {
			out += " "
		}
		out += strconv.Itoa(n)
	}
	return out + "]"
}
