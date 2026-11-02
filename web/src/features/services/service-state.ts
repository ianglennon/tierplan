// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HealthState } from "@/api/environments";
import type { ServiceItem } from "@/api/services";

// What a service's state is read from: its mode, the task counts and the state of its last
// update or rollback.
export type ServiceStateInput = Pick<ServiceItem, "mode" | "running" | "desired" | "updateState">;

export type ServiceStateKey =
  | "updatePaused"
  | "rollbackPaused"
  | "updating"
  | "rollingBack"
  | "rolledBack"
  | "notRunning"
  | "degraded"
  | "scaledToZero"
  | "completed"
  | "running";

export interface ServiceState {
  key: ServiceStateKey;
  // The status colour, as the StatusDot names it: online, degraded (the warning amber),
  // reconnecting (the pulsing amber), offline, or unknown (neutral).
  tone: HealthState;
  labelKey: `services.list.state.${ServiceStateKey}`;
}

function state(key: ServiceStateKey, tone: HealthState): ServiceState {
  return { key, tone, labelKey: `services.list.state.${key}` };
}

const isJob = (mode: ServiceItem["mode"]) => mode === "replicated-job" || mode === "global-job";

// serviceState is the one word a service row carries (UI-SPEC "Color"). The update state
// takes priority, in this order: update or rollback paused, updating or rolling back, rolled
// back; then the task counts: not running, degraded, scaled to 0, running. A job that has
// finished is Completed. The restart-loop badge is separate and never replaces the state.
export function serviceState(row: ServiceStateInput): ServiceState {
  switch (row.updateState) {
    case "paused":
      return state("updatePaused", "offline");
    case "rollback_paused":
      return state("rollbackPaused", "offline");
    case "updating":
      return state("updating", "reconnecting");
    case "rollback_started":
      return state("rollingBack", "reconnecting");
    case "rollback_completed":
      return state("rolledBack", "degraded");
    default:
      break;
  }
  if (isJob(row.mode)) {
    // A job has no desired count to hold: it runs to completion.
    return row.running > 0 ? state("running", "online") : state("completed", "unknown");
  }
  if (row.desired > 0 && row.running === 0) return state("notRunning", "offline");
  if (row.running > 0 && row.running < row.desired) return state("degraded", "degraded");
  if (row.desired === 0) return state("scaledToZero", "unknown");
  return state("running", "online");
}
