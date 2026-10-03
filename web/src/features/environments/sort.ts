// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentItem, HealthState } from "@/api/environments";

// RED stub.
export const SEVERITY: HealthState[] = [];
export type SortKey = "name" | "health" | "nodes" | "failing" | "lastSeen";
export type SortDir = "asc" | "desc";

export function sortEnvironments(
  items: EnvironmentItem[],
  _sort?: SortKey,
  _dir?: SortDir,
): EnvironmentItem[] {
  return items;
}
