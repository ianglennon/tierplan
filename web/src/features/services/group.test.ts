// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import type { ServiceItem } from "@/api/services";
import { groupServices, sortServices } from "@/features/services/group";

function row(name: string, extra: Partial<ServiceItem> = {}): ServiceItem {
  return {
    id: `id-${name}`,
    name,
    mode: "replicated",
    running: 1,
    desired: 1,
    updateState: "",
    image: "nginx:1",
    stack: "",
    tierplanStackId: "",
    agent: false,
    updatedAt: "2026-10-01T10:00:00Z",
    ports: [],
    updateMessage: "",
    hasPreviousSpec: false,
    createdAt: "2026-09-01T10:00:00Z",
    ...extra,
  };
}

const names = (rows: readonly ServiceItem[]) => rows.map((r) => r.name);

describe("groupServices by stack", () => {
  it("puts a row under its stack, else under Ungrouped", () => {
    const groups = groupServices(
      [row("a", { stack: "shop" }), row("b"), row("c", { stack: "blog" })],
      "stack",
    );
    expect(groups.map((g) => [g.name, g.kind, names(g.rows)])).toEqual([
      ["blog", "stack", ["c"]],
      ["shop", "stack", ["a"]],
      ["", "ungrouped", ["b"]],
    ]);
  });

  it("sorts stacks A to Z without regard to case and puts Ungrouped last", () => {
    const groups = groupServices(
      [
        row("u1"),
        row("a", { stack: "zeta" }),
        row("b", { stack: "Alpha" }),
        row("c", { stack: "beta" }),
      ],
      "stack",
    );
    expect(groups.map((g) => g.name)).toEqual(["Alpha", "beta", "zeta", ""]);
  });

  it("counts the healthy rows of each group: running, scaled to 0 or completed", () => {
    const groups = groupServices(
      [
        row("up", { stack: "s" }),
        row("zero", { stack: "s", running: 0, desired: 0 }),
        row("done", { stack: "s", mode: "replicated-job", running: 0, desired: 0 }),
        row("half", { stack: "s", running: 1, desired: 3 }),
        row("down", { stack: "s", running: 0, desired: 2 }),
        row("paused", { stack: "s", updateState: "paused" }),
        row("updating", { stack: "s", updateState: "updating" }),
      ],
      "stack",
    );
    expect(groups).toHaveLength(1);
    expect(groups[0]).toMatchObject({ healthy: 3, total: 7 });
  });

  it("sorts rows by name then ID inside a group, whatever order they arrive in", () => {
    const rows = [
      row("b", { stack: "s" }),
      row("B", { stack: "s", id: "id-B2" }),
      row("a", { stack: "s" }),
      row("B", { stack: "s", id: "id-B1" }),
    ];
    const one = groupServices(rows, "stack")[0]?.rows.map((r) => r.id);
    const two = groupServices([...rows].reverse(), "stack")[0]?.rows.map((r) => r.id);
    expect(one).toEqual(["id-a", "id-B1", "id-B2", "id-b"]);
    expect(two).toEqual(one);
  });

  it("keeps the order the rows arrived in when told not to sort", () => {
    const groups = groupServices(
      [row("b", { stack: "s" }), row("a", { stack: "s" })],
      "stack",
      null,
    );
    expect(names(groups[0]?.rows ?? [])).toEqual(["b", "a"]);
  });
});

describe("groupServices by none", () => {
  it("is one group of every row, with no header to show", () => {
    const groups = groupServices([row("b", { stack: "x" }), row("a")], "none");
    expect(groups).toHaveLength(1);
    expect(groups[0]).toMatchObject({ key: "all", kind: "all", total: 2, healthy: 2 });
    expect(names(groups[0]?.rows ?? [])).toEqual(["a", "b"]);
  });

  it("is no group at all for no rows", () => {
    expect(groupServices([], "none")).toEqual([]);
    expect(groupServices([], "stack")).toEqual([]);
  });
});

describe("sortServices", () => {
  it("sorts by name ascending by default, and descending on request", () => {
    const rows = [row("b"), row("c"), row("a")];
    expect(names(sortServices(rows))).toEqual(["a", "b", "c"]);
    expect(names(sortServices(rows, { key: "name", dir: "desc" }))).toEqual(["c", "b", "a"]);
  });

  it("sorts by state, the ones doing well first, ties by name", () => {
    const rows = [
      row("paused", { updateState: "paused" }),
      row("down", { running: 0, desired: 2 }),
      row("b-up"),
      row("a-up"),
      row("half", { running: 1, desired: 2 }),
      row("zero", { running: 0, desired: 0 }),
    ];
    expect(names(sortServices(rows, { key: "state", dir: "asc" }))).toEqual([
      "a-up",
      "b-up",
      "zero",
      "half",
      "down",
      "paused",
    ]);
  });

  it("sorts by updated time, a row without one last whichever way the list runs", () => {
    const rows = [
      row("old", { updatedAt: "2026-01-01T00:00:00Z" }),
      row("none", { updatedAt: "" }),
      row("new", { updatedAt: "2026-10-01T00:00:00Z" }),
    ];
    expect(names(sortServices(rows, { key: "updated", dir: "asc" }))).toEqual([
      "old",
      "new",
      "none",
    ]);
    expect(names(sortServices(rows, { key: "updated", dir: "desc" }))).toEqual([
      "new",
      "old",
      "none",
    ]);
  });

  it("does not change the rows it is given", () => {
    const rows = [row("b"), row("a")];
    sortServices(rows);
    expect(names(rows)).toEqual(["b", "a"]);
  });
});
