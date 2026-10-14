// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ServiceItem } from "@/api/services";

interface ForceUpdateDialogProps {
  envId: string;
  target: ServiceItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // True for Retry update: the same request with the wording of a retry.
  retry?: boolean;
}

// ForceUpdateDialog is "Force update {name}?": it replaces every task of a service again, after
// Bayplan has looked the image's tag up again.
export function ForceUpdateDialog(_props: ForceUpdateDialogProps) {
  return null;
}
