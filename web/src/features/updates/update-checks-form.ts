// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type {
  UpdateCheckDefaults,
  UpdateCheckPolicyRequest,
  UpdateCheckStatus,
} from "@/api/updates";
import { cronOf, type ScheduleDraft, scheduleDraftOf } from "@/features/janitor/policy-diff";

// The environment card edits a copy of the status it read (a Draft) and saves the whole of it. This is
// the arithmetic of that, for the two settings an environment has: what the form shows for each, what
// a save sends and whether anything changed. It is the janitor's policy-diff cut down to the switch
// and the schedule.

export type FieldName = "enabled" | "schedule";

// What the person has changed. A field that is absent is as saved. null returns it to the instance
// default (Reset to default). A value overrides the default for this environment.
export interface Draft {
  enabled?: boolean | null;
  schedule?: ScheduleDraft | null;
}

export interface FieldView<T> {
  value: T;
  // True when this environment sets the field itself; false when it follows the instance default.
  overridden: boolean;
}

export interface View {
  enabled: FieldView<boolean>;
  schedule: FieldView<ScheduleDraft>;
}

// The instance defaults as far as the person may read them (settings.read). Without them a field
// that is reset keeps the value it shows until the save answers with the default that applies.
export type KnownDefaults = Pick<UpdateCheckDefaults, "enabled" | "schedule" | "preset">;

export const isDirty = (draft: Draft) => Object.keys(draft).length > 0;

// viewOf is the status as the form shows it: the saved value of each field, unless the draft changed
// it. A reset field shows the instance default and counts as not overridden.
export function viewOf(
  status: UpdateCheckStatus,
  defaults: KnownDefaults | undefined,
  draft: Draft,
): View {
  const savedSchedule = scheduleDraftOf(status.schedule.value, status.schedule.preset);
  const enabled: FieldView<boolean> =
    draft.enabled === undefined
      ? { value: status.enabled.value, overridden: status.enabled.overridden }
      : draft.enabled === null
        ? { value: defaults?.enabled ?? status.enabled.value, overridden: false }
        : { value: draft.enabled, overridden: true };
  const schedule: FieldView<ScheduleDraft> =
    draft.schedule === undefined
      ? { value: savedSchedule, overridden: status.schedule.overridden }
      : draft.schedule === null
        ? {
            value: defaults ? scheduleDraftOf(defaults.schedule, defaults.preset) : savedSchedule,
            overridden: false,
          }
        : { value: draft.schedule, overridden: true };
  return { enabled, schedule };
}

const sameSchedule = (a: ScheduleDraft, b: ScheduleDraft) =>
  a.preset === b.preset && cronOf(a) === cronOf(b);

function same(field: FieldName, a: FieldView<unknown>, b: FieldView<unknown>): boolean {
  return field === "schedule"
    ? sameSchedule(a.value as ScheduleDraft, b.value as ScheduleDraft)
    : a.value === b.value;
}

// withEdit returns the draft with one field edited, dropping the edit again when it leaves the field
// as saved, so changing the switch twice is no change at all. An edit to the value a field already
// shows while it follows the default is dropped too: it would only stop the field following the
// default, and nothing the person can see would differ.
export function withEdit(
  status: UpdateCheckStatus,
  defaults: KnownDefaults | undefined,
  draft: Draft,
  field: FieldName,
  edit: Draft[FieldName],
): Draft {
  const next: Draft = { ...draft, [field]: edit };
  const saved = viewOf(status, defaults, {})[field];
  const now = viewOf(status, defaults, next)[field];
  const unchanged = saved.overridden === now.overridden && same(field, saved, now);
  const followsSameValue = edit !== null && !saved.overridden && same(field, saved, now);
  if (unchanged || followsSameValue) {
    const { [field]: _dropped, ...rest } = next;
    return rest;
  }
  return next;
}

// fieldsOf is what a save stores for a view: the value of each field this environment sets, and null
// for each that follows the instance default. The server takes a PUT as a full replace.
export function fieldsOf(view: View, version: number): UpdateCheckPolicyRequest {
  const schedule = view.schedule.value;
  return {
    version,
    enabled: view.enabled.overridden ? view.enabled.value : null,
    schedule: view.schedule.overridden ? cronOf(schedule) : null,
    preset: view.schedule.overridden ? schedule.preset : null,
  };
}
