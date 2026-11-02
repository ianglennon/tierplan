// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import {
  appendPoint,
  gapAt,
  HISTORY_CAP,
  isGap,
  type StatsPoint,
  useStatsHistory,
} from "./use-stats-history";

const point = (t: number, cpu: number | null = 1): StatsPoint => ({
  t,
  cpu,
  mem: 100,
  netRx: 1,
  netTx: 2,
  blkRead: 3,
  blkWrite: 4,
});

describe("appendPoint", () => {
  it("keeps at most 150 points and drops the oldest first", () => {
    let history: StatsPoint[] = [];
    for (let t = 1; t <= 200; t++) history = appendPoint(history, point(t));
    expect(HISTORY_CAP).toBe(150);
    expect(history).toHaveLength(150);
    expect(history[0]?.t).toBe(51);
    expect(history[149]?.t).toBe(200);
  });

  it("does not change the history it is given", () => {
    const before = [point(1)];
    const after = appendPoint(before, point(2));
    expect(before).toHaveLength(1);
    expect(after).toHaveLength(2);
  });

  it("honours a smaller cap", () => {
    let history: StatsPoint[] = [];
    for (let t = 1; t <= 5; t++) history = appendPoint(history, point(t), 3);
    expect(history.map((p) => p.t)).toEqual([3, 4, 5]);
  });
});

describe("gapAt", () => {
  it("is a point with every value empty", () => {
    const gap = gapAt(7);
    expect(gap).toEqual({
      t: 7,
      cpu: null,
      mem: null,
      netRx: null,
      netTx: null,
      blkRead: null,
      blkWrite: null,
    });
    expect(isGap(gap)).toBe(true);
    expect(isGap(point(1))).toBe(false);
  });
});

describe("useStatsHistory", () => {
  it("collects points, caps them at 150 and is empty again for the next visit", () => {
    const first = renderHook(() => useStatsHistory());
    expect(first.result.current.points).toEqual([]);
    act(() => {
      for (let t = 1; t <= 160; t++) first.result.current.push(point(t));
    });
    expect(first.result.current.points).toHaveLength(150);
    expect(first.result.current.points[0]?.t).toBe(11);
    first.unmount();

    const second = renderHook(() => useStatsHistory());
    expect(second.result.current.points).toEqual([]);
  });

  it("marks one gap, not two, and none before the first sample", () => {
    const { result } = renderHook(() => useStatsHistory());
    act(() => result.current.markGap(1));
    expect(result.current.points).toEqual([]);
    act(() => result.current.push(point(2)));
    act(() => {
      result.current.markGap(3);
      result.current.markGap(4);
    });
    expect(result.current.points.map((p) => p.t)).toEqual([2, 3]);
    expect(isGap(result.current.points[1] as StatsPoint)).toBe(true);
    act(() => result.current.push(point(5)));
    expect(result.current.points.map((p) => p.t)).toEqual([2, 3, 5]);
  });

  it("clears", () => {
    const { result } = renderHook(() => useStatsHistory());
    act(() => result.current.push(point(1)));
    act(() => result.current.clear());
    expect(result.current.points).toEqual([]);
  });
});
