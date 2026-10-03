// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package agents

import (
	"context"

	"github.com/ianglennon/bayplan/internal/store/sqlc"
	"github.com/ianglennon/bayplan/proto/wire"
)

// Decision is what an accepted identity changes in the environment it enrols into.
type Decision struct {
	// EnvType, EngineID and ClusterID are bound to the environment when it has none yet.
	EnvType, EngineID, ClusterID string
	// Name is the automatic name for an environment still carrying the placeholder.
	Name string
	// PendingCluster is the cluster a manager reported when the refusal is
	// cluster_changed, and RecordChange says whether it is to be recorded.
	PendingCluster string
	RecordChange   bool
}

// CheckIdentity decides whether a host may enrol into an environment.
func CheckIdentity(_ context.Context, _ *sqlc.Queries, _ string, _ wire.Facts) (Decision, error) {
	return Decision{}, nil
}
