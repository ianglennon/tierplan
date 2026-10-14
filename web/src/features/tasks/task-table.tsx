// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { TaskSlot } from "@/features/tasks/task-state";

export interface TaskTableProps {
  envId: string;
  serviceName?: string;
  slots: readonly TaskSlot[];
  retention: number;
  global: boolean;
  showFailedOnly: boolean;
  onShowFailedOnlyChange?: (value: boolean) => void;
  onScale?: () => void;
}

export function TaskTable(_props: TaskTableProps) {
  return null;
}
