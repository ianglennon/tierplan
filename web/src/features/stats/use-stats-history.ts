// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useCallback, useState } from "react";

// How many samples a series keeps: about five minutes at one every two seconds (D-30).
// The history lives in the open page only. Nothing is written to storage or sent back.
export const HISTORY_CAP = 150;

// One sample of a container, as the stream sends it. A value is null when it could not
// be told: the first sample has no CPU and no rates. A point whose every value is null
// and whose mem is null is a gap (the tab was hidden, or the stream dropped).
export interface StatsPoint {
  // The engine's read time in Unix milliseconds.
  t: number;
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
}

// appendPoint adds a point and keeps the newest `cap`; the oldest go first. It does not
// change its input.
export function appendPoint(
  history: StatsPoint[],
  point: StatsPoint,
  cap = HISTORY_CAP,
): StatsPoint[] {
  const next = history.length >= cap ? history.slice(history.length - cap + 1) : history.slice();
  next.push(point);
  return next;
}

// gapAt is the point that breaks a chart's lines: every value empty.
export function gapAt(t: number): StatsPoint {
  return { t, cpu: null, mem: null, netRx: null, netTx: null, blkRead: null, blkWrite: null };
}

export function isGap(p: StatsPoint): boolean {
  return p.mem === null;
}

export interface StatsHistory {
  points: StatsPoint[];
  push: (point: StatsPoint) => void;
  // Breaks the lines at a time. Two gaps in a row are one, and a gap before the first
  // sample is nothing.
  markGap: (t: number) => void;
  clear: () => void;
}

// useStatsHistory is the ring buffer of one stats surface: the last HISTORY_CAP points,
// in memory, gone when the component is. It is state, not a store, so leaving the page
// drops it and a new visit starts empty.
export function useStatsHistory(cap = HISTORY_CAP): StatsHistory {
  const [points, setPoints] = useState<StatsPoint[]>([]);
  const push = useCallback(
    (point: StatsPoint) => setPoints((p) => appendPoint(p, point, cap)),
    [cap],
  );
  const markGap = useCallback(
    (t: number) =>
      setPoints((p) => {
        const last = p[p.length - 1];
        return last === undefined || isGap(last) ? p : appendPoint(p, gapAt(t), cap);
      }),
    [cap],
  );
  const clear = useCallback(() => setPoints([]), []);
  return { points, push, markGap, clear };
}
