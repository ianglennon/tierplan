// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import type { ContainerRow } from "@/api/resources";
import { groupContainers, sortContainers } from "@/features/containers/group";

function row(name: string, extra: Partial<ContainerRow> = {}): ContainerRow {
  return {
    node: "node-1",
    nodeId: "n1",
    id: `id-${name}`,
    name,
    state: "running",
    status: "Up",
    image: "nginx",
    created: "2026-10-01T10:00:00Z",
    ports: [],
    isSelf: false,
    project: "",
    stack: "",
    tierplanStackId: "",
    service: "",
    health: "none",
    selfKind: "",
    readOnly: false,
    ...extra,
  };
}

describe("groupContainers by stack or project", () => {
  it("puts a row under its stack, else its project, else Ungrouped", () => {
    const groups = groupContainers(
      [
        row("a", { stack: "shop" }),
        row("b", { project: "blog" }),
        row("c"),
        // A swarm stack wins when a container somehow has both labels.
        row("d", { stack: "shop", project: "blog" }),
      ],
      "stack",
    );
    expect(groups.map((g) => [g.name, g.kind, g.rows.map((r) => r.name)])).toEqual([
      ["blog", "project", ["b"]],
      ["shop", "stack", ["a", "d"]],
      ["", "ungrouped", ["c"]],
    ]);
  });

  it("sorts groups by name without regard to case and puts Ungrouped last", () => {
    const groups = groupContainers(
      [
        row("1"),
        row("2", { project: "beta" }),
        row("3", { project: "Alpha" }),
        row("4", { stack: "zed" }),
      ],
      "stack",
    );
    expect(groups.map((g) => g.name)).toEqual(["Alpha", "beta", "zed", ""]);
  });

  it("sorts the rows in a group by name ascending, without regard to case", () => {
    const [group] = groupContainers(
      [row("web-2", { stack: "s" }), row("Web-1", { stack: "s" }), row("api", { stack: "s" })],
      "stack",
    );
    expect(group?.rows.map((r) => r.name)).toEqual(["api", "Web-1", "web-2"]);
  });

  it("counts the running rows and the rows of each group", () => {
    const [group] = groupContainers(
      [
        row("a", { stack: "s" }),
        row("b", { stack: "s", state: "exited" }),
        row("c", { stack: "s", state: "paused" }),
      ],
      "stack",
    );
    expect(group?.running).toBe(1);
    expect(group?.total).toBe(3);
  });

  it("keeps a stack and a project of the same name apart", () => {
    const groups = groupContainers([row("a", { stack: "x" }), row("b", { project: "x" })], "stack");
    expect(groups).toHaveLength(2);
    expect(new Set(groups.map((g) => g.key)).size).toBe(2);
  });
});

describe("groupContainers by node and none", () => {
  it("groups by node name, with a row that has no node under Ungrouped last", () => {
    const groups = groupContainers(
      [row("a", { node: "worker-2" }), row("b", { node: "Manager" }), row("c", { node: "" })],
      "node",
    );
    expect(groups.map((g) => [g.name, g.kind])).toEqual([
      ["Manager", "node"],
      ["worker-2", "node"],
      ["", "ungrouped"],
    ]);
  });

  it("returns one unnamed group for none, and none for no rows", () => {
    const groups = groupContainers([row("b"), row("a")], "none");
    expect(groups).toHaveLength(1);
    expect(groups[0]?.name).toBe("");
    expect(groups[0]?.kind).toBe("all");
    expect(groups[0]?.rows.map((r) => r.name)).toEqual(["a", "b"]);
    expect(groupContainers([], "none")).toEqual([]);
    expect(groupContainers([], "stack")).toEqual([]);
  });

  it("keeps the arrival order when the sort is null", () => {
    const [group] = groupContainers([row("b"), row("a")], "none", null);
    expect(group?.rows.map((r) => r.name)).toEqual(["b", "a"]);
  });
});

describe("sortContainers", () => {
  it("sorts by state with running first, and descending reverses it", () => {
    const rows = [
      row("a", { state: "exited" }),
      row("b", { state: "running" }),
      row("c", { state: "paused" }),
    ];
    expect(sortContainers(rows, { key: "state", dir: "asc" }).map((r) => r.name)).toEqual([
      "b",
      "c",
      "a",
    ]);
    expect(sortContainers(rows, { key: "state", dir: "desc" }).map((r) => r.name)).toEqual([
      "a",
      "c",
      "b",
    ]);
  });

  it("sorts by created and keeps a row with no created time last either way", () => {
    const rows = [
      row("none", { created: undefined }),
      row("new", { created: "2026-10-03T00:00:00Z" }),
      row("old", { created: "2026-10-01T00:00:00Z" }),
    ];
    expect(sortContainers(rows, { key: "created", dir: "asc" }).map((r) => r.name)).toEqual([
      "old",
      "new",
      "none",
    ]);
    expect(sortContainers(rows, { key: "created", dir: "desc" }).map((r) => r.name)).toEqual([
      "new",
      "old",
      "none",
    ]);
  });
});
