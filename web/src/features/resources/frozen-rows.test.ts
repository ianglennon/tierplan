// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { CHANGED_MS, LEAVE_MS, useFrozenRows } from "@/features/resources/frozen-rows";

interface Item {
  id: string;
  state: string;
}

const a: Item = { id: "a", state: "running" };
const b: Item = { id: "b", state: "running" };
const c: Item = { id: "c", state: "exited" };

const getKey = (i: Item) => i.id;
const fingerprint = (i: Item) => i.state;

interface Props {
  rows: readonly Item[];
  frozen?: boolean;
  ready?: boolean;
  resetKey?: string;
}

function setup(initial: Props) {
  return renderHook(
    ({ rows, frozen = false, ready = true, resetKey = "r" }: Props) =>
      useFrozenRows(rows, { frozen, getKey, fingerprint, ready, resetKey }),
    { initialProps: initial },
  );
}

const ids = (r: { rows: { row: Item }[] }) => r.rows.map((x) => x.row.id);
const flagged = (r: { rows: { row: Item; changed: boolean }[] }) =>
  r.rows.filter((x) => x.changed).map((x) => x.row.id);

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("useFrozenRows while not frozen", () => {
  it("returns the rows as they are, with nothing flagged on the first render", () => {
    const { result } = setup({ rows: [a, b] });
    expect(ids(result.current)).toEqual(["a", "b"]);
    expect(result.current.rows.every((r) => !r.changed && !r.added && !r.removed)).toBe(true);
    expect(result.current.heldCount).toBe(0);
  });

  it("flags a row whose fingerprint changed for 1500 ms, and only that row", () => {
    const { result, rerender } = setup({ rows: [a, b] });
    rerender({ rows: [{ ...a, state: "exited" }, b] });
    expect(flagged(result.current)).toEqual(["a"]);
    expect(result.current.rows[0]?.row.state).toBe("exited");
    act(() => {
      vi.advanceTimersByTime(CHANGED_MS - 100);
    });
    expect(flagged(result.current)).toEqual(["a"]);
    act(() => {
      vi.advanceTimersByTime(200);
    });
    expect(flagged(result.current)).toEqual([]);
  });

  it("flags a row that was not in the previous render as added", () => {
    const { result, rerender } = setup({ rows: [a] });
    rerender({ rows: [a, b] });
    expect(result.current.rows.map((r) => [r.row.id, r.added])).toEqual([
      ["a", false],
      ["b", true],
    ]);
  });

  it("keeps a removed row for the fade-out, flagged removed and leaving, then drops it", () => {
    const { result, rerender } = setup({ rows: [a, b, c] });
    rerender({ rows: [a, c] });
    expect(ids(result.current)).toEqual(["a", "b", "c"]);
    expect(result.current.rows[1]).toMatchObject({ removed: true, leaving: true });
    act(() => {
      vi.advanceTimersByTime(LEAVE_MS + 20);
    });
    expect(ids(result.current)).toEqual(["a", "c"]);
  });

  it("drops a removed row at once under prefers-reduced-motion", () => {
    const original = window.matchMedia;
    window.matchMedia = ((query: string) => ({
      matches: query.includes("prefers-reduced-motion"),
      media: query,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      addListener: () => undefined,
      removeListener: () => undefined,
      dispatchEvent: () => false,
      onchange: null,
    })) as typeof window.matchMedia;
    try {
      const { result, rerender } = setup({ rows: [a, b] });
      rerender({ rows: [a] });
      expect(ids(result.current)).toEqual(["a"]);
    } finally {
      window.matchMedia = original;
    }
  });

  it("starts fresh, with no flags, when the filters change or the data is not ready", () => {
    const { result, rerender } = setup({ rows: [a, b] });
    rerender({ rows: [c], resetKey: "other" });
    expect(ids(result.current)).toEqual(["c"]);
    expect(result.current.rows.every((r) => !r.added && !r.removed)).toBe(true);
    rerender({ rows: [c, a], ready: false, resetKey: "third" });
    expect(ids(result.current)).toEqual(["c", "a"]);
    rerender({ rows: [a, b], ready: true, resetKey: "third" });
    expect(result.current.rows.every((r) => !r.added && !r.changed)).toBe(true);
  });
});

describe("useFrozenRows while frozen", () => {
  it("keeps the order and the members: a changed row updates in place, a new row is held, a gone row stays removed", () => {
    const { result, rerender } = setup({ rows: [a, b, c] });
    rerender({ rows: [a, b, c], frozen: true });
    const d: Item = { id: "d", state: "running" };
    rerender({ rows: [d, { ...a, state: "paused" }, c], frozen: true });
    expect(ids(result.current)).toEqual(["a", "b", "c"]);
    expect(result.current.rows[0]?.row.state).toBe("paused");
    expect(result.current.rows[0]?.changed).toBe(true);
    expect(result.current.rows[1]).toMatchObject({ removed: true, leaving: false });
    expect(result.current.heldCount).toBe(1);
    expect(result.current.removedCount).toBe(1);
  });

  it("applies everything at once when the freeze ends", () => {
    const { result, rerender } = setup({ rows: [a, b] });
    const d: Item = { id: "d", state: "running" };
    rerender({ rows: [a, b], frozen: true });
    rerender({ rows: [d, a], frozen: true });
    expect(ids(result.current)).toEqual(["a", "b"]);
    rerender({ rows: [d, a], frozen: false });
    expect(ids(result.current)).toEqual(["d", "a", "b"]);
    expect(result.current.rows[0]?.added).toBe(true);
    expect(result.current.rows[2]).toMatchObject({ removed: true, leaving: true });
    expect(result.current.heldCount).toBe(0);
    act(() => {
      vi.advanceTimersByTime(LEAVE_MS + 20);
    });
    expect(ids(result.current)).toEqual(["d", "a"]);
  });

  it("applies the held changes on release, even while it is still frozen", () => {
    const { result, rerender } = setup({ rows: [a] });
    const d: Item = { id: "d", state: "running" };
    rerender({ rows: [a], frozen: true });
    rerender({ rows: [a, d], frozen: true });
    expect(ids(result.current)).toEqual(["a"]);
    expect(result.current.heldCount).toBe(1);
    act(() => result.current.release());
    expect(ids(result.current)).toEqual(["a", "d"]);
    expect(result.current.heldCount).toBe(0);
    // Still frozen: a later arrival is held again.
    const e: Item = { id: "e", state: "running" };
    rerender({ rows: [a, d, e], frozen: true });
    expect(ids(result.current)).toEqual(["a", "d"]);
    expect(result.current.heldCount).toBe(1);
  });
});
