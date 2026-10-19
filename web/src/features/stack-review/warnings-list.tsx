// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackProblem } from "@/api/stacks";

interface WarningsListProps {
  problems: StackProblem[];
  envId: string;
  stackId: string;
}

// WarningsList is the collapsible card of warnings (written in the next commit).
export function WarningsList(_props: WarningsListProps) {
  return null;
}
