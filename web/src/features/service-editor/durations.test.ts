// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import { formatDuration, parseDuration } from "@/features/service-editor/durations";

// Go durations as the Form's Delay, Window and Monitor fields take them (plan 06-21, UI-SPEC
// "Field errors"): 5s, 30s, 2m, 1h30m, stored as integer nanoseconds.

describe("parseDuration", () => {
  it("reads a single unit", () => {
    expect(parseDuration("5s")).toBe(5_000_000_000);
    expect(parseDuration("30s")).toBe(30_000_000_000);
    expect(parseDuration("2m")).toBe(120_000_000_000);
    expect(parseDuration("1h")).toBe(3_600_000_000_000);
    expect(parseDuration("250ms")).toBe(250_000_000);
    expect(parseDuration("10us")).toBe(10_000);
    expect(parseDuration("10µs")).toBe(10_000);
    expect(parseDuration("7ns")).toBe(7);
  });

  it("adds up several units", () => {
    expect(parseDuration("1h30m")).toBe(5_400_000_000_000);
    expect(parseDuration("1m30s")).toBe(90_000_000_000);
    expect(parseDuration("1h2m3s4ms")).toBe(3_723_004_000_000);
  });

  it("reads a fraction, as Go does", () => {
    expect(parseDuration("1.5s")).toBe(1_500_000_000);
    expect(parseDuration("0.5h")).toBe(1_800_000_000_000);
  });

  it("trims the text around it", () => {
    expect(parseDuration("  5s ")).toBe(5_000_000_000);
  });

  it("answers undefined for nothing typed", () => {
    expect(parseDuration("")).toBeUndefined();
    expect(parseDuration("   ")).toBeUndefined();
  });

  it("reads a bare 0, which Go accepts, as zero", () => {
    expect(parseDuration("0")).toBe(0);
    expect(parseDuration("0s")).toBe(0);
  });

  it("refuses a number with no unit, a space inside, an unknown unit, a sign and words", () => {
    expect(parseDuration("5")).toBeNull();
    expect(parseDuration("5 s")).toBeNull();
    expect(parseDuration("5d")).toBeNull();
    expect(parseDuration("-5s")).toBeNull();
    expect(parseDuration("+5s")).toBeNull();
    expect(parseDuration("s")).toBeNull();
    expect(parseDuration("five seconds")).toBeNull();
    expect(parseDuration("5s5")).toBeNull();
    expect(parseDuration("1..5s")).toBeNull();
    expect(parseDuration("5S")).toBeNull();
  });

  it("refuses a length that is not a whole number of nanoseconds a browser can hold", () => {
    expect(parseDuration("1000000h")).toBeNull();
  });
});

describe("formatDuration", () => {
  it("writes the largest units that fit", () => {
    expect(formatDuration(5_000_000_000)).toBe("5s");
    expect(formatDuration(90_000_000_000)).toBe("1m30s");
    expect(formatDuration(120_000_000_000)).toBe("2m");
    expect(formatDuration(3_600_000_000_000)).toBe("1h");
    expect(formatDuration(5_400_000_000_000)).toBe("1h30m");
    expect(formatDuration(3_723_000_000_000)).toBe("1h2m3s");
  });

  it("writes a part of a second in the unit that holds it exactly", () => {
    expect(formatDuration(250_000_000)).toBe("250ms");
    expect(formatDuration(10_000)).toBe("10µs");
    expect(formatDuration(7)).toBe("7ns");
    expect(formatDuration(1_500_000_000)).toBe("1.5s");
    expect(formatDuration(61_500_000_000)).toBe("1m1.5s");
  });

  it("writes zero as 0s", () => {
    expect(formatDuration(0)).toBe("0s");
  });

  it("writes nothing for a value that is not a length of time", () => {
    expect(formatDuration(undefined)).toBe("");
    expect(formatDuration(null)).toBe("");
    expect(formatDuration("5s")).toBe("");
    expect(formatDuration(-1)).toBe("");
    expect(formatDuration(Number.NaN)).toBe("");
    expect(formatDuration(1.5)).toBe("");
  });

  it("round-trips what it writes", () => {
    for (const ns of [1, 999, 1_000, 5_000_000, 5_000_000_000, 61_500_000_000, 5_400_000_000_000]) {
      expect(parseDuration(formatDuration(ns))).toBe(ns);
    }
  });
});
