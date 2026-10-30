// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { EnvironmentItem, HealthState } from "@/api/environments";

export type SortKey = "name" | "health" | "nodes" | "failing" | "lastSeen";
export type SortDir = "asc" | "desc";

export const SORT_KEYS: readonly SortKey[] = ["name", "health", "nodes", "failing", "lastSeen"];

// SEVERITY is the "problems first" order, worst first (UI-SPEC Color, D-32).
// `unknown` is not in the UI-SPEC list: it only means the local socket has not
// been probed yet, which is no problem to act on, so it sits next to pending.
export const SEVERITY: readonly HealthState[] = [
  "offline",
  "revoked",
  "cluster_changed",
  "degraded",
  "unsupported",
  "reconnecting",
  "pending",
  "unknown",
  "online",
];

const RANK = new Map<HealthState, number>(SEVERITY.map((s, i) => [s, i]));

function rank(e: EnvironmentItem): number {
  return RANK.get(e.status.state) ?? SEVERITY.length;
}

// Names compare without regard to case; accents still count.
function byName(a: EnvironmentItem, b: EnvironmentItem): number {
  return a.name.localeCompare(b.name, undefined, { sensitivity: "accent" });
}

function byId(a: EnvironmentItem, b: EnvironmentItem): number {
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

// The order every sort ends with, so the result never depends on the order the
// list arrived in: name, then ID. Two environments with one name both stay.
function tie(a: EnvironmentItem, b: EnvironmentItem): number {
  return byName(a, b) || byId(a, b);
}

// The numbers a column sorts on, or undefined when the row has none. Rows
// with none go last whichever way the column is sorted.
function numbers(e: EnvironmentItem, key: SortKey): number[] | undefined {
  switch (key) {
    case "nodes":
      return e.nodes ? [e.nodes.ready, e.nodes.total] : undefined;
    case "failing":
      return e.failingServices ? [e.failingServices.failing, e.failingServices.total] : undefined;
    case "lastSeen": {
      const at = e.lastSeen ? Date.parse(e.lastSeen) : Number.NaN;
      return Number.isNaN(at) ? undefined : [at];
    }
    default:
      return undefined;
  }
}

// sortEnvironments returns the list in the order the overview shows it. With
// no column it is problems first, then name (the default view). Health sorts
// by severity: ascending is worst first. Every other direction flips only the
// column's own order; the ties, and rows with no value, keep their place.
export function sortEnvironments(
  items: readonly EnvironmentItem[],
  sort?: SortKey,
  dir: SortDir = "asc",
): EnvironmentItem[] {
  const sign = dir === "desc" ? -1 : 1;
  const out = [...items];
  if (sort === undefined) return out.sort((a, b) => rank(a) - rank(b) || tie(a, b));
  switch (sort) {
    case "name":
      return out.sort((a, b) => sign * byName(a, b) || byId(a, b));
    case "health":
      return out.sort((a, b) => sign * (rank(a) - rank(b)) || tie(a, b));
    default:
      return out.sort((a, b) => {
        const x = numbers(a, sort);
        const y = numbers(b, sort);
        if (x && y) {
          for (let i = 0; i < x.length; i++) {
            const d = (x[i] ?? 0) - (y[i] ?? 0);
            if (d !== 0) return sign * d;
          }
          return tie(a, b);
        }
        if (x) return -1;
        if (y) return 1;
        return tie(a, b);
      });
  }
}
