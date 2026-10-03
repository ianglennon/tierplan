// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentItem } from "@/api/environments";

interface AdoptClusterDialogProps {
  environment: Pick<EnvironmentItem, "id" | "name" | "pendingClusterId">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCloseAutoFocus?: (event: Event) => void;
}

// AdoptClusterDialog is "Adopt the new cluster?" (D-14).
export function AdoptClusterDialog(_props: AdoptClusterDialogProps) {
  return null;
}
