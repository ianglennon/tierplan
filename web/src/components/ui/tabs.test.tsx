// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Tabs, TabsList, TabsScroller, TabsTrigger } from "./tabs";

afterEach(cleanup);

describe("TabsScroller", () => {
  it("lets a tab bar scroll sideways inside it instead of widening the page", () => {
    render(
      <Tabs value="a">
        <TabsScroller>
          <TabsList variant="line" aria-label="Sections">
            <TabsTrigger value="a">Overview</TabsTrigger>
            <TabsTrigger value="b">Inspect</TabsTrigger>
          </TabsList>
        </TabsScroller>
      </Tabs>,
    );
    const scroller = screen.getByRole("tablist", { name: "Sections" }).parentElement;
    expect(scroller?.dataset.slot).toBe("tabs-scroller");
    const classes = [...(scroller?.classList ?? [])];
    // The padding keeps the 2px active indicator below the tabs inside the scrolling box,
    // and the negative margin keeps the spacing under the bar as it was.
    expect(classes).toEqual(
      expect.arrayContaining(["max-w-full", "min-w-0", "overflow-x-auto", "pb-2", "-mb-2"]),
    );
  });

  // At 360px the detail pages' bars (Overview … Inspect) are wider than the screen. A line
  // tab bar outside a dialog that is not held by TabsScroller widens the whole page.
  it("holds every line tab bar outside a dialog", () => {
    const sources = import.meta.glob(["/src/**/*.tsx", "!/src/**/*.test.tsx"], {
      query: "?raw",
      import: "default",
      eager: true,
    }) as Record<string, string>;
    const inDialog = new Set(["/src/features/stack-review/concurrent-deploy-panel.tsx"]);
    const offenders: string[] = [];
    let bars = 0;
    for (const [path, code] of Object.entries(sources)) {
      if (inDialog.has(path)) continue;
      const lines = code.split("\n");
      lines.forEach((line, i) => {
        if (!/<TabsList\b/.test(line)) return;
        const tag = lines.slice(i, i + 6).join(" ");
        if (!/variant="line"/.test(tag.slice(0, tag.indexOf(">") + 1 || undefined))) return;
        bars++;
        if (!/<TabsScroller\b/.test(lines[i - 1] ?? "")) offenders.push(`${path}:${i + 1}`);
      });
    }
    expect(bars).toBeGreaterThan(15);
    expect(offenders).toEqual([]);
  });
});
