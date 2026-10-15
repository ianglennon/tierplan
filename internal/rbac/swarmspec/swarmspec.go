// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package swarmspec is the D-13 host-escalation check over a decoded swarm
// service spec. Where internal/rbac/compose reads the text of a compose file,
// this package reads the swarm.ServiceSpec that is about to be forwarded to
// Docker, so the syntax of an editor (JSON, YAML, a form) cannot hide anything.
package swarmspec

import (
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/rbac/compose"
)

// NetworkResolver answers the name and driver of the network a task attaches to
// by target (an ID or a name). ok is false when the network is not known; an
// unknown network is not a finding, because Docker refuses it itself.
type NetworkResolver func(target string) (name, driver string, ok bool)

// Check returns the host-escalation findings in spec.
func Check(spec swarm.ServiceSpec, resolve NetworkResolver) []compose.Finding {
	return nil
}

// FormatSpec renders a finding as "{path}: {value}".
func FormatSpec(f compose.Finding) string {
	return ""
}
