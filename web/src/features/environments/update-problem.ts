// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The problem types a refused update request answers with (409), each with a
// sentence of its own under envsettings.updateErrors. Anything else is general.
const KNOWN = new Set([
  "agent_offline",
  "no_manager_connected",
  "self_update_disabled",
  "agent_up_to_date",
  "update_in_progress",
]);

// updateProblemKey is the i18n key of what to tell the person about a failed
// update request, from the problem type the server gave.
export function updateProblemKey(type: string | undefined): string {
  return type && KNOWN.has(type)
    ? `envsettings.updateErrors.${type}`
    : "envsettings.updateErrors.failed";
}
