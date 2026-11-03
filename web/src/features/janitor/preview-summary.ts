// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { JanitorReason } from "@/api/janitor";

export type Translate = (key: string, options?: Record<string, unknown>) => string;

const BYTE_UNITS = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte", "petabyte"] as const;

// formatSize is a size the way the summary line shows it: "1.2 GB", one decimal at most, in the
// decimal units a disk is sold in. A size that is not known (null) or not a number is "—", never 0:
// an unknown estimate is not an empty one (UI-SPEC "Missing values").
export function formatSize(bytes: number | null | undefined, t: Translate): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes) || bytes < 0) {
    return t("janitor.size.unknown");
  }
  let value = bytes;
  let unit = 0;
  while (value >= 1000 && unit < BYTE_UNITS.length - 1) {
    value /= 1000;
    unit++;
  }
  return new Intl.NumberFormat(undefined, {
    style: "unit",
    unit: BYTE_UNITS[unit],
    unitDisplay: "short",
    maximumFractionDigits: unit === 0 ? 0 : 1,
  }).format(value);
}

// ageText is how long something has been stopped or idle, in the one largest unit that fits:
// minutes under two hours, hours under two days, days after that.
export function ageText(seconds: number, t: Translate): string {
  const minutes = Math.max(1, Math.floor(seconds / 60));
  if (minutes < 120) return t("janitor.age.minutes", { count: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return t("janitor.age.hours", { count: hours });
  return t("janitor.age.days", { count: Math.floor(hours / 24) });
}

// reasonText is the words for why an object is listed or left, from the stable code the server
// gave and its parameters. The client never works out a reason of its own. An empty code, which a
// run item has when the planner did not look at it, is an empty string, and the caller shows "—".
export function reasonText(reason: JanitorReason, t: Translate): string {
  if (!reason.code) return "";
  const global = reason.code === "task_beyond_keep" && !reason.slot;
  const key = global ? "task_beyond_keep_global" : reason.code;
  return t(`janitor.reason.${key}`, {
    rank: t("janitor.rank", { count: reason.rank ?? 1, ordinal: true }),
    service: reason.service ?? "",
    slot: reason.slot ?? "",
    node: reason.node ?? "",
    project: reason.project ?? "",
    stack: reason.stack ?? "",
    age: ageText(reason.ageSeconds ?? 0, t),
    time: ageText(reason.remainingSeconds ?? 0, t),
  });
}

// durationText is how long a run took: seconds under a minute, then ageText's units.
export function durationText(seconds: number, t: Translate): string {
  if (seconds < 60)
    return t("janitor.duration.seconds", { count: Math.max(0, Math.floor(seconds)) });
  return ageText(seconds, t);
}

export interface Counts {
  containers?: number;
  networks?: number;
  images?: number;
}

// countParts is "14 containers", "3 networks", "2 images" for the counts that are above zero, in
// that order. Zero parts are left out and the words follow the number (1 container, 2 containers).
export function countParts(counts: Counts, t: Translate): string[] {
  const out: string[] = [];
  for (const key of ["containers", "networks", "images"] as const) {
    const n = counts[key] ?? 0;
    if (n > 0) out.push(t(`janitor.counts.${key}`, { count: n }));
  }
  return out;
}

export interface SummaryCounts extends Counts {
  // The estimated bytes the ticked items hold, null when no size was known.
  estBytes?: number | null;
}

// summaryLine is "Would remove 14 containers, 3 networks, 2 images, ~1.2 GB": the parts that are
// above zero, with the words in the right number, and the estimated size last when one is known.
// With nothing to remove it is "Nothing selected", whatever the size.
export function summaryLine(counts: SummaryCounts, t: Translate): string {
  const parts = countParts(counts, t);
  if (parts.length === 0) return t("janitor.preview.summary.none");
  if (counts.estBytes !== null && counts.estBytes !== undefined) {
    parts.push(t("janitor.counts.size", { size: formatSize(counts.estBytes, t) }));
  }
  return t("janitor.preview.summary.line", { parts: parts.join(", ") });
}
