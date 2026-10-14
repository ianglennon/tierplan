// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ServiceItem } from "@/api/services";
import type { ServiceGroupBy, ServiceSortKey, SortDir } from "@/features/services/list-search";
import { type ServiceStateKey, serviceState } from "@/features/services/service-state";

export interface ServiceSort {
  key: ServiceSortKey;
  dir: SortDir;
}

export type ServiceGroupKind = "stack" | "ungrouped" | "all";

export interface ServiceGroup {
  // Unique among the groups.
  key: string;
  // The name on the header; empty for the one group of "none" and for rows that have no
  // stack, which the header shows as "Ungrouped".
  name: string;
  kind: ServiceGroupKind;
  // How many rows are doing well: running, scaled to 0 or completed.
  healthy: number;
  total: number;
  rows: ServiceItem[];
}

// How urgent a state is when sorting by state: what is doing well first, then what is
// changing, then what needs a look.
const STATE_RANK: Record<ServiceStateKey, number> = {
  running: 0,
  completed: 1,
  scaledToZero: 2,
  updating: 3,
  rollingBack: 4,
  degraded: 5,
  rolledBack: 6,
  notRunning: 7,
  updatePaused: 8,
  rollbackPaused: 9,
};

// The states a group header counts as healthy.
const HEALTHY: ReadonlySet<ServiceStateKey> = new Set(["running", "scaledToZero", "completed"]);

const isHealthy = (row: ServiceItem) => HEALTHY.has(serviceState(row).key);

// Case-insensitive first, so "Web" and "api" sort as a person reads them, then the exact
// text so the order never depends on how the rows arrived.
function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

const byName = (a: ServiceItem, b: ServiceItem) => byText(a.name, b.name) || byText(a.id, b.id);

// The time of a row's last update, or NaN for a row that has none.
const updatedOf = (row: ServiceItem) => (row.updatedAt ? Date.parse(row.updatedAt) : Number.NaN);

function compareBy(key: ServiceSortKey): (a: ServiceItem, b: ServiceItem) => number {
  switch (key) {
    case "state":
      return (a, b) =>
        STATE_RANK[serviceState(a).key] - STATE_RANK[serviceState(b).key] || byName(a, b);
    case "updated":
      return (a, b) => {
        // A row with no time sorts after every row that has one, whichever way the list runs.
        const x = updatedOf(a);
        const y = updatedOf(b);
        if (Number.isNaN(x) || Number.isNaN(y)) {
          return Number.isNaN(x) === Number.isNaN(y) ? byName(a, b) : Number.isNaN(x) ? 1 : -1;
        }
        return x - y || byName(a, b);
      };
    default:
      return byName;
  }
}

// sortServices returns the rows in the order the list shows them: Name ascending unless told
// otherwise, ties by ID so the order is the same on every refresh. A descending sort reverses
// the rows that have the value and still keeps the ones without it last.
export function sortServices(
  rows: readonly ServiceItem[],
  sort: ServiceSort = { key: "name", dir: "asc" },
): ServiceItem[] {
  const out = [...rows].sort(compareBy(sort.key));
  if (sort.dir === "desc") {
    if (sort.key === "updated") {
      const has = out.filter((r) => !Number.isNaN(updatedOf(r)));
      const lacks = out.filter((r) => Number.isNaN(updatedOf(r)));
      return [...has.reverse(), ...lacks];
    }
    return out.reverse();
  }
  return out;
}

// groupServices sorts the rows into the groups of the list: by stack, a row sits under its
// stack when it has one, else under Ungrouped. Groups are ordered by name without regard to
// case, with Ungrouped last. Within a group the rows follow the sort (Name ascending by
// default); pass null to keep the order the rows arrived in, which a frozen list needs so
// nothing moves. "none" is one group of every row.
export function groupServices(
  rows: readonly ServiceItem[],
  by: ServiceGroupBy,
  sort: ServiceSort | null = { key: "name", dir: "asc" },
): ServiceGroup[] {
  const ordered = sort ? sortServices(rows, sort) : [...rows];
  if (by === "none") {
    if (ordered.length === 0) return [];
    return [
      {
        key: "all",
        name: "",
        kind: "all",
        healthy: ordered.filter(isHealthy).length,
        total: ordered.length,
        rows: ordered,
      },
    ];
  }
  const groups = new Map<string, ServiceGroup>();
  for (const row of ordered) {
    const stack = row.stack;
    const key = stack ? `stack:${stack}` : "ungrouped";
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        name: stack,
        kind: stack ? "stack" : "ungrouped",
        healthy: 0,
        total: 0,
        rows: [],
      };
      groups.set(key, group);
    }
    group.rows.push(row);
    group.total += 1;
    if (isHealthy(row)) group.healthy += 1;
  }
  return [...groups.values()].sort((a, b) => {
    if (a.kind === "ungrouped" || b.kind === "ungrouped") {
      return a.kind === b.kind ? 0 : a.kind === "ungrouped" ? 1 : -1;
    }
    return byText(a.name, b.name);
  });
}
