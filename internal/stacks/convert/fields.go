// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package convert

// FieldClass says what the converter does with one compose service field on a swarm.
type FieldClass int

const (
	// Converted fields are carried into the swarm service spec.
	Converted FieldClass = iota + 1
	// IgnoredOnSwarm fields have no swarm equivalent. docker stack deploy ignores them with a
	// warning, so the converter does too and lists them in ServicePlan.Ignored for the lint.
	IgnoredOnSwarm
	// Blocking fields cannot be deployed to a swarm at all: dropping one would change what the
	// stack does, so Convert refuses the service.
	Blocking
)

// FieldClasses classifies every field of compose-go's types.ServiceConfig, keyed by the compose
// YAML key.
var FieldClasses = map[string]FieldClass{}
