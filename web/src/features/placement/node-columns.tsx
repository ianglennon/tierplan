// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { PlacementColumn } from "@/features/placement/project";

export interface NodeColumnsProps {
  envId: string;
  columns: readonly PlacementColumn[];
}

export function NodeColumns(_props: NodeColumnsProps) {
  return null;
}
