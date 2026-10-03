// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Package facts reads what an agent reports about its own host from the Docker
// socket: who the engine is and what part it plays in a swarm.
package facts

import (
	"context"

	"github.com/ianglennon/bayplan/proto/wire"
)

// Collect describes the engine behind socketPath.
func Collect(_ context.Context, _ string) wire.Facts {
	return wire.Facts{}
}
