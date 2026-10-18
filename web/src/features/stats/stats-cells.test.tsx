// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import type { ListStat } from "@/features/stats/list-stats";
import { CpuCell, MemoryCell, NetworkCell } from "@/features/stats/stats-cells";
import "@/i18n";

// The CPU, Memory and Network cells of the services and containers lists (plan 06-33,
// RES-11, D-31, UI-SPEC "Services list" Stats cells).

afterEach(cleanup);

const MIB = 1024 * 1024;
const GIB = 1024 * MIB;

const stat = (extra: Partial<ListStat> = {}): ListStat => ({
  cpu: 142.4,
  mem: 1.2 * GIB,
  netRx: 1.2 * MIB,
  netTx: 340 * 1024,
  counted: 2,
  silent: 0,
  limitBytes: null,
  ...extra,
});

const wrap = (node: React.ReactNode) => render(<TooltipProvider>{node}</TooltipProvider>);

describe("CpuCell", () => {
  it("shows the percent of one core", () => {
    wrap(<CpuCell stat={stat()} />);
    expect(screen.getByText("142%")).toBeTruthy();
  });

  it("shows — while waiting and for nothing running", () => {
    const { unmount } = wrap(<CpuCell stat={undefined} />);
    expect(screen.getByText("—")).toBeTruthy();
    unmount();
    wrap(<CpuCell stat={stat({ cpu: null, mem: null, counted: 0 })} />);
    expect(screen.getByText("—")).toBeTruthy();
  });

  it("marks a total that leaves out tasks on silent nodes, with the count in a Tooltip", async () => {
    wrap(<CpuCell stat={stat({ silent: 2 })} />);
    const mark = screen.getByRole("img", {
      name: "Not counting 2 tasks on nodes that aren't reporting.",
    });
    act(() => mark.focus());
    expect(
      (await screen.findAllByText("Not counting 2 tasks on nodes that aren't reporting.")).length,
    ).toBeGreaterThan(0);
  });

  it("says Not counting 1 task for one, in the singular", () => {
    wrap(<CpuCell stat={stat({ silent: 1 })} />);
    expect(
      screen.getByRole("img", { name: "Not counting 1 task on a node that isn't reporting." }),
    ).toBeTruthy();
  });
});

describe("MemoryCell", () => {
  it("shows the memory in binary units", () => {
    wrap(<MemoryCell stat={stat()} />);
    expect(screen.getByText("1.2 GiB")).toBeTruthy();
  });

  it("puts the percent of the limit in a Tooltip when every task has one", async () => {
    wrap(<MemoryCell stat={stat({ limitBytes: 1 * GIB, counted: 2 })} />);
    act(() => screen.getByText("1.2 GiB").focus());
    // 1.2 GiB against two limits of 1 GiB is 60%.
    expect((await screen.findAllByText("60% of a 2 GiB limit")).length).toBeGreaterThan(0);
  });

  it("shows — while waiting", () => {
    wrap(<MemoryCell stat={undefined} />);
    expect(screen.getByText("—")).toBeTruthy();
  });
});

describe("NetworkCell", () => {
  it("shows the arrows for the eye and In and out in words for assistive technology", () => {
    wrap(<NetworkCell stat={stat()} />);
    expect(screen.getByText("↓ 1.2 MiB/s ↑ 340 KiB/s")).toBeTruthy();
    expect(screen.getByText("In 1.2 MiB/s, out 340 KiB/s")).toBeTruthy();
  });

  it("shows — for a rate the first sample could not tell", () => {
    wrap(<NetworkCell stat={stat({ netRx: null, netTx: null })} />);
    expect(screen.getByText("—")).toBeTruthy();
    expect(screen.queryByText(/NaN/)).toBeNull();
  });
});
