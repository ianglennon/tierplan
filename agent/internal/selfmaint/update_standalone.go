// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"context"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/proto/wire"
)

// updateStandalone is replaced by the helper-container update in the next task.
func (m *Maint) updateStandalone(_ context.Context, _ *client.Client, _ container.InspectResponse, _ string) string {
	return wire.UpdateErrNotImplemented
}
