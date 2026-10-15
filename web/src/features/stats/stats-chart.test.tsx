// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { formatCpu, formatRate } from "@/features/stats/format";
import { Sparkline, sparklinePath } from "@/features/stats/sparkline";
import { StatsChart } from "@/features/stats/stats-chart";
import { gapAt, type StatsPoint } from "@/features/stats/use-stats-history";
import "@/i18n";

// The charts and the sparkline (plan 06-32, UI-SPEC "Charts and sparklines", "Charts").

afterEach(cleanup);

const point = (t: number, extra: Partial<StatsPoint> = {}): StatsPoint => ({
  t,
  cpu: 10,
  mem: 1000,
  netRx: 100,
  netTx: 50,
  blkRead: 0,
  blkWrite: 0,
  ...extra,
});

function renderChart(props: Partial<React.ComponentProps<typeof StatsChart>> = {}) {
  return render(
    <TooltipProvider>
      <StatsChart
        title="CPU"
        points={[point(1, { cpu: 171 }), point(2, { cpu: 142 })]}
        series={[{ key: "cpu", label: "CPU" }]}
        format={formatCpu}
        floor={100}
        {...props}
      />
    </TooltipProvider>,
  );
}

describe("StatsChart", () => {
  it("is an image named for what it shows now and its highest value in the last 5 minutes", () => {
    renderChart();
    const chart = screen.getByRole("img", {
      name: "CPU, now 142%, highest 171% in the last 5 minutes",
    });
    // 192 px tall (UI-SPEC "Charts").
    expect(chart.className).toContain("h-48");
  });

  it("names both series of a pair, and draws the second dashed in the legend", () => {
    renderChart({
      title: "Network in and out",
      series: [
        { key: "netRx", label: "In" },
        { key: "netTx", label: "Out", dashed: true },
      ],
      format: formatRate,
      floor: undefined,
    });
    expect(
      screen.getByRole("img", {
        name: "Network in and out, now In 100 B/s, Out 50 B/s, highest In 100 B/s, Out 50 B/s in the last 5 minutes",
      }),
    ).toBeTruthy();
    const legend = screen.getByRole("list", { name: "Network in and out" });
    const items = within(legend).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual(["In", "Out"]);
    expect(items[0]?.querySelector("[data-dashed]")).toBeNull();
    expect(items[1]?.querySelector("[data-dashed='true']")).not.toBeNull();
  });

  it("says it is waiting when there is no sample yet", () => {
    renderChart({ points: [] });
    expect(screen.getByRole("img", { name: "CPU, waiting for the first sample" })).toBeTruthy();
  });

  it("looks past a gap for the latest value and the highest", () => {
    renderChart({ points: [point(1, { cpu: 30 }), gapAt(2)] });
    expect(
      screen.getByRole("img", { name: "CPU, now 30%, highest 30% in the last 5 minutes" }),
    ).toBeTruthy();
  });

  it("marks the ends of the time axis", () => {
    renderChart();
    expect(screen.getByText("5 min ago")).toBeTruthy();
    expect(screen.getByText("now")).toBeTruthy();
  });

  it("names the memory limit as a dotted reference in the legend", () => {
    renderChart({
      title: "Memory",
      series: [{ key: "mem", label: "Memory" }],
      limit: 512 * 1024 * 1024,
      limitLabel: "Limit 512 MiB",
    });
    const item = screen.getByText("Limit 512 MiB").closest("li");
    expect(item?.querySelector("[data-dotted='true']")).not.toBeNull();
  });

  it("truncates a long legend label with an ellipsis and keeps the full name for a tooltip", () => {
    const long = "web.2@a-very-long-node-name-that-goes-on.example.internal";
    renderChart({ series: [{ key: "cpu", label: long }] });
    const label = screen.getByText(long);
    expect(label.className).toContain("truncate");
  });

  it("draws numbers with tabular figures", () => {
    renderChart();
    expect(screen.getByRole("img").parentElement?.className).toContain("tabular-nums");
  });
});

describe("Sparkline", () => {
  it("is a 96 by 24 path at 70% opacity that assistive technology skips", () => {
    const { container } = render(<Sparkline values={[1, 2, 3]} />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("aria-hidden")).toBe("true");
    expect(svg?.getAttribute("width")).toBe("96");
    expect(svg?.getAttribute("height")).toBe("24");
    expect(svg?.querySelector("path")?.getAttribute("opacity")).toBe("0.7");
  });

  it("puts the largest value at the top and the first and last at the edges", () => {
    expect(sparklinePath([0, 10], 96, 24)).toBe("M0.0 23.0 L96.0 1.0");
  });

  it("breaks the line at a null", () => {
    const d = sparklinePath([1, 2, null, 3, 4]);
    expect(d.match(/M/g)).toHaveLength(2);
  });

  it("is empty with no values, and a flat line at zero is the bottom edge", () => {
    expect(sparklinePath([])).toBe("");
    expect(sparklinePath([0, 0])).toBe("M0.0 23.0 L96.0 23.0");
  });
});
