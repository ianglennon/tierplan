// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ServiceItem } from "@/api/services";

interface RollbackDialogProps {
  envId: string;
  target: ServiceItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// RollbackDialog is "Roll back {name}?": it shows what the service runs now against what it
// will run after the rollback, then rolls back.
export function RollbackDialog(_props: RollbackDialogProps) {
  return null;
}
