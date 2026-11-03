// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { JanitorPolicy, JanitorPolicyFields, SchedulePreset } from "@/api/janitor";

// The policy card edits a copy of the policy it read (a Draft) and saves the whole of it. This file
// is the arithmetic of that: what the form shows for each field, what a save sends, whether anything
// changed, and whether the change could make the janitor remove more than it does now (UI-SPEC
// interpretation 7). Nothing here decides what is removable; that is the server's, in the preview.

export const KEEP_MIN = 0;
export const KEEP_MAX = 20;
export const GRACE_MIN = 1;
export const GRACE_MAX = 720;

export type FieldName =
  | "enabled"
  | "schedule"
  | "keepN"
  | "graceHours"
  | "removeUnusedNetworks"
  | "removeStoppedContainers";

// The schedule as the form holds it: the preset and what its inputs say. The cron it stands for is
// worked out from these by cronOf, and the cron text is only used under Custom.
export interface ScheduleDraft {
  preset: SchedulePreset;
  // "HH:MM", for Daily and Weekly.
  time: string;
  // The cron day of week for Weekly: 0 is Sunday.
  weekday: number;
  cron: string;
}

// What the person has changed. A field that is absent is as saved. null returns it to the instance
// default (Reset to default). A value overrides the default for this environment. The numbers are
// the text of their fields, because a half-typed number is still being edited.
export interface Draft {
  enabled?: boolean | null;
  schedule?: ScheduleDraft | null;
  keepN?: string | null;
  graceHours?: string | null;
  removeUnusedNetworks?: boolean | null;
  removeStoppedContainers?: boolean | null;
}

export interface FieldView<T> {
  value: T;
  // True when this environment sets the field itself; false when it follows the instance default.
  overridden: boolean;
}

export interface PolicyView {
  enabled: FieldView<boolean>;
  schedule: FieldView<ScheduleDraft>;
  keepN: FieldView<string>;
  graceHours: FieldView<string>;
  removeUnusedNetworks: FieldView<boolean>;
  removeStoppedContainers: FieldView<boolean>;
}

// The values that apply, as numbers and text, for comparing a saved policy with an edited one.
export interface PolicyValues {
  enabled: boolean;
  schedule: string;
  keepN: number;
  graceHours: number;
  removeUnusedNetworks: boolean;
  removeStoppedContainers: boolean;
}

export const DEFAULT_TIME = "03:00";
const HOURLY = "0 * * * *";
const EVERY_6H = "0 */6 * * *";
const DAILY_OR_WEEKLY = /^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-6])$/;
const TIME = /^(\d{2}):(\d{2})$/;

const pad = (n: number) => String(n).padStart(2, "0");

// cronOf is the five-field expression a schedule stands for, or "" when it is not complete (a time
// that was cleared, a custom expression left blank). The presets write the same expressions as the
// server's janitor.PresetCron.
export function cronOf(s: ScheduleDraft): string {
  switch (s.preset) {
    case "hourly":
      return HOURLY;
    case "every_6h":
      return EVERY_6H;
    case "custom":
      return s.cron.trim();
    default: {
      const m = TIME.exec(s.time);
      if (!m) return "";
      const at = `${Number(m[2])} ${Number(m[1])}`;
      return s.preset === "daily" ? `${at} * * *` : `${at} * * ${s.weekday}`;
    }
  }
}

// scheduleDraftOf reads a saved schedule into the form. The preset the server named is trusted only
// when the expression is the one that preset writes; anything else is shown as Custom, so the form
// never says "Daily" over an expression that is not.
export function scheduleDraftOf(cron: string, preset: SchedulePreset): ScheduleDraft {
  const custom: ScheduleDraft = { preset: "custom", time: DEFAULT_TIME, weekday: 1, cron };
  if (preset === "hourly" && cron === HOURLY) return { ...custom, preset };
  if (preset === "every_6h" && cron === EVERY_6H) return { ...custom, preset };
  const m = DAILY_OR_WEEKLY.exec(cron);
  if (m && (preset === "daily" || preset === "weekly")) {
    const time = `${pad(Number(m[2]))}:${pad(Number(m[1]))}`;
    if (preset === "daily" && m[3] === "*") return { ...custom, preset, time };
    if (preset === "weekly" && m[3] !== "*") {
      return { ...custom, preset, time, weekday: Number(m[3]) };
    }
  }
  return custom;
}

// scheduleChange is the schedule after the person picked a preset or changed one of its inputs. A
// switch to Custom starts from the expression the old choice wrote, so the person edits it and does
// not retype it.
export function scheduleChange(from: ScheduleDraft, patch: Partial<ScheduleDraft>): ScheduleDraft {
  const next = { ...from, ...patch };
  if (patch.preset === "custom" && from.preset !== "custom") {
    const written = cronOf(from);
    if (written) next.cron = written;
  }
  return next;
}

// viewOf is the policy as the form shows it: the saved value of each field, unless the draft changed
// it. A reset field shows the instance default and counts as not overridden.
export function viewOf(policy: JanitorPolicy, draft: Draft): PolicyView {
  const d = policy.defaults;
  function pick<T>(
    edit: T | null | undefined,
    saved: { value: T; overridden: boolean },
    fallback: T,
  ): FieldView<T> {
    if (edit === undefined) return { value: saved.value, overridden: saved.overridden };
    if (edit === null) return { value: fallback, overridden: false };
    return { value: edit, overridden: true };
  }
  const savedSchedule = {
    value: scheduleDraftOf(policy.schedule.value, policy.schedule.preset),
    overridden: policy.schedule.overridden,
  };
  return {
    enabled: pick(draft.enabled, policy.enabled, d.enabled),
    schedule: pick(draft.schedule, savedSchedule, scheduleDraftOf(d.schedule, d.schedulePreset)),
    keepN: pick(
      draft.keepN,
      { value: String(policy.keepN.value), overridden: policy.keepN.overridden },
      String(d.keepN),
    ),
    graceHours: pick(
      draft.graceHours,
      { value: String(policy.graceHours.value), overridden: policy.graceHours.overridden },
      String(d.graceHours),
    ),
    removeUnusedNetworks: pick(
      draft.removeUnusedNetworks,
      policy.removeUnusedNetworks,
      d.removeUnusedNetworks,
    ),
    removeStoppedContainers: pick(
      draft.removeStoppedContainers,
      policy.removeStoppedContainers,
      d.removeStoppedContainers,
    ),
  };
}

const sameSchedule = (a: ScheduleDraft, b: ScheduleDraft) =>
  a.preset === b.preset && cronOf(a) === cronOf(b);

// withEdit returns the draft with one field edited, dropping the edit again when it leaves the field
// as saved, so changing a switch twice, or typing the old number back, is no change at all.
// An edit to the value the field already shows while it follows the default is dropped too: it would
// only stop the field following the default, and nothing the person can see would differ.
export function withEdit(
  policy: JanitorPolicy,
  draft: Draft,
  field: FieldName,
  edit: Draft[FieldName],
): Draft {
  const next: Draft = { ...draft, [field]: edit };
  const saved = viewOf(policy, {});
  const now = viewOf(policy, next);
  const s = saved[field];
  const n = now[field];
  const unchanged =
    s.overridden === n.overridden &&
    (field === "schedule"
      ? sameSchedule(s.value as ScheduleDraft, n.value as ScheduleDraft)
      : String(s.value).trim() === String(n.value).trim());
  const followsSameValue =
    edit !== null &&
    !s.overridden &&
    (field === "schedule"
      ? sameSchedule(s.value as ScheduleDraft, n.value as ScheduleDraft)
      : String(s.value).trim() === String(n.value).trim());
  if (unchanged || followsSameValue) {
    const { [field]: _dropped, ...rest } = next;
    return rest;
  }
  return next;
}

export function isDirty(draft: Draft): boolean {
  return Object.keys(draft).length > 0;
}

// parseWhole reads the text of a number field: a whole number inside its range, or null.
export function parseWhole(text: string, min: number, max: number): number | null {
  if (!/^\d+$/.test(text.trim())) return null;
  const n = Number(text.trim());
  return n >= min && n <= max ? n : null;
}

// valuesOf is what applies in a view, as numbers. A field whose text is not a whole number in range
// is NaN, which is never "lower" than anything: the card does not compare or save such a form.
export function valuesOf(view: PolicyView): PolicyValues {
  return {
    enabled: view.enabled.value,
    schedule: cronOf(view.schedule.value),
    keepN: parseWhole(view.keepN.value, KEEP_MIN, KEEP_MAX) ?? Number.NaN,
    graceHours: parseWhole(view.graceHours.value, GRACE_MIN, GRACE_MAX) ?? Number.NaN,
    removeUnusedNetworks: view.removeUnusedNetworks.value,
    removeStoppedContainers: view.removeStoppedContainers.value,
  };
}

// couldRemoveMore is whether the edit could make the janitor remove something it would not remove
// now: turning it on, a lower number of containers kept, a shorter grace period, or an opt-in
// turned on. Narrowing and schedule-only edits cannot, so they save without a preview.
export function couldRemoveMore(saved: PolicyValues, edited: PolicyValues): boolean {
  return (
    (!saved.enabled && edited.enabled) ||
    edited.keepN < saved.keepN ||
    edited.graceHours < saved.graceHours ||
    (!saved.removeUnusedNetworks && edited.removeUnusedNetworks) ||
    (!saved.removeStoppedContainers && edited.removeStoppedContainers)
  );
}

// fieldsOf is what a save stores for a view, and what a preview of it is asked about: the value of
// each field this environment sets, and null for each that follows the instance default. The server
// takes a PUT as a full replace, so every field is sent.
export function fieldsOf(view: PolicyView): JanitorPolicyFields {
  const schedule = view.schedule.value;
  return {
    enabled: view.enabled.overridden ? view.enabled.value : null,
    schedule: view.schedule.overridden ? cronOf(schedule) : null,
    schedulePreset: view.schedule.overridden ? schedule.preset : null,
    keepN: view.keepN.overridden ? Number(view.keepN.value.trim()) : null,
    graceHours: view.graceHours.overridden ? Number(view.graceHours.value.trim()) : null,
    removeUnusedNetworks: view.removeUnusedNetworks.overridden
      ? view.removeUnusedNetworks.value
      : null,
    removeStoppedContainers: view.removeStoppedContainers.overridden
      ? view.removeStoppedContainers.value
      : null,
  };
}
