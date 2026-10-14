// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";

export type PlacementData = components["schemas"]["PlacementBody"];
export type PlacementNode = components["schemas"]["PlacementNode"];
export type PlacementTask = components["schemas"]["PlacementTask"];
export type PlacementService = components["schemas"]["PlacementService"];

// The value of the stack filter for the services that belong to no stack.
export const NO_STACK = "none";

export interface PlacementFilters {
  // A stack name, or NO_STACK; left out for all stacks.
  stack?: string;
  // A case-insensitive part of a service name.
  q?: string;
}

export type ColumnKind = "notPlaced" | "node" | "removed";

export interface StackGroup {
  // The stack's name; empty for the tasks of services that belong to no stack.
  stack: string;
  tasks: PlacementTask[];
}

export interface PlacementColumn {
  key: string;
  kind: ColumnKind;
  // The node of a "node" column.
  node?: PlacementNode;
  groups: StackGroup[];
  count: number;
}

export function project(_data: PlacementData, _filters: PlacementFilters): PlacementColumn[] {
  return [];
}
