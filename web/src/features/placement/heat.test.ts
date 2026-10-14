// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { HEAT_CLASS, heatStep } from "@/features/placement/heat";

// The shading of the placement grid (plan 06-12, RES-12, D-18, UI-SPEC "Placement heat").

describe("heatStep", () => {
  it("is 0 for a count of zero, whatever the row holds", () => {
    expect(heatStep(0, 5)).toBe(0);
    expect(heatStep(0, 0)).toBe(0);
  });

  it("is 1 to 4 for the quarter of the row's maximum the count falls in", () => {
    const steps = [1, 2, 3, 4, 5, 6, 7, 8].map((n) => heatStep(n, 8));
    expect(steps).toEqual([1, 1, 2, 2, 3, 3, 4, 4]);
  });

  it("is the top step for the row's own maximum, also when that is 1", () => {
    expect(heatStep(1, 1)).toBe(4);
    expect(heatStep(5, 5)).toBe(4);
  });

  it("never leaves 1 to 4 for a count above the maximum or a maximum of zero", () => {
    expect(heatStep(9, 8)).toBe(4);
    expect(heatStep(3, 0)).toBe(4);
  });
});

describe("HEAT_CLASS", () => {
  it("tints 6%, 12%, 18% and 24% of the foreground, and leaves zero untinted", () => {
    expect(HEAT_CLASS[0]).toBe("");
    expect(HEAT_CLASS[1]).toContain("bg-foreground/6");
    expect(HEAT_CLASS[2]).toContain("bg-foreground/12");
    expect(HEAT_CLASS[3]).toContain("bg-foreground/18");
    expect(HEAT_CLASS[4]).toContain("bg-foreground/24");
  });
});
