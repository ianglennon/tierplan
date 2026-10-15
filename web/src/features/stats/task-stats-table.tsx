// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Scaffold for the red step of plan 06-33.

export interface TaskStatsRow {
  id: string;
  slot: number;
  node: string;
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
  noData: boolean;
  trend: ReadonlyArray<number | null>;
}

export function TaskStatsTable(_props: { serviceName: string; rows: readonly TaskStatsRow[] }) {
  return null;
}
