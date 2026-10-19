// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stackops

import "context"

// cleanupCreated is filled in by the next commit.
func (d Deps) cleanupCreated(_ context.Context, _ string, _ Stack, _ *DeployResult, _ *progress) {}
