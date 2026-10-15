// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export const HISTORY_CAP = 150;

export interface StatsPoint {
  t: number;
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
}

export function appendPoint(
  history: StatsPoint[],
  _point: StatsPoint,
  _cap = HISTORY_CAP,
): StatsPoint[] {
  return history;
}

export function gapAt(t: number): StatsPoint {
  return { t, cpu: 0, mem: 0, netRx: 0, netTx: 0, blkRead: 0, blkWrite: 0 };
}

export function isGap(_p: StatsPoint): boolean {
  return false;
}

export interface StatsHistory {
  points: StatsPoint[];
  push: (point: StatsPoint) => void;
  markGap: (t: number) => void;
  clear: () => void;
}

export function useStatsHistory(_cap = HISTORY_CAP): StatsHistory {
  return { points: [], push: () => {}, markGap: () => {}, clear: () => {} };
}
