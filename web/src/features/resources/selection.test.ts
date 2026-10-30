// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { useSelection } from "@/features/resources/selection";

const KEYS = ["a", "b", "c", "d", "e"];

function setup(eligible: (k: string) => boolean = () => true, ordered: string[] = KEYS) {
  return renderHook(
    ({ ordered, eligible }) => useSelection({ orderedKeys: ordered, isEligible: eligible }),
    { initialProps: { ordered, eligible } },
  );
}

const picked = (r: { current: { selected: ReadonlySet<string> } }) =>
  [...r.current.selected].sort();

describe("useSelection", () => {
  it("toggles one key on and off", () => {
    const { result } = setup();
    act(() => result.current.toggle("b"));
    expect(picked(result)).toEqual(["b"]);
    act(() => result.current.toggle("b"));
    expect(picked(result)).toEqual([]);
  });

  it("selects the range between the last toggled key and this one on Shift, in the visible order", () => {
    const { result } = setup();
    act(() => result.current.toggle("b"));
    act(() => result.current.toggle("d", { shift: true }));
    expect(picked(result)).toEqual(["b", "c", "d"]);
    // And backwards.
    act(() => result.current.clear());
    act(() => result.current.toggle("d"));
    act(() => result.current.toggle("a", { shift: true }));
    expect(picked(result)).toEqual(["a", "b", "c", "d"]);
  });

  it("follows the order it is given, not the alphabet", () => {
    const { result } = setup(() => true, ["e", "a", "c", "b", "d"]);
    act(() => result.current.toggle("e"));
    act(() => result.current.toggle("b", { shift: true }));
    expect(picked(result)).toEqual(["a", "b", "c", "e"]);
  });

  it("never adds a key that is not eligible, in a range or alone", () => {
    const { result } = setup((k) => k !== "c");
    act(() => result.current.toggle("c"));
    expect(picked(result)).toEqual([]);
    act(() => result.current.toggle("a"));
    act(() => result.current.toggle("e", { shift: true }));
    expect(picked(result)).toEqual(["a", "b", "d", "e"]);
  });

  it("selects every eligible visible key, and clears them when all are selected", () => {
    const { result } = setup((k) => k !== "b");
    expect(result.current.eligibleCount).toBe(4);
    expect(result.current.allState).toBe("unchecked");
    act(() => result.current.selectAll());
    expect(picked(result)).toEqual(["a", "c", "d", "e"]);
    expect(result.current.allState).toBe("checked");
    act(() => result.current.selectAll());
    expect(picked(result)).toEqual([]);
  });

  it("reports a group as checked, mixed or unchecked, and selects its eligible keys", () => {
    const { result } = setup((k) => k !== "c");
    const group = ["b", "c", "d"];
    expect(result.current.groupState(group)).toBe("unchecked");
    act(() => result.current.toggle("b"));
    expect(result.current.groupState(group)).toBe("mixed");
    act(() => result.current.selectGroup(group));
    expect(picked(result)).toEqual(["b", "d"]);
    expect(result.current.groupState(group)).toBe("checked");
    act(() => result.current.selectGroup(group));
    expect(picked(result)).toEqual([]);
  });

  it("drops a key that stops being eligible", () => {
    const { result, rerender } = setup();
    act(() => result.current.toggle("a"));
    act(() => result.current.toggle("b"));
    rerender({ ordered: KEYS, eligible: (k: string) => k !== "a" });
    expect(picked(result)).toEqual(["b"]);
  });

  it("keeps a selected key whose group is folded away", () => {
    const { result, rerender } = setup();
    act(() => result.current.toggle("d"));
    rerender({ ordered: ["a", "b"], eligible: () => true });
    expect(picked(result)).toEqual(["d"]);
  });

  it("clears everything", () => {
    const { result } = setup();
    act(() => result.current.selectAll());
    act(() => result.current.clear());
    expect(picked(result)).toEqual([]);
  });
});
