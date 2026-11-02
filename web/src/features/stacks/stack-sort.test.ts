// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import type { StackRow } from "@/features/stacks/stack-columns";
import { sortStackRows } from "@/features/stacks/stack-sort";

const row = (name: string, state: StackRow["state"], over: Partial<StackRow> = {}): StackRow => ({
  id: name,
  name,
  target: "swarm",
  source: "inline",
  managed: true,
  state,
  lastDeployedAt: "",
  lastDeployedBy: "",
  hasDraft: false,
  envId: "e1",
  envName: "Prod",
  ...over,
});

// The same five stacks as the server's own sort test: the orders must agree.
const ROWS = [
  row("echo", "not_deployed"),
  row("alpha", "degraded", { lastDeployedAt: "2026-10-01T10:00:00Z" }),
  row("delta", "not_deployed"),
  row("bravo", "stopped", { lastDeployedAt: "2026-10-02T10:00:00Z" }),
  row("charlie", "deploy_failed"),
];
const names = (rows: StackRow[]) => rows.map((r) => r.name);

describe("sortStackRows", () => {
  it("sorts by name, both ways", () => {
    expect(names(sortStackRows(ROWS, "name", "asc"))).toEqual([
      "alpha",
      "bravo",
      "charlie",
      "delta",
      "echo",
    ]);
    expect(names(sortStackRows(ROWS, "name", "desc"))).toEqual([
      "echo",
      "delta",
      "charlie",
      "bravo",
      "alpha",
    ]);
  });

  it("sorts by the state priority and breaks a tie by name whichever way it runs", () => {
    expect(names(sortStackRows(ROWS, "state", "asc"))).toEqual([
      "bravo",
      "charlie",
      "alpha",
      "delta",
      "echo",
    ]);
    expect(names(sortStackRows(ROWS, "state", "desc"))).toEqual([
      "delta",
      "echo",
      "alpha",
      "charlie",
      "bravo",
    ]);
  });

  it("sorts by when a stack was last deployed and leaves a stack never deployed last either way", () => {
    expect(names(sortStackRows(ROWS, "lastDeployed", "asc"))).toEqual([
      "alpha",
      "bravo",
      "charlie",
      "delta",
      "echo",
    ]);
    expect(names(sortStackRows(ROWS, "lastDeployed", "desc"))).toEqual([
      "bravo",
      "alpha",
      "charlie",
      "delta",
      "echo",
    ]);
  });

  it("breaks a tie between environments by the environment name, then the ID", () => {
    const rows = [
      row("web", "running", { id: "2", envName: "Staging", envId: "e2" }),
      row("web", "running", { id: "1", envName: "Prod", envId: "e1" }),
    ];
    expect(sortStackRows(rows, "name", "asc").map((r) => r.envName)).toEqual(["Prod", "Staging"]);
    expect(sortStackRows(rows, "name", "desc").map((r) => r.envName)).toEqual(["Prod", "Staging"]);
  });

  it("does not change the rows it was given", () => {
    const before = names(ROWS);
    sortStackRows(ROWS, "state", "desc");
    expect(names(ROWS)).toEqual(before);
  });
});
