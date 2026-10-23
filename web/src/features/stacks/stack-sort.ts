// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackSortKey } from "@/features/stacks/list-search";
import type { StackRow } from "@/features/stacks/stack-columns";

// The order the State column sorts in, the priority of UI-SPEC "Color" and the server's own: an
// operation under way, then stopped, deploy failed, not running, degraded, running, and a stack never
// deployed last. A state Bayplan could not read sits with deploy failed.
const STATE_RANK: Record<StackRow["state"], number> = {
  removing: 0,
  deploying: 1,
  rolling_back: 1,
  starting: 1,
  stopping: 1,
  stopped: 2,
  deploy_failed: 3,
  unknown: 4,
  not_running: 5,
  degraded: 6,
  running: 7,
  not_deployed: 8,
};

// Case-insensitive first, so "Web" and "api" sort as a person reads them, then the exact text, so
// the order never depends on how the rows arrived.
function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

const timeOf = (row: StackRow) =>
  row.lastDeployedAt ? Date.parse(row.lastDeployedAt) : Number.NaN;

// sortStackRows orders rows the way the server orders one environment's list, for the All stacks list
// when it is not grouped by environment: the server sorts inside each environment, and one list needs
// the rows of all of them in a single order. A direction turns the key round and never the tie-break
// (name, then environment name, then ID); a stack never deployed comes after every deployed one
// whichever way the list runs.
export function sortStackRows(
  rows: readonly StackRow[],
  key: StackSortKey,
  dir: "asc" | "desc",
): StackRow[] {
  const sign = dir === "desc" ? -1 : 1;
  const tie = (a: StackRow, b: StackRow) =>
    byText(a.name, b.name) || byText(a.envName, b.envName) || byText(a.id, b.id);
  return [...rows].sort((a, b) => {
    let by = 0;
    if (key === "state") {
      by = sign * (STATE_RANK[a.state] - STATE_RANK[b.state]);
    } else if (key === "lastDeployed") {
      const x = timeOf(a);
      const y = timeOf(b);
      if (Number.isNaN(x) || Number.isNaN(y)) {
        if (Number.isNaN(x) !== Number.isNaN(y)) return Number.isNaN(x) ? 1 : -1;
      } else {
        by = sign * (x - y);
      }
    } else {
      by = sign * byText(a.name, b.name);
    }
    return by || tie(a, b);
  });
}
