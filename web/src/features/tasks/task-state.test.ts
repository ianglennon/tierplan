// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import {
  filterFailedOnly,
  isFailedTask,
  type TaskItem,
  type TaskSlot,
  taskState,
} from "@/features/tasks/task-state";

// The words and colours of a task's state (plan 06-07, RES-04, UI-SPEC "Color" task rows).

describe("taskState", () => {
  it.each([
    ["running", "online", "running"],
    ["new", "degraded", "new"],
    ["pending", "degraded", "pending"],
    ["assigned", "degraded", "assigned"],
    ["accepted", "degraded", "accepted"],
    ["ready", "degraded", "ready"],
    ["preparing", "reconnecting", "preparing"],
    ["starting", "reconnecting", "starting"],
    ["complete", "unknown", "complete"],
    ["shutdown", "unknown", "shutdown"],
    ["remove", "unknown", "remove"],
    ["failed", "offline", "failed"],
    ["rejected", "offline", "rejected"],
    ["orphaned", "offline", "orphaned"],
    ["unknown", "unknown", "unknown"],
  ])("maps %s to the %s colour", (state, tone, key) => {
    expect(taskState(state)).toEqual({ key, tone, labelKey: `tasks.state.${key}` });
  });

  it("reads a state it does not know as unknown, never as running", () => {
    expect(taskState("hibernating")).toEqual({
      key: "unknown",
      tone: "unknown",
      labelKey: "tasks.state.unknown",
    });
  });
});

const item = (id: string, dockerState: string): TaskItem => ({
  id,
  slot: 1,
  nodeId: "n1",
  node: "mgr-1",
  nodeKnown: true,
  targetNodeId: "e1",
  state: dockerState,
  dockerState,
  desiredState: "running",
  error: "",
  message: "",
  exitCode: null,
  containerId: "",
  createdAt: "2026-10-14T09:00:00Z",
  updatedAt: "2026-10-14T09:00:00Z",
  stateSince: "2026-10-14T09:00:00Z",
});

const slot = (n: number, current: string, ...history: string[]): TaskSlot => ({
  slot: n,
  node: "mgr-1",
  current: item(`s${n}-cur`, current),
  history: history.map((h, i) => item(`s${n}-h${i}`, h)),
});

describe("isFailedTask", () => {
  it("counts Docker's failed and rejected, by Docker's word and not the shown one", () => {
    expect(isFailedTask({ dockerState: "failed" })).toBe(true);
    expect(isFailedTask({ dockerState: "rejected" })).toBe(true);
    for (const s of ["running", "shutdown", "complete", "orphaned", "remove"]) {
      expect(isFailedTask({ dockerState: s })).toBe(false);
    }
  });
});

describe("filterFailedOnly", () => {
  it("keeps the slots with a failed or rejected task, current or earlier, and trims their history to those", () => {
    const slots = [
      slot(1, "running", "failed", "shutdown", "rejected"),
      slot(2, "running", "shutdown"),
      slot(3, "failed"),
    ];
    const kept = filterFailedOnly(slots);
    expect(kept.map((s) => s.slot)).toEqual([1, 3]);
    expect(kept[0]?.history?.map((h) => h.dockerState)).toEqual(["failed", "rejected"]);
    expect(kept[1]?.current.dockerState).toBe("failed");
  });

  it("keeps a slot whose current task is the failed one even with no failed history", () => {
    expect(filterFailedOnly([slot(1, "rejected", "shutdown")])[0]?.history).toEqual([]);
  });

  it("does not change the slots it is given", () => {
    const slots = [slot(1, "running", "failed", "shutdown")];
    filterFailedOnly(slots);
    expect(slots[0]?.history).toHaveLength(2);
  });
});
