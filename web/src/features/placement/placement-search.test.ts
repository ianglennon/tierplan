// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { describe, expect, it } from "vitest";

import { placementSearchSchema } from "@/features/placement/placement-search";

// The placement route's search (plan 06-11, UI-SPEC "Routes"): view, stack, q and stats.

describe("placementSearchSchema", () => {
  it("keeps a view, a stack, a search and the stats flag", () => {
    expect(
      placementSearchSchema.parse({ view: "grid", stack: "shop", q: "web", stats: 1 }),
    ).toEqual({
      view: "grid",
      stack: "shop",
      q: "web",
      stats: 1,
    });
    expect(placementSearchSchema.parse({ view: "nodes", stack: "none" })).toEqual({
      view: "nodes",
      stack: "none",
    });
  });

  it("is empty by default, which means the By node layout", () => {
    expect(placementSearchSchema.parse({})).toEqual({});
  });

  it("drops what does not fit instead of failing", () => {
    expect(
      placementSearchSchema.parse({ view: "cards", stack: "", q: "x".repeat(129), stats: 2 }),
    ).toEqual({});
    expect(placementSearchSchema.parse({ view: 3, stack: 7, q: [], stats: "1" })).toEqual({});
  });

  it("takes a search of exactly 128 characters", () => {
    const q = "x".repeat(128);
    expect(placementSearchSchema.parse({ q })).toEqual({ q });
  });
});
