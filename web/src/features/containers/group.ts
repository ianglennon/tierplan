// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ContainerRow } from "@/api/resources";
import type { GroupBy, SortDir, SortKey } from "@/features/containers/list-search";

export interface SortSpec {
  key: SortKey;
  dir: SortDir;
}

export type GroupKind = "stack" | "project" | "node" | "ungrouped" | "all";

export interface ContainerGroup {
  // Unique among the groups: a stack and a project may share a name.
  key: string;
  // The name on the header; empty for the one group of "none" and for rows
  // that have no group, which the header shows as "Ungrouped".
  name: string;
  kind: GroupKind;
  running: number;
  total: number;
  rows: ContainerRow[];
}

// How urgent a state is when sorting by state: what runs first, then what is
// changing, then what has stopped.
const STATE_RANK: Record<ContainerRow["state"], number> = {
  running: 0,
  restarting: 1,
  paused: 2,
  removing: 3,
  created: 4,
  exited: 5,
  dead: 6,
};

// Case-insensitive first, so "Web" and "api" sort as a person reads them, then
// the exact text so the order never depends on how the rows arrived.
function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

const byName = (a: ContainerRow, b: ContainerRow) =>
  byText(a.name, b.name) || byText(a.node ?? "", b.node ?? "") || byText(a.id, b.id);

function compareBy(key: SortKey): (a: ContainerRow, b: ContainerRow) => number {
  switch (key) {
    case "state":
      return (a, b) => STATE_RANK[a.state] - STATE_RANK[b.state] || byName(a, b);
    case "created":
      return (a, b) => {
        // A row with no created time sorts after every row that has one,
        // whichever way the list runs.
        const x = a.created ? Date.parse(a.created) : Number.NaN;
        const y = b.created ? Date.parse(b.created) : Number.NaN;
        if (Number.isNaN(x) || Number.isNaN(y)) {
          return Number.isNaN(x) === Number.isNaN(y) ? byName(a, b) : Number.isNaN(x) ? 1 : -1;
        }
        return x - y || byName(a, b);
      };
    default:
      return byName;
  }
}

// sortContainers returns the rows in the order the list shows them: Name
// ascending unless told otherwise. A descending sort reverses the rows that
// have the value and still keeps the ones without it last.
export function sortContainers(
  rows: readonly ContainerRow[],
  sort: SortSpec = { key: "name", dir: "asc" },
): ContainerRow[] {
  const compare = compareBy(sort.key);
  const out = [...rows].sort(compare);
  if (sort.dir === "desc") {
    if (sort.key === "created") {
      const has = out.filter((r) => r.created && !Number.isNaN(Date.parse(r.created)));
      const lacks = out.filter((r) => !(r.created && !Number.isNaN(Date.parse(r.created))));
      return [...has.reverse(), ...lacks];
    }
    return out.reverse();
  }
  return out;
}

function keyOf(row: ContainerRow, by: GroupBy): { key: string; name: string; kind: GroupKind } {
  if (by === "node") {
    return row.node
      ? { key: `node:${row.node}`, name: row.node, kind: "node" }
      : { key: "ungrouped", name: "", kind: "ungrouped" };
  }
  if (row.stack) return { key: `stack:${row.stack}`, name: row.stack, kind: "stack" };
  if (row.project) return { key: `project:${row.project}`, name: row.project, kind: "project" };
  return { key: "ungrouped", name: "", kind: "ungrouped" };
}

// groupContainers sorts the rows into the groups of the list (D-01). By stack
// or project, a row sits under its swarm stack when it has one, else under its
// compose project, else under Ungrouped. Groups are ordered by name without
// regard to case, with Ungrouped last. Within a group the rows follow the sort
// (Name ascending by default); pass null to keep the order the rows arrived in,
// which a frozen list needs so nothing moves.
export function groupContainers(
  rows: readonly ContainerRow[],
  by: GroupBy,
  sort: SortSpec | null = { key: "name", dir: "asc" },
): ContainerGroup[] {
  const ordered = sort ? sortContainers(rows, sort) : [...rows];
  if (by === "none") {
    if (ordered.length === 0) return [];
    return [
      {
        key: "all",
        name: "",
        kind: "all",
        running: ordered.filter((r) => r.state === "running").length,
        total: ordered.length,
        rows: ordered,
      },
    ];
  }
  const groups = new Map<string, ContainerGroup>();
  for (const row of ordered) {
    const { key, name, kind } = keyOf(row, by);
    let group = groups.get(key);
    if (!group) {
      group = { key, name, kind, running: 0, total: 0, rows: [] };
      groups.set(key, group);
    }
    group.rows.push(row);
    group.total += 1;
    if (row.state === "running") group.running += 1;
  }
  return [...groups.values()].sort((a, b) => {
    if (a.kind === "ungrouped" || b.kind === "ungrouped") {
      return a.kind === b.kind ? 0 : a.kind === "ungrouped" ? 1 : -1;
    }
    return byText(a.name, b.name) || byText(a.kind, b.kind);
  });
}
