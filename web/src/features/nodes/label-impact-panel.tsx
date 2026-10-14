// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { NodeLabelImpact } from "@/api/nodes";

export interface LabelImpactPanelProps {
  status: "idle" | "pending" | "ready" | "error";
  impact?: NodeLabelImpact;
  onRetry?: () => void;
}

export function LabelImpactPanel(_props: LabelImpactPanelProps) {
  return null;
}
