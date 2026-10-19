// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackProblem } from "@/api/stacks";

interface BlockingListProps {
  problems: StackProblem[];
  envId: string;
  stackId: string;
}

// BlockingList is the alert listing the errors that stop a deploy (written in the next commit).
export function BlockingList(_props: BlockingListProps) {
  return null;
}
