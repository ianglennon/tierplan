// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { node, placement, service, task } from "@/features/placement/fixtures";
import { NodeServiceGrid } from "@/features/placement/node-service-grid";
import type { PlacementData, PlacementFilters } from "@/features/placement/project";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The node x service grid of the Placement page (plan 06-12, RES-12, D-18, UI-SPEC
// "Placement", "Placement heat", E9).

afterEach(cleanup);

function renderGrid(
  data: PlacementData,
  filters: PlacementFilters = {},
  extra: { keepServices?: boolean; onPopoverOpenChange?: (open: boolean) => void } = {},
) {
  return render(
    <TooltipProvider>
      <TestRouter>
        <NodeServiceGrid envId="e1" data={data} filters={filters} {...extra} />
      </TestRouter>
    </TooltipProvider>,
  );
}

const NODES = [
  node({ id: "n1", hostname: "mgr-1" }),
  node({ id: "n2", hostname: "wrk-1", role: "worker" }),
  node({ id: "n3", hostname: "wrk-2", role: "worker" }),
];

const TASKS = [
  task({ id: "w1", serviceId: "svc-web", service: "web", slot: 1, nodeId: "n2" }),
  task({ id: "w2", serviceId: "svc-web", service: "web", slot: 2, nodeId: "n2" }),
  task({ id: "w3", serviceId: "svc-web", service: "web", slot: 3, nodeId: "n3" }),
  task({ id: "d1", serviceId: "svc-db", service: "db", slot: 1, nodeId: "n1" }),
  task({ id: "s1", serviceId: "svc-solo", service: "solo", stack: "", slot: 1, nodeId: "n1" }),
];

const DATA = placement(NODES, TASKS);

const cell = (name: string) => screen.getByRole("button", { name });

describe("NodeServiceGrid", () => {
  it("is a native table with column headers for the nodes and row headers for the services", () => {
    renderGrid(DATA);
    expect(screen.getByRole("table")).toBeTruthy();
    const names = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(names).toEqual(["Service", "mgr-12 tasks", "wrk-12 tasks", "wrk-21 task", "Total"]);
    for (const header of screen.getAllByRole("columnheader")) {
      expect(header.getAttribute("scope")).toBe("col");
    }
    const web = screen.getByRole("rowheader", { name: "web" });
    expect(web.getAttribute("scope")).toBe("row");
    expect(within(web).getByRole("link").getAttribute("href")).toBe("/env/e1/services/svc-web");
  });

  it("puts the services under stack bands, A to Z, with Not in a stack last", () => {
    renderGrid(DATA);
    const bands = screen
      .getAllByRole("rowheader")
      .filter((h) => h.getAttribute("scope") === "rowgroup");
    expect(bands.map((b) => b.textContent)).toEqual(["shop", "Not in a stack"]);
    const rows = screen
      .getAllByRole("rowheader")
      .filter((h) => h.getAttribute("scope") === "row")
      .map((h) => h.textContent);
    expect(rows).toEqual(["db", "web", "solo", "Total"]);
  });

  it("prints the task count in every cell, zero muted, and names each button for its cell", () => {
    renderGrid(DATA);
    expect(cell("web on wrk-1: 2 tasks").textContent).toContain("2");
    expect(cell("web on wrk-2: 1 task").textContent).toContain("1");
    const webRow = screen.getByRole("rowheader", { name: "web" }).closest("tr") as HTMLElement;
    const zero = within(webRow).getByText("0");
    expect(zero.className).toContain("text-muted-foreground");
    expect(within(webRow).queryByRole("button", { name: /mgr-1/ })).toBeNull();
  });

  it("totals each service and each node, and the whole", () => {
    renderGrid(DATA);
    const row = (name: string) =>
      screen.getByRole("rowheader", { name }).closest("tr") as HTMLElement;
    expect(within(row("web")).getAllByRole("cell").at(-1)?.textContent).toBe("3");
    expect(within(row("db")).getAllByRole("cell").at(-1)?.textContent).toBe("1");
    expect(
      within(row("Total"))
        .getAllByRole("cell")
        .map((c) => c.textContent),
    ).toEqual(["2", "2", "1", "5"]);
  });

  it("shades a cell by its share of the row's maximum, and prints the count whatever the shade", () => {
    renderGrid(DATA);
    expect(cell("web on wrk-1: 2 tasks").closest("td")?.getAttribute("data-heat")).toBe("4");
    expect(cell("web on wrk-2: 1 task").closest("td")?.getAttribute("data-heat")).toBe("2");
    const webRow = screen.getByRole("rowheader", { name: "web" }).closest("tr") as HTMLElement;
    expect(within(webRow).getByText("0").closest("td")?.getAttribute("data-heat")).toBe("0");
    expect(cell("web on wrk-1: 2 tasks").closest("td")?.className).toContain("bg-foreground/24");
  });

  it("has a Not placed column only when a task has no node, with the error in its popover", () => {
    const { unmount } = renderGrid(DATA);
    expect(screen.queryByRole("columnheader", { name: /Not placed/ })).toBeNull();
    unmount();
    renderGrid(
      placement(NODES, [
        ...TASKS,
        task({
          id: "p1",
          serviceId: "svc-web",
          service: "web",
          slot: 4,
          nodeId: "",
          state: "pending",
          dockerState: "pending",
          error: "no suitable node (scheduling constraints not satisfied on 3 nodes)",
        }),
      ]),
    );
    const headers = screen.getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual([
      "Service",
      "mgr-12 tasks",
      "wrk-12 tasks",
      "wrk-21 task",
      "Not placed1 task",
      "Total",
    ]);
    fireEvent.click(cell("web (not placed): 1 task"));
    expect(screen.getByText(/no suitable node/)).toBeTruthy();
  });

  it("marks a cell holding a task that is not running with a dot, and no other", () => {
    const { container } = renderGrid(
      placement(NODES, [
        ...TASKS,
        task({
          id: "w4",
          serviceId: "svc-web",
          service: "web",
          slot: 4,
          nodeId: "n3",
          state: "failed",
          dockerState: "failed",
        }),
        task({
          id: "d2",
          serviceId: "svc-db",
          service: "db",
          slot: 2,
          nodeId: "n1",
          state: "pending",
          dockerState: "pending",
        }),
      ]),
    );
    const dotted = container.querySelectorAll("[data-slot=cell-dot]");
    expect(dotted).toHaveLength(2);
    expect(
      cell("web on wrk-2: 2 tasks").querySelector("[data-slot=cell-dot]")?.className,
    ).toContain("bg-status-offline");
    expect(cell("db on mgr-1: 2 tasks").querySelector("[data-slot=cell-dot]")?.className).toContain(
      "bg-status-warning",
    );
    expect(cell("web on wrk-1: 2 tasks").querySelector("[data-slot=cell-dot]")).toBeNull();
  });

  it("opens a popover titled for the cell, listing each task with its state, linked to the service", () => {
    renderGrid(DATA);
    fireEvent.click(cell("web on wrk-1: 2 tasks"));
    const popover = screen.getByRole("dialog");
    expect(within(popover).getByText("web on wrk-1")).toBeTruthy();
    const links = within(popover).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["1 — Running", "2 — Running"]);
    expect(links[0]?.getAttribute("href")).toBe("/env/e1/services/svc-web");
  });

  it("tells the page when a popover opens and closes", () => {
    const onOpen = vi.fn();
    renderGrid(DATA, {}, { onPopoverOpenChange: onOpen });
    fireEvent.click(cell("web on wrk-1: 2 tasks"));
    expect(onOpen).toHaveBeenLastCalledWith(true);
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(onOpen).toHaveBeenLastCalledWith(false);
  });

  it("shows a global service as 1 in every node that runs it", () => {
    renderGrid(
      placement(NODES, [
        task({ id: "g1", serviceId: "svc-agent", service: "agent", slot: 0, nodeId: "n1" }),
        task({ id: "g2", serviceId: "svc-agent", service: "agent", slot: 0, nodeId: "n2" }),
        task({ id: "g3", serviceId: "svc-agent", service: "agent", slot: 0, nodeId: "n3" }),
      ]),
    );
    for (const n of ["mgr-1", "wrk-1", "wrk-2"]) {
      expect(cell(`agent on ${n}: 1 task`).textContent).toContain("1");
    }
    fireEvent.click(cell("agent on wrk-1: 1 task"));
    expect(within(screen.getByRole("dialog")).getByRole("link").textContent).toBe(
      "Global — Running",
    );
  });

  it("narrows the rows by the filters and never the nodes", () => {
    renderGrid(DATA, { stack: "shop", q: "web" });
    expect(screen.getAllByRole("columnheader")).toHaveLength(5);
    expect(screen.getByRole("rowheader", { name: "web" })).toBeTruthy();
    expect(screen.queryByRole("rowheader", { name: "db" })).toBeNull();
    expect(screen.queryByRole("rowheader", { name: "solo" })).toBeNull();
  });

  it("keeps a row for a service with no task when asked to, all zeros", () => {
    renderGrid(
      placement(NODES, [], { services: [service({ id: "svc-web", name: "web", stack: "shop" })] }),
      {},
      { keepServices: true },
    );
    const row = screen.getByRole("rowheader", { name: "web" }).closest("tr") as HTMLElement;
    expect(within(row).getAllByText("0")).toHaveLength(4);
    expect(within(row).queryByRole("button")).toBeNull();
  });

  it("explains the shading and the dot under the table", () => {
    renderGrid(DATA);
    expect(
      screen.getByText(
        "Shading shows how a service's tasks are spread across nodes. A dot marks a task that isn't running.",
      ),
    ).toBeTruthy();
  });

  it("draws a hostile service and node name as text, never as markup", () => {
    const evil = "<img src=x onerror=alert(1)>";
    const { container } = renderGrid(
      placement(
        [node({ id: "n1", hostname: evil })],
        [task({ id: "t1", service: evil, serviceId: "svc-evil", nodeId: "n1" })],
      ),
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).toContain(evil);
  });
});
