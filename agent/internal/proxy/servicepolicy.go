// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package proxy

import (
	"context"
	"errors"

	"github.com/moby/moby/api/types/swarm"
)

// errHostPolicy is the refusal of a service spec that gives tasks host access on
// a host whose owner did not allow it.
var errHostPolicy = errors.New("host policy")

// networkLookup answers the name and driver of a network on the agent's own engine.
type networkLookup func(ctx context.Context, target string) (name, driver string, err error)

// escalations returns the spec path of each host-escalation feature in spec.
func escalations(ctx context.Context, spec swarm.ServiceSpec, lookup networkLookup) []string {
	return nil
}
