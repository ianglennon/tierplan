// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { meterPercent, meterState, UsageMeter } from "@/features/address-space/usage-meter";
import "@/i18n";

afterEach(cleanup);

function mount(props: Partial<Parameters<typeof UsageMeter>[0]> = {}) {
  return render(
    <TooltipProvider>
      <UsageMeter used={43} capacity={255} label="10.0.0.0/8" unit="subnets" {...props} />
    </TooltipProvider>,
  );
}

describe("meterState and meterPercent", () => {
  it("rounds down at the 70 and 90 thresholds", () => {
    // The UI-SPEC backstop: the state comes from the exact ratio and the percent is rounded down.
    const table: [number, number, string, number][] = [
      [6999, 10000, "normal", 69],
      [7000, 10000, "getting_full", 70],
      [8999, 10000, "getting_full", 89],
      [9000, 10000, "almost_full", 90],
      [9999, 10000, "almost_full", 99],
      [0, 10000, "normal", 0],
      [10000, 10000, "almost_full", 100],
    ];
    for (const [used, capacity, state, percent] of table) {
      expect(meterState(used, capacity), `${used}/${capacity}`).toBe(state);
      expect(meterPercent(used, capacity), `${used}/${capacity}`).toBe(percent);
    }
    // A 69.9% ratio shows 69 and is normal; a 70.0% one shows 70 and is amber.
    expect(meterState(699, 1000)).toBe("normal");
    expect(meterPercent(699, 1000)).toBe(69);
    expect(meterState(700, 1000)).toBe("getting_full");
    // 89.99% shows 89 and is amber; 90.0% shows 90 and is red.
    expect(meterState(8999, 10000)).toBe("getting_full");
    expect(meterPercent(8999, 10000)).toBe(89);
    expect(meterState(900, 1000)).toBe("almost_full");
    expect(meterPercent(900, 1000)).toBe(90);
  });

  it("is unknown for a capacity of zero", () => {
    expect(meterState(0, 0)).toBe("unknown");
    expect(meterPercent(0, 0)).toBe(0);
  });

  it("renders the thresholds as the words and fill the colours say", () => {
    mount({ used: 6999, capacity: 10000 });
    expect(screen.getByText("69%")).toBeTruthy();
    expect(screen.queryByText("Getting full")).toBeNull();
    cleanup();
    mount({ used: 7000, capacity: 10000 });
    expect(screen.getByText("70%")).toBeTruthy();
    expect(screen.getByText("Getting full")).toBeTruthy();
    cleanup();
    mount({ used: 8999, capacity: 10000 });
    expect(screen.getByText("89%")).toBeTruthy();
    expect(screen.getByText("Getting full")).toBeTruthy();
    cleanup();
    mount({ used: 9000, capacity: 10000 });
    expect(screen.getByText("90%")).toBeTruthy();
    expect(screen.getByText("Almost full")).toBeTruthy();
  });
});

describe("UsageMeter", () => {
  it("is a meter with its range, value and a spoken value", () => {
    mount();
    const meter = screen.getByRole("meter", { name: "10.0.0.0/8" });
    expect(meter.getAttribute("aria-valuemin")).toBe("0");
    expect(meter.getAttribute("aria-valuemax")).toBe("255");
    expect(meter.getAttribute("aria-valuenow")).toBe("43");
    expect(meter.getAttribute("aria-valuetext")).toBe("43 of 255 subnets used, 16 percent");
  });

  it("appends the state word to the spoken value from 70 percent", () => {
    mount({ used: 72, capacity: 100, unit: "addresses" });
    expect(screen.getByRole("meter").getAttribute("aria-valuetext")).toBe(
      "72 of 100 addresses used, 72 percent, getting full",
    );
    cleanup();
    mount({ used: 95, capacity: 100, unit: "addresses" });
    expect(screen.getByRole("meter").getAttribute("aria-valuetext")).toBe(
      "95 of 100 addresses used, 95 percent, almost full",
    );
  });

  it("says when the number is an estimate", () => {
    mount({ estimated: true });
    expect(screen.getByRole("meter").getAttribute("aria-valuetext")).toBe(
      "43 of 255 subnets used, 16 percent, estimated",
    );
  });

  it("is 8px high in the full variant and 6px high and 96px wide in the compact one", () => {
    mount();
    const full = screen.getByRole("meter");
    expect(full.className).toContain("h-2");
    expect(full.className).not.toContain("w-24");
    cleanup();
    mount({ variant: "compact" });
    const compact = screen.getByRole("meter");
    expect(compact.className).toContain("h-1.5");
    expect(compact.className).toContain("w-24");
  });

  it("draws the ticks at 70 and 90 percent in both variants", () => {
    for (const variant of ["full", "compact"] as const) {
      const { container } = mount({ variant });
      const lefts = Array.from(container.querySelectorAll<HTMLElement>("[aria-hidden=true][style]"))
        .map((e) => e.style.left)
        .filter(Boolean);
      expect(lefts).toEqual(["70%", "90%"]);
      cleanup();
    }
  });

  it("fills neutral below 70, amber to below 90 and red from 90", () => {
    const fill = (used: number) => {
      const { container } = mount({ used, capacity: 100 });
      const bar = container.querySelector<HTMLElement>("[role=meter] > div");
      cleanup();
      return bar?.className ?? "";
    };
    expect(fill(69)).toContain("bg-foreground/70");
    expect(fill(70)).toContain("bg-status-warning");
    expect(fill(89)).toContain("bg-status-warning");
    expect(fill(90)).toContain("bg-status-offline");
  });

  it("hides the state icon from assistive technology and keeps the word as text", () => {
    const { container } = mount({ used: 80, capacity: 100 });
    const icon = container.querySelector("svg");
    expect(icon?.getAttribute("aria-hidden")).toBe("true");
    expect(screen.getByText("Getting full").tagName).toBe("SPAN");
  });

  it("uses the server's state and percent when it gives them", () => {
    mount({ used: 1, capacity: 100, state: "almost_full", percent: 91 });
    expect(screen.getByText("91%")).toBeTruthy();
    expect(screen.getByText("Almost full")).toBeTruthy();
  });

  it("draws only the track, a dash and 'Not measured' when it is unknown", () => {
    const { container } = mount({ used: 0, capacity: 0, state: "unknown" });
    expect(screen.queryByRole("meter")).toBeNull();
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.getByText("Not measured")).toBeTruthy();
    expect(screen.queryByText("0%")).toBeNull();
    expect(container.querySelector("[aria-hidden=true].bg-muted")).toBeTruthy();
  });

  it("explains an unknown meter in a tooltip trigger when it is given a hint", () => {
    mount({ used: 0, capacity: 0, state: "unknown", notMeasuredHint: "Needs Engine 29." });
    const word = screen.getByText("Not measured");
    expect(word.getAttribute("tabindex")).toBe("0");
  });
});
