// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import {
  couldRemoveMore,
  cronOf,
  fieldsOf,
  isDirty,
  type PolicyValues,
  parseWhole,
  scheduleChange,
  scheduleDraftOf,
  valuesOf,
  viewOf,
  withEdit,
} from "@/features/janitor/policy-diff";
import { janitorPolicy } from "@/test-janitor";

const saved: PolicyValues = {
  enabled: true,
  schedule: "0 3 * * *",
  keepN: 2,
  graceHours: 24,
  removeUnusedNetworks: false,
  removeStoppedContainers: false,
};

describe("couldRemoveMore", () => {
  const cases: [string, Partial<PolicyValues>, Partial<PolicyValues>, boolean][] = [
    ["turning the janitor on", { enabled: false }, { enabled: true }, true],
    ["turning the janitor off", { enabled: true }, { enabled: false }, false],
    ["keeping fewer (2 to 1)", {}, { keepN: 1 }, true],
    ["keeping fewer (2 to 0)", {}, { keepN: 0 }, true],
    ["keeping more (2 to 3)", {}, { keepN: 3 }, false],
    ["a shorter grace period (24 to 12)", {}, { graceHours: 12 }, true],
    ["a longer grace period (24 to 48)", {}, { graceHours: 48 }, false],
    ["an opt-in turned on (networks)", {}, { removeUnusedNetworks: true }, true],
    ["an opt-in turned on (containers)", {}, { removeStoppedContainers: true }, true],
    [
      "an opt-in turned off",
      { removeUnusedNetworks: true },
      { removeUnusedNetworks: false },
      false,
    ],
    ["only the schedule", {}, { schedule: "0 */6 * * *" }, false],
    ["nothing", {}, {}, false],
  ];
  for (const [name, from, to, want] of cases) {
    it(`${want ? "could" : "could not"} remove more for ${name}`, () => {
      expect(couldRemoveMore({ ...saved, ...from }, { ...saved, ...from, ...to })).toBe(want);
    });
  }

  it("is true when anything widens, even if something else narrows", () => {
    expect(couldRemoveMore(saved, { ...saved, keepN: 1, graceHours: 48 })).toBe(true);
  });

  it("never reads a half-typed number as a lower one", () => {
    expect(couldRemoveMore(saved, { ...saved, keepN: Number.NaN, graceHours: Number.NaN })).toBe(
      false,
    );
  });
});

describe("parseWhole", () => {
  it("takes a whole number inside the range and nothing else", () => {
    expect(parseWhole("0", 0, 20)).toBe(0);
    expect(parseWhole(" 20 ", 0, 20)).toBe(20);
    expect(parseWhole("21", 0, 20)).toBeNull();
    expect(parseWhole("-1", 0, 20)).toBeNull();
    expect(parseWhole("1.5", 0, 20)).toBeNull();
    expect(parseWhole("", 0, 20)).toBeNull();
    expect(parseWhole("1e1", 0, 20)).toBeNull();
    expect(parseWhole("0", 1, 720)).toBeNull();
  });
});

describe("schedule drafts", () => {
  it("writes each preset as the cron the server writes", () => {
    const base = { time: "03:00", weekday: 1, cron: "" };
    expect(cronOf({ ...base, preset: "hourly" })).toBe("0 * * * *");
    expect(cronOf({ ...base, preset: "every_6h" })).toBe("0 */6 * * *");
    expect(cronOf({ ...base, preset: "daily" })).toBe("0 3 * * *");
    expect(cronOf({ ...base, preset: "daily", time: "14:30" })).toBe("30 14 * * *");
    expect(cronOf({ ...base, preset: "weekly", weekday: 0 })).toBe("0 3 * * 0");
    expect(cronOf({ ...base, preset: "custom", cron: "  */20 * * * * " })).toBe("*/20 * * * *");
    expect(cronOf({ ...base, preset: "daily", time: "" })).toBe("");
    expect(cronOf({ ...base, preset: "custom" })).toBe("");
  });

  it("reads a saved schedule back into its preset only when the expression is the preset's", () => {
    expect(scheduleDraftOf("0 3 * * *", "daily")).toMatchObject({ preset: "daily", time: "03:00" });
    expect(scheduleDraftOf("30 14 * * 0", "weekly")).toMatchObject({
      preset: "weekly",
      time: "14:30",
      weekday: 0,
    });
    expect(scheduleDraftOf("0 * * * *", "hourly").preset).toBe("hourly");
    expect(scheduleDraftOf("0 */6 * * *", "every_6h").preset).toBe("every_6h");
    // A preset name over an expression that is not its own is Custom, and keeps the expression.
    expect(scheduleDraftOf("0 4 * * 2", "daily")).toMatchObject({
      preset: "custom",
      cron: "0 4 * * 2",
    });
    expect(scheduleDraftOf("*/20 * * * *", "custom")).toMatchObject({
      preset: "custom",
      cron: "*/20 * * * *",
    });
  });

  it("starts Custom from the expression the old choice wrote", () => {
    const daily = scheduleDraftOf("0 5 * * *", "daily");
    expect(scheduleChange(daily, { preset: "custom" }).cron).toBe("0 5 * * *");
    // Editing the text of a custom schedule leaves it as typed.
    const custom = scheduleChange(daily, { preset: "custom" });
    expect(scheduleChange(custom, { cron: "0 5" }).cron).toBe("0 5");
  });
});

describe("the form's view of a policy", () => {
  it("shows the saved values, and a reset field as the default and not overridden", () => {
    const policy = janitorPolicy({ keepN: { value: 5, overridden: true } });
    expect(viewOf(policy, {}).keepN).toEqual({ value: "5", overridden: true });
    expect(viewOf(policy, { keepN: null }).keepN).toEqual({ value: "2", overridden: false });
    expect(viewOf(policy, { keepN: "7" }).keepN).toEqual({ value: "7", overridden: true });
    expect(viewOf(policy, {}).graceHours).toEqual({ value: "24", overridden: false });
  });

  it("follows a changed default for a field that is not overridden and not for one that is", () => {
    const policy = janitorPolicy({
      keepN: { value: 9, overridden: false },
      graceHours: { value: 30, overridden: true },
      defaults: { ...janitorPolicy().defaults, keepN: 9, graceHours: 48 },
    });
    expect(viewOf(policy, {}).keepN).toEqual({ value: "9", overridden: false });
    expect(viewOf(policy, {}).graceHours).toEqual({ value: "30", overridden: true });
  });

  it("drops an edit that leaves the field as saved", () => {
    const policy = janitorPolicy({ keepN: { value: 5, overridden: true } });
    expect(withEdit(policy, {}, "keepN", "5")).toEqual({});
    expect(withEdit(policy, { keepN: "6" }, "keepN", "5")).toEqual({});
    expect(withEdit(policy, {}, "keepN", "6")).toEqual({ keepN: "6" });
    // Reset on a field that follows the default is nothing to save.
    expect(withEdit(janitorPolicy(), {}, "keepN", null)).toEqual({});
    // Typing the default into a field that follows it is nothing to save either.
    expect(withEdit(janitorPolicy(), {}, "keepN", "2")).toEqual({});
    expect(withEdit(janitorPolicy(), {}, "removeUnusedNetworks", false)).toEqual({});
    // Resetting an overridden field is a change, even though the shown value may be the same.
    const same = janitorPolicy({ keepN: { value: 2, overridden: true } });
    expect(withEdit(same, {}, "keepN", null)).toEqual({ keepN: null });
  });

  it("is dirty exactly when an edit is left", () => {
    expect(isDirty({})).toBe(false);
    expect(isDirty({ enabled: false })).toBe(true);
    expect(isDirty({ keepN: null })).toBe(true);
  });

  it("sends the value of each field the environment sets and null for each that follows the default", () => {
    const policy = janitorPolicy({
      keepN: { value: 5, overridden: true },
      schedule: { value: "0 4 * * 2", preset: "custom", overridden: true },
    });
    expect(fieldsOf(viewOf(policy, {}))).toEqual({
      enabled: null,
      schedule: "0 4 * * 2",
      schedulePreset: "custom",
      keepN: 5,
      graceHours: null,
      removeUnusedNetworks: null,
      removeStoppedContainers: null,
    });
    expect(fieldsOf(viewOf(policy, { keepN: null, enabled: false })).keepN).toBeNull();
    expect(fieldsOf(viewOf(policy, { enabled: false })).enabled).toBe(false);
  });

  it("reads the numbers of a view, and a half-typed one as not a number", () => {
    const view = viewOf(janitorPolicy(), { keepN: "", graceHours: "12" });
    const values = valuesOf(view);
    expect(values.keepN).toBeNaN();
    expect(values.graceHours).toBe(12);
    expect(values.schedule).toBe("0 3 * * *");
  });
});
