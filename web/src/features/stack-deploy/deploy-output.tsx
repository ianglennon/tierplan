// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackJobOutputLine } from "@/api/stacks";

interface DeployOutputProps {
  lines: readonly StackJobOutputLine[];
  // Whether the deploy failed: the panel opens by itself.
  failed: boolean;
}

// DeployOutput is the deploy's output log.
export function DeployOutput(_props: DeployOutputProps) {
  return null;
}
