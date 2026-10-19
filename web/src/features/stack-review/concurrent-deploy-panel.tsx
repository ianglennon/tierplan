// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackConcurrent } from "@/api/stacks";

interface ConcurrentDeployPanelProps {
  // What was deployed after the draft was started, as the review answered it.
  concurrent: StackConcurrent;
  acknowledged: boolean;
  onAcknowledge: (acknowledged: boolean) => void;
}

// ConcurrentDeployPanel is the warning that someone deployed after the person started editing
// (STK-05, D-09), the dialog that shows what changed, and the checkbox that gates Deploy stack.
// The behaviour follows in the next commit of the plan.
export function ConcurrentDeployPanel(_props: ConcurrentDeployPanelProps) {
  return null;
}
