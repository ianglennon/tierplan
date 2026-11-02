// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";

describe("STATUS_CHIP_ROW", () => {
  // jsdom does no layout, so this guards the class string only. The e2e checks in
  // e2e/tests/helpers.ts (expectChipsInsideScreen) prove the chips wrap on a 375px screen.
  it("wraps the chips onto further rows and never turns the row into a sideways scroll strip (G-06-2)", () => {
    const classes = STATUS_CHIP_ROW.split(/\s+/);
    expect(classes).toContain("flex-wrap");
    expect(classes).toContain("max-w-full");
    expect(STATUS_CHIP_ROW).not.toMatch(/overflow-x-|flex-nowrap/);
  });
});
