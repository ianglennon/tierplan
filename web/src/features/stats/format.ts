// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { formatBytes } from "@/lib/format";

// Number formatting for the stats views. Sizes and rates use binary units; a rate that
// is not a number is an em dash, never "NaN B/s".

const NO_VALUE = "—";

// formatRate is bytes per second: 1536 is "1.5 KiB/s".
export function formatRate(bytesPerSecond: number | null | undefined): string {
  if (bytesPerSecond === null || bytesPerSecond === undefined) return NO_VALUE;
  const size = formatBytes(bytesPerSecond);
  return size === "" ? NO_VALUE : `${size}/s`;
}

// formatCpu is percent of one core, whole: 142.4 is "142%". 100% is one core fully used.
export function formatCpu(percent: number | null | undefined): string {
  if (percent === null || percent === undefined || !Number.isFinite(percent) || percent < 0) {
    return NO_VALUE;
  }
  return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(percent)}%`;
}

// formatMemory is the usage in bytes.
export function formatMemory(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined) return NO_VALUE;
  return formatBytes(bytes) || NO_VALUE;
}

// percentOf is how much of a limit is used, whole, never above what a bar can show.
export function percentOf(used: number, limit: number): number {
  if (!(limit > 0) || !Number.isFinite(used) || used < 0) return 0;
  return Math.round((used / limit) * 100);
}
