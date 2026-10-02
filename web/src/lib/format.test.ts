// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import {
  formatAbsolute,
  formatAbsoluteUtc,
  formatBytes,
  formatClock,
  formatRelative,
} from "@/lib/format";

// A translate stand-in that shows which key and count the formatter asked for,
// so the thresholds are tested without the English copy.
const t = (key: string, options?: Record<string, unknown>) =>
  options?.count === undefined ? key : `${key}:${options.count}`;

const NOW = new Date("2026-10-01T12:00:00Z");
const ago = (ms: number) => new Date(NOW.getTime() - ms).toISOString();
const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

describe("formatRelative (D-66)", () => {
  it("says just now under a minute", () => {
    expect(formatRelative(ago(30 * SEC), NOW, t)).toBe("dashboard.time.justNow");
    expect(formatRelative(ago(59 * SEC), NOW, t)).toBe("dashboard.time.justNow");
  });

  it("counts minutes under an hour, hours under a day and days under a week", () => {
    expect(formatRelative(ago(5 * MIN), NOW, t)).toBe("dashboard.time.minutesAgo:5");
    expect(formatRelative(ago(59 * MIN), NOW, t)).toBe("dashboard.time.minutesAgo:59");
    expect(formatRelative(ago(3 * HOUR), NOW, t)).toBe("dashboard.time.hoursAgo:3");
    expect(formatRelative(ago(23 * HOUR), NOW, t)).toBe("dashboard.time.hoursAgo:23");
    expect(formatRelative(ago(2 * DAY), NOW, t)).toBe("dashboard.time.daysAgo:2");
    expect(formatRelative(ago(6 * DAY + 23 * HOUR), NOW, t)).toBe("dashboard.time.daysAgo:6");
  });

  it("switches to an absolute date from seven days on", () => {
    const expected = new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "short",
      year: "numeric",
    }).format(new Date(ago(10 * DAY)));
    const out = formatRelative(ago(10 * DAY), NOW, t);
    expect(out).toBe(expected);
    expect(out).toContain("2026");
    expect(formatRelative(ago(7 * DAY), NOW, t)).not.toContain("dashboard.time");
  });

  it("never goes negative for a timestamp in the future (clock skew)", () => {
    expect(formatRelative(ago(-45 * SEC), NOW, t)).toBe("dashboard.time.justNow");
    expect(formatRelative(ago(-3 * HOUR), NOW, t)).toBe("dashboard.time.justNow");
  });

  it("returns an empty string for a time it cannot read", () => {
    expect(formatRelative("not a time", NOW, t)).toBe("");
  });
});

describe("formatBytes", () => {
  it("uses binary units with one decimal at most", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(734003200)).toBe("700 MiB");
    expect(formatBytes(5 * 1024 ** 3)).toBe("5 GiB");
  });

  it("moves up a unit when rounding would print 1,024", () => {
    expect(formatBytes(1024 * 1024 - 1)).toBe("1 MiB");
  });

  it("returns an empty string for a size that is not a number", () => {
    expect(formatBytes(Number.NaN)).toBe("");
    expect(formatBytes(-1)).toBe("");
  });
});

describe("formatAbsolute", () => {
  it("shows the local date, time and the time-zone name", () => {
    const iso = "2026-09-21T08:30:15Z";
    const out = formatAbsolute(iso);
    const zone = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" })
      .formatToParts(new Date(iso))
      .find((p) => p.type === "timeZoneName")?.value;
    expect(out).toContain("2026");
    expect(out).toMatch(/\d{1,2}:\d{2}:\d{2}/);
    expect(zone).toBeTruthy();
    expect(out).toContain(zone as string);
  });

  it("returns an empty string for a time it cannot read", () => {
    expect(formatAbsolute("nope")).toBe("");
    expect(formatAbsolute(undefined)).toBe("");
  });
});

describe("formatClock", () => {
  it("is the HH:MM of a time, and undefined when there is none", () => {
    expect(formatClock("2026-10-01T14:05:00Z")).toMatch(/\d{1,2}:\d{2}/);
    expect(formatClock(undefined)).toBeUndefined();
    expect(formatClock("nope")).toBeUndefined();
  });
});

describe("formatAbsoluteUtc", () => {
  it("shows the moment in UTC whatever the browser zone is, with the zone named", () => {
    const out = formatAbsoluteUtc("2026-10-01T04:30:05Z");
    expect(out).toContain("04:30:05");
    expect(out).toContain("UTC");
  });

  it("is empty for a missing or unreadable time", () => {
    expect(formatAbsoluteUtc(undefined)).toBe("");
    expect(formatAbsoluteUtc("nope")).toBe("");
  });
});
