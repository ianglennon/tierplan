// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { node, placement, task } from "@/features/placement/fixtures";
import { NodeColumns } from "@/features/placement/node-columns";
import { type PlacementFilters, project } from "@/features/placement/project";
import "@/i18n";
import { TestRouter } from "@/test-router";

// The node columns of the Placement page (plan 06-11, RES-12, D-18, UI-SPEC "Placement
// views", E9).

afterEach(cleanup);

function renderColumns(data: ReturnType<typeof placement>, filters: PlacementFilters = {}) {
  return render(
    <TooltipProvider>
      <TestRouter>
        <NodeColumns envId="e1" columns={project(data, filters)} />
      </TestRouter>
    </TooltipProvider>,
  );
}

const MGR = node({ id: "n1", hostname: "mgr-1", leader: true });
const WRK = node({
  id: "n2",
  hostname: "wrk-1",
  role: "worker",
  cpus: 2,
  memoryBytes: 4 * 1024 ** 3,
});
const DOWN = node({ id: "n3", hostname: "wrk-down", role: "worker", state: "down" });

const column = (name: string) => screen.getByRole("region", { name });

describe("NodeColumns", () => {
  it("draws each node as a section with an h2 and its tasks as links named for the task", () => {
    renderColumns(
      placement(
        [MGR, WRK],
        [
          task({ id: "a", nodeId: "n1", service: "web", slot: 2, serviceId: "svc-web" }),
          task({
            id: "b",
            nodeId: "n1",
            service: "api",
            slot: 1,
            serviceId: "svc-api",
            state: "pending",
            dockerState: "pending",
          }),
        ],
      ),
    );
    expect(screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent)).toEqual([
      "mgr-1",
      "wrk-1",
    ]);
    const mgr = column("mgr-1");
    const link = within(mgr).getByRole("link", { name: "Open service web, slot 2, Running" });
    expect(link.getAttribute("href")).toBe("/env/e1/services/svc-web");
    expect(
      within(mgr).getByRole("link", { name: "Open service api, slot 1, Pending" }),
    ).toBeTruthy();
    // Tasks of one column are in one list per stack group, service names in order.
    expect(
      within(mgr)
        .getAllByRole("link")
        .map((l) => l.getAttribute("aria-label")),
    ).toEqual(["Open service api, slot 1, Pending", "Open service web, slot 2, Running"]);
  });

  it("names the role, the leader, the capacity and the task count of a node", () => {
    renderColumns(placement([MGR, WRK], [task({ nodeId: "n2" })]));
    const mgr = column("mgr-1");
    expect(within(mgr).getByText("Manager")).toBeTruthy();
    expect(within(mgr).getByText("Leader")).toBeTruthy();
    expect(within(mgr).getByText("4 CPUs · 8 GiB")).toBeTruthy();
    expect(within(mgr).getByText("0 tasks")).toBeTruthy();
    const wrk = column("wrk-1");
    expect(within(wrk).getByText("Worker")).toBeTruthy();
    expect(within(wrk).queryByText("Leader")).toBeNull();
    expect(within(wrk).getByText("2 CPUs · 4 GiB")).toBeTruthy();
    expect(within(wrk).getByText("1 task")).toBeTruthy();
  });

  it("links the name in a node's header to the node's page, and leaves the columns that are not nodes plain", () => {
    renderColumns(
      placement(
        [MGR, WRK],
        [task({ id: "p", nodeId: "" }), task({ id: "r", nodeId: "gone-id", nodeKnown: false })],
      ),
    );
    expect(within(column("mgr-1")).getByRole("link", { name: "mgr-1" }).getAttribute("href")).toBe(
      "/env/e1/nodes/n1",
    );
    expect(within(column("wrk-1")).getByRole("link", { name: "wrk-1" }).getAttribute("href")).toBe(
      "/env/e1/nodes/n2",
    );
    expect(within(column("Not placed")).queryByRole("link", { name: "Not placed" })).toBeNull();
    expect(within(column("Removed node")).queryByRole("link", { name: "Removed node" })).toBeNull();
  });

  it("keeps a node that is down as a muted column whose tasks read Unknown", () => {
    renderColumns(
      placement(
        [MGR, DOWN],
        [task({ id: "o", nodeId: "n3", state: "unknown", dockerState: "running" })],
      ),
    );
    const down = column("wrk-down");
    expect(down.getAttribute("data-muted")).toBe("true");
    expect(within(down).getByText("Down")).toBeTruthy();
    expect(
      within(down).getByRole("link", { name: "Open service web, slot 1, Unknown" }),
    ).toBeTruthy();
    expect(column("mgr-1").getAttribute("data-muted")).toBeNull();
  });

  it("says Disconnected for a node the swarm lost touch with", () => {
    renderColumns(placement([node({ id: "n4", hostname: "wrk-away", state: "disconnected" })], []));
    expect(within(column("wrk-away")).getByText("Disconnected")).toBeTruthy();
  });

  it("puts tasks without a node in a first Not placed column, with their error", () => {
    renderColumns(
      placement(
        [MGR],
        [
          task({
            id: "p",
            nodeId: "",
            state: "pending",
            dockerState: "pending",
            error: "no suitable node (scheduling constraints not satisfied on 1 node)",
          }),
        ],
      ),
    );
    const names = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(names).toEqual(["Not placed", "mgr-1"]);
    const col = column("Not placed");
    expect(
      within(col).getByRole("link", { name: "Open service web, slot 1, Pending" }),
    ).toBeTruthy();
    expect(within(col).getByText(/no suitable node/)).toBeTruthy();
  });

  it("puts the tasks of a node the swarm no longer lists in a last Removed node column", () => {
    renderColumns(
      placement(
        [MGR],
        [
          task({
            id: "g",
            nodeId: "node-gone",
            nodeKnown: false,
            state: "unknown",
            dockerState: "running",
          }),
        ],
      ),
    );
    const names = screen.getAllByRole("heading", { level: 2 }).map((h) => h.textContent);
    expect(names).toEqual(["mgr-1", "Removed node"]);
  });

  it("groups cards under stack sub-headers, Not in a stack last", () => {
    renderColumns(
      placement(
        [MGR],
        [
          task({ id: "1", stack: "", service: "solo", serviceId: "svc-solo" }),
          task({ id: "2", stack: "shop" }),
          task({ id: "3", stack: "blog", service: "db", serviceId: "svc-db" }),
        ],
      ),
    );
    const mgr = column("mgr-1");
    expect(
      within(mgr)
        .getAllByRole("heading", { level: 3 })
        .map((h) => h.textContent),
    ).toEqual(["blog1", "shop1", "Not in a stack1"]);
  });

  it("keeps the stack sub-headers sticky only in a column of more than 40 tasks", () => {
    const few = Array.from({ length: 40 }, (_, i) => task({ id: `f${i}`, slot: i + 1 }));
    const { unmount } = renderColumns(placement([MGR], few));
    expect(within(column("mgr-1")).getByRole("heading", { level: 3 }).className).not.toContain(
      "sticky",
    );
    unmount();
    const many = Array.from({ length: 41 }, (_, i) => task({ id: `m${i}`, slot: i + 1 }));
    renderColumns(placement([MGR], many));
    expect(within(column("mgr-1")).getByRole("heading", { level: 3 }).className).toContain(
      "sticky",
    );
  });

  it("truncates a long service name and a long node name", () => {
    const long = "a-very-long-service-name-that-goes-on-and-on-and-on-and-on-and-on";
    renderColumns(
      placement([node({ id: "n1", hostname: `${long}-node` })], [task({ service: long })]),
    );
    const link = screen.getByRole("link", { name: new RegExp(`Open service ${long}`) });
    expect(within(link).getByText(long).className).toContain("truncate");
    expect(screen.getByRole("heading", { level: 2 }).className).toContain("truncate");
  });

  it("says a column has no tasks when it has none", () => {
    renderColumns(placement([MGR], []));
    expect(within(column("mgr-1")).getByText("No tasks")).toBeTruthy();
  });

  it("shows the state text beside its colour, and a one-node swarm as one column", () => {
    renderColumns(placement([MGR], [task({ state: "failed", dockerState: "failed" })]));
    expect(screen.getAllByRole("region")).toHaveLength(1);
    expect(within(column("mgr-1")).getByText("Failed")).toBeTruthy();
  });
});
