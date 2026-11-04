// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { durationText } from "@/features/janitor/preview-summary";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// The failure codes the server names (UpdateInfoBody.reason). Anything else, or a code a newer
// server adds, reads as "other" rather than as the code itself.
const KNOWN: ReadonlySet<string> = new Set([
  "auth",
  "rate_limited",
  "unreachable",
  "not_found",
  "unsupported",
  "other",
]);

// reasonText is the sentence for a failed check (UI-SPEC "Check failure reasons"): TierPlan's
// own words for the typed code, with the registry, repository and tag the check was about. The
// registry's own message is never shown, and never reaches this function. Each sentence ends with
// what to do. The values are plain text; the caller renders the result as a text node.
export function reasonText(
  t: Translate,
  reason: string,
  registry: string,
  repository: string,
  tag: string,
): string {
  const code = KNOWN.has(reason) ? reason : "other";
  return t(`updates.reason.${code}`, {
    registry: registry || t("updates.reason.registryFallback"),
    repository: repository || t("updates.reason.repositoryFallback"),
    tag: tag || t("updates.reason.tagFallback"),
  });
}

// How long a wait for a service or a container may run when the job does not say: the server's
// converge timeout (ten minutes) for a service, and its window for a replacement container (five).
// A step that carries its own window in seconds is used instead.
export const DEFAULT_SERVICE_WINDOW_SECONDS = 600;
export const DEFAULT_CONTAINER_WINDOW_SECONDS = 300;

// The codes of a failed step, or of a remembered failure, that mean the same thing to a person. The
// step of a service says tasks_failed, update_stopped or network_full where the run says didnt_start,
// and a stack's wait says converge_timeout where an update's says timeout.
const REASON_ALIASES: Record<string, string> = {
  tasks_failed: "didnt_start",
  update_stopped: "didnt_start",
  network_full: "didnt_start",
  converge_timeout: "timeout",
};

const ROLLBACK_REASONS: ReadonlySet<string> = new Set([
  "didnt_start",
  "unhealthy",
  "timeout",
  "pull_failed",
  "server_restarted",
  "environment_offline",
  "create_refused",
  "agent_host_policy",
]);

// reasonCodeOf is the code of a failed update as the words know it: the aliases folded in, and a
// code this build has no words for read as "other" rather than as the code itself.
export function reasonCodeOf(code: string): string {
  const folded = REASON_ALIASES[code] ?? code;
  return ROLLBACK_REASONS.has(folded) ? folded : "other";
}

export interface RollbackReasonOptions {
  // Who the sentence is about: the workload's name in the console, "it" where the name is already
  // said. The reasons that are TierPlan's own ("TierPlan restarted during this update") name nobody.
  subject: string;
  // How long the wait was allowed to take, in seconds. Absent, the sentence says "in time".
  windowSeconds?: number;
}

// rollbackReasonText is why an update failed, as a clause that ends a sentence about the workload
// (UI-SPEC "Rollback reasons"): "web didn't start", "it didn't become healthy within 5 minutes",
// "TierPlan restarted during this update". It comes from our own table keyed by the code, and the
// engine's own message is never shown. A pull failure is worded by the callers, since nothing was
// changed and nothing was rolled back; it is here for the places that only name the cause.
export function rollbackReasonText(
  t: Translate,
  code: string,
  options: RollbackReasonOptions,
): string {
  const key = reasonCodeOf(code);
  // Only a wait has a window. Without one the sentence says "in time" rather than inventing a number.
  const waits = key === "unhealthy" || key === "timeout";
  const window =
    waits && options.windowSeconds !== undefined
      ? durationText(options.windowSeconds, t)
      : undefined;
  const suffix = waits && window === undefined ? "_noWindow" : "";
  return t(`updates.rollbackReason.${key}${suffix}`, {
    subject: options.subject,
    window: window ?? "",
  });
}

// restoreReasonText is why putting the previous version back failed, from the code of the restore
// step: restore_refused, restore_unsettled, or restore_failed for a container.
export function restoreReasonText(t: Translate, code: string): string {
  const known = ["restore_refused", "restore_unsettled", "restore_failed"];
  return t(`updates.restoreReason.${known.includes(code) ? code : "other"}`);
}
