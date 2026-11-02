// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HealthState } from "@/api/environments";
import type { components } from "@/api/schema";

export type TaskItem = components["schemas"]["TaskItem"];
export type TaskSlot = components["schemas"]["TaskSlot"];

export interface TaskStateInfo {
  // The state's own name, the last part of its label key.
  key: string;
  // The status colour, as the StatusDot names it: online, degraded (the warning amber),
  // reconnecting (the pulsing amber), offline, or unknown (neutral).
  tone: HealthState;
  labelKey: string;
}

// The colour of each state Docker gives a task (UI-SPEC "Color"): running is online; the
// states before it are the warning amber, pulsing while the task is being prepared or started;
// a task that ended well or was told to stop is neutral; one that failed is offline.
const TONES: Record<string, HealthState> = {
  running: "online",
  new: "degraded",
  pending: "degraded",
  assigned: "degraded",
  accepted: "degraded",
  ready: "degraded",
  preparing: "reconnecting",
  starting: "reconnecting",
  complete: "unknown",
  shutdown: "unknown",
  remove: "unknown",
  failed: "offline",
  rejected: "offline",
  orphaned: "offline",
  unknown: "unknown",
};

// taskState is the word and colour of a task state, Docker's own or the unknown that the
// server shows for a task on a node that cannot vouch for it. A word it does not know reads
// Unknown and is never taken for running.
export function taskState(state: string): TaskStateInfo {
  const key = Object.hasOwn(TONES, state) ? state : "unknown";
  return { key, tone: TONES[key] ?? "unknown", labelKey: `tasks.state.${key}` };
}

// isFailedTask says whether Docker recorded the task as failed or rejected. It reads Docker's
// own word, not the one shown: a task on a down node reads Unknown but is not failed. Never an
// exit code, which a rolling update sets to 137 on healthy tasks (RESEARCH E12).
export function isFailedTask(task: Pick<TaskItem, "dockerState">): boolean {
  return task.dockerState === "failed" || task.dockerState === "rejected";
}

// filterFailedOnly keeps the slots with at least one failed or rejected task, the current one
// or an earlier one, and trims each slot's history to its failed tasks. The slots it is given
// are not changed.
export function filterFailedOnly(slots: readonly TaskSlot[]): TaskSlot[] {
  return slots
    .filter((s) => isFailedTask(s.current) || (s.history ?? []).some(isFailedTask))
    .map((s) => ({ ...s, history: (s.history ?? []).filter(isFailedTask) }));
}
