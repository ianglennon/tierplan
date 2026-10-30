// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { type TaskStatsRow, TaskStatsTable } from "@/features/stats/task-stats-table";
import "@/i18n";

// The per-task table of a service's Stats tab (plan 06-33, RES-11, D-29, UI-SPEC "Stats",
// E8 long-text and zero-one-many).

afterEach(cleanup);

const MIB = 1024 * 1024;

function task(extra: Partial<TaskStatsRow> = {}): TaskStatsRow {
  return {
    id: "abcdef0123456789",
    slot: 1,
    node: "wrk-1",
    cpu: 142.4,
    mem: 1.2 * 1024 * MIB,
    netRx: 1.2 * MIB,
    netTx: 340 * 1024,
    blkRead: 0,
    blkWrite: 12 * 1024,
    noData: false,
    trend: [10, 40, 142],
    ...extra,
  };
}

function renderTable(rows: TaskStatsRow[], serviceName = "web") {
  return render(
    <TooltipProvider>
      <TaskStatsTable serviceName={serviceName} rows={rows} />
    </TooltipProvider>,
  );
}

describe("TaskStatsTable", () => {
  it("is headed By task and names its columns", () => {
    renderTable([task()]);
    expect(screen.getByRole("heading", { name: "By task" })).toBeTruthy();
    const names = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(names).toEqual(["Task", "Node", "CPU", "Memory", "Network", "Block I/O", "Trend"]);
  });

  it("shows a task as the service and its slot with a short ID, on its node, with its numbers", () => {
    renderTable([task()]);
    const row = screen.getByRole("row", { name: /web\.1/ });
    expect(within(row).getByText("web.1")).toBeTruthy();
    expect(within(row).getByText("abcdef01")).toBeTruthy();
    expect(within(row).getByText("wrk-1")).toBeTruthy();
    expect(within(row).getByText("142%")).toBeTruthy();
    expect(within(row).getByText("1.2 GiB")).toBeTruthy();
    expect(within(row).getByText("In 1.2 MiB/s, out 340 KiB/s")).toBeTruthy();
    expect(within(row).getByText("Read 0 B/s, write 12 KiB/s")).toBeTruthy();
  });

  it("names a global service's task by the service alone, since the node has its own column", () => {
    renderTable([task({ slot: 0 })]);
    const row = screen.getAllByRole("row")[1] as HTMLElement;
    expect(within(row).getByText("web")).toBeTruthy();
    expect(within(row).queryByText("web.0")).toBeNull();
  });

  it("draws one sparkline of the CPU per task, hidden from assistive technology", () => {
    renderTable([task(), task({ id: "zzzz00001111", slot: 2 })]);
    const trends = document.querySelectorAll("svg[data-testid='sparkline']");
    expect(trends).toHaveLength(2);
    for (const t of trends) expect(t.getAttribute("aria-hidden")).toBe("true");
  });

  it("says No data for a task on a node that is not reporting, and why in a Tooltip", async () => {
    renderTable([task({ noData: true, cpu: null, mem: null, netRx: null, netTx: null })]);
    const row = screen.getByRole("row", { name: /web\.1/ });
    const cell = within(row).getByText("No data");
    expect(within(row).queryByText("142%")).toBeNull();
    act(() => cell.closest("button")?.focus());
    expect((await screen.findAllByText("wrk-1 isn't reporting right now.")).length).toBeGreaterThan(
      0,
    );
  });

  it("shows a dash for a task that has not answered yet, and never No data", () => {
    renderTable([
      task({ cpu: null, mem: null, netRx: null, netTx: null, blkRead: null, blkWrite: null }),
    ]);
    expect(screen.queryByText("No data")).toBeNull();
    expect(screen.queryByText(/NaN/)).toBeNull();
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });

  it("shows one row for one task and many for many, in the order it is given", () => {
    renderTable([task({ slot: 2, id: "bbbbbbbb1" }), task({ slot: 1, id: "aaaaaaaa1" })]);
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0] as HTMLElement).getByText("web.2")).toBeTruthy();
  });

  it("scrolls inside its own card when there are many tasks", () => {
    renderTable(Array.from({ length: 100 }, (_, i) => task({ slot: i + 1, id: `id-${i}-000000` })));
    const scroller = document.querySelector("[data-slot='task-stats-scroll']");
    expect(scroller).toBeTruthy();
    expect(scroller?.className).toMatch(/overflow-y-auto/);
    expect(scroller?.className).toMatch(/max-h-/);
    expect(screen.getAllByRole("row")).toHaveLength(101);
  });

  it("cuts a long node name with an ellipsis and keeps the whole of it for a Tooltip", () => {
    const long = "worker-node-with-a-very-long-name-that-goes-on.example.internal";
    renderTable([task({ node: long })]);
    const cell = screen.getByText(long);
    expect(cell.className).toMatch(/truncate/);
  });
});
