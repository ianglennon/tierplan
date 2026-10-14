// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { PlacementData, PlacementFilters } from "@/features/placement/project";

export interface NodeServiceGridProps {
  envId: string;
  data: PlacementData;
  filters: PlacementFilters;
  // Keep a row for every service the answer lists, also one with no task.
  keepServices?: boolean;
  onPopoverOpenChange?: (open: boolean) => void;
}

export function NodeServiceGrid(_props: NodeServiceGridProps) {
  return null;
}
