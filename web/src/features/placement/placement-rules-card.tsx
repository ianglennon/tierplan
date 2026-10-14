// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { PlacementNode, PlacementService } from "@/features/placement/project";

export interface PlacementRulesCardProps {
  service: PlacementService;
  nodes: readonly PlacementNode[];
}

export function PlacementRulesCard(_props: PlacementRulesCardProps) {
  return null;
}
