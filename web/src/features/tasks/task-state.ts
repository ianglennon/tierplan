// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { HealthState } from "@/api/environments";
import type { components } from "@/api/schema";

export type TaskItem = components["schemas"]["TaskItem"];
export type TaskSlot = components["schemas"]["TaskSlot"];

export interface TaskStateInfo {
  key: string;
  tone: HealthState;
  labelKey: string;
}

export function taskState(_state: string): TaskStateInfo {
  return { key: "unknown", tone: "unknown", labelKey: "tasks.state.unknown" };
}

export function isFailedTask(_task: Pick<TaskItem, "dockerState">): boolean {
  return false;
}

export function filterFailedOnly(slots: readonly TaskSlot[]): TaskSlot[] {
  return [...slots];
}
