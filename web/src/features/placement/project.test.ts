// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { node, placement, task } from "@/features/placement/fixtures";
import { NO_STACK, project } from "@/features/placement/project";

// The projection of a placement answer into columns (plan 06-11, RES-12, D-18, UI-SPEC
// "Placement views").

const NODES = [
  node({ id: "n-c", hostname: "wrk-c" }),
  node({ id: "n-a", hostname: "mgr-a" }),
  node({ id: "n-b", hostname: "wrk-b", state: "down" }),
];

describe("project", () => {
  it("makes one column per node in node-name order, empty ones too", () => {
    const cols = project(placement(NODES, []), {});
    expect(cols.map((c) => c.node?.hostname)).toEqual(["mgr-a", "wrk-b", "wrk-c"]);
    expect(cols.every((c) => c.kind === "node" && c.count === 0)).toBe(true);
  });

  it("puts Not placed first and Removed node last, and only when they hold tasks", () => {
    const none = project(placement(NODES, [task({ nodeId: "n-a" })]), {});
    expect(none.map((c) => c.kind)).toEqual(["node", "node", "node"]);

    const cols = project(
      placement(NODES, [
        task({ id: "pending", nodeId: "", state: "pending", error: "no suitable node" }),
        task({ id: "gone", nodeId: "node-gone", nodeKnown: false, state: "unknown" }),
        task({ id: "ok", nodeId: "n-c" }),
      ]),
      {},
    );
    expect(cols.map((c) => c.kind)).toEqual(["notPlaced", "node", "node", "node", "removed"]);
    expect(cols[0]?.groups[0]?.tasks[0]?.id).toBe("pending");
    expect(cols.at(-1)?.groups[0]?.tasks[0]?.id).toBe("gone");
  });

  it("keeps the tasks of a node that is down in its column", () => {
    const cols = project(
      placement(NODES, [
        task({ id: "orphan", nodeId: "n-b", state: "unknown", dockerState: "orphaned" }),
      ]),
      {},
    );
    const down = cols.find((c) => c.node?.id === "n-b");
    expect(down?.count).toBe(1);
    expect(down?.node?.state).toBe("down");
  });

  it("groups a column by stack A to Z, Not in a stack last, then by service and slot", () => {
    const cols = project(
      placement(NODES, [
        task({ id: "1", nodeId: "n-a", stack: "", service: "solo", slot: 1 }),
        task({ id: "2", nodeId: "n-a", stack: "shop", service: "web", slot: 2 }),
        task({ id: "3", nodeId: "n-a", stack: "blog", service: "db", slot: 1 }),
        task({ id: "4", nodeId: "n-a", stack: "shop", service: "web", slot: 1 }),
        task({ id: "5", nodeId: "n-a", stack: "shop", service: "api", slot: 3 }),
      ]),
      {},
    );
    const col = cols.find((c) => c.node?.id === "n-a");
    expect(col?.groups.map((g) => g.stack)).toEqual(["blog", "shop", ""]);
    expect(col?.groups[1]?.tasks.map((t) => t.id)).toEqual(["5", "4", "2"]);
    expect(col?.count).toBe(5);
  });

  it("filters by stack, by the token none and by part of a service name, keeping every node", () => {
    const data = placement(NODES, [
      task({ id: "1", nodeId: "n-a", stack: "shop", service: "web" }),
      task({ id: "2", nodeId: "n-c", stack: "blog", service: "Database" }),
      task({ id: "3", nodeId: "n-c", stack: "", service: "solo" }),
    ]);
    const ids = (f: Parameters<typeof project>[1]) =>
      project(data, f).flatMap((c) => c.groups.flatMap((g) => g.tasks.map((t) => t.id)));

    expect(ids({ stack: "shop" })).toEqual(["1"]);
    expect(ids({ stack: NO_STACK })).toEqual(["3"]);
    expect(ids({ q: "DATA" })).toEqual(["2"]);
    expect(ids({ stack: "blog", q: "web" })).toEqual([]);
    expect(project(data, { stack: "shop" })).toHaveLength(3);
  });

  it("drops Not placed when a filter leaves it nothing", () => {
    const data = placement(NODES, [task({ nodeId: "", service: "web", state: "pending" })]);
    expect(project(data, { q: "zzz" }).some((c) => c.kind === "notPlaced")).toBe(false);
  });
});
