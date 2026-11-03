// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { describe, expect, it } from "vitest";

import {
  formatServerTime,
  formatUntil,
  removedSentence,
  scheduleLine,
  scheduleSummary,
} from "@/features/janitor/status-text";
import i18n from "@/i18n";
import { runSummary } from "@/test-janitor";

const t = (key: string, options?: Record<string, unknown>) =>
  i18n.t(key, { ...options, ns: key.split(".")[0] }) as string;

describe("scheduleSummary", () => {
  it("words each preset, with the time of day for Daily and Weekly", () => {
    expect(scheduleSummary({ schedule: "0 * * * *", schedulePreset: "hourly" }, t)).toBe("Hourly");
    expect(scheduleSummary({ schedule: "0 */6 * * *", schedulePreset: "every_6h" }, t)).toBe(
      "Every 6 hours",
    );
    expect(scheduleSummary({ schedule: "0 3 * * *", schedulePreset: "daily" }, t)).toBe(
      "Daily at 03:00",
    );
    expect(scheduleSummary({ schedule: "30 14 * * *", schedulePreset: "daily" }, t)).toBe(
      "Daily at 14:30",
    );
    expect(scheduleSummary({ schedule: "0 3 * * 1", schedulePreset: "weekly" }, t)).toBe(
      "Weekly on Monday at 03:00",
    );
  });

  it("shows an expression written by hand, and one a preset does not write, as it is", () => {
    expect(scheduleSummary({ schedule: "0 3 * * 1-5", schedulePreset: "custom" }, t)).toBe(
      "On the schedule 0 3 * * 1-5",
    );
    expect(scheduleSummary({ schedule: "0 3 1 * *", schedulePreset: "daily" }, t)).toBe(
      "On the schedule 0 3 1 * *",
    );
  });

  it("names the server's zone in the line", () => {
    expect(
      scheduleLine(
        { schedule: "0 3 * * *", schedulePreset: "daily", timezone: "Europe/London" },
        t,
      ),
    ).toBe("Daily at 03:00, server time (Europe/London).");
  });
});

describe("formatServerTime", () => {
  it("shows the wall clock the server's own timestamp carries, not the browser's", () => {
    expect(formatServerTime("2026-06-04T03:04:00+01:00", "Europe/London")).toContain("03:04");
    expect(formatServerTime("2026-06-04T03:04:00+09:00", "Asia/Tokyo")).toContain("03:04");
    expect(formatServerTime("2026-06-04T03:04:00-05:00", "America/Chicago")).toContain("03:04");
    expect(formatServerTime("2026-06-04T03:04:00Z", "UTC")).toContain("03:04");
  });

  it("trusts the offset over a zone name that Intl would read as another zone", () => {
    // Intl reads BST as a Bangladesh zone; the server meant British Summer Time.
    expect(formatServerTime("2026-06-04T03:04:00+01:00", "BST")).toContain("03:04");
  });

  it("still answers a time without an offset, whatever the zone name", () => {
    expect(formatServerTime("2026-06-04T03:04:00", "Europe/London")).not.toBe("");
    expect(formatServerTime("2026-06-04T03:04:00", "not a zone")).not.toBe("");
  });

  it("is empty for a time it cannot read", () => {
    expect(formatServerTime("not a time", "UTC")).toBe("");
  });
});

describe("formatUntil", () => {
  const now = Date.parse("2026-11-03T10:00:00Z");
  it("says how far away a moment is, in the largest unit that fits", () => {
    expect(formatUntil("2026-11-03T10:30:00Z", now)).toBe("in 30 minutes");
    expect(formatUntil("2026-11-03T15:00:00Z", now)).toBe("in 5 hours");
    expect(formatUntil("2026-11-06T10:00:00Z", now)).toBe("in 3 days");
  });

  it("never says ago for a moment that has passed", () => {
    expect(formatUntil("2026-11-03T09:00:00Z", now)).toBe("this minute");
  });
});

describe("removedSentence", () => {
  it("lists what was removed with the estimated space, zero parts left out", () => {
    const run = runSummary({
      counts: {
        failed: 0,
        kept: 0,
        removedContainers: 14,
        removedImages: 2,
        removedNetworks: 3,
        skippedOffline: 0,
      },
      estBytes: 1_200_000_000,
    });
    expect(removedSentence(run, t)).toBe("Removed 14 containers, 3 networks, 2 images, ~1.2 GB.");
    expect(
      removedSentence(
        runSummary({
          counts: { ...run.counts, removedNetworks: 0, removedImages: 0, removedContainers: 1 },
        }),
        t,
      ),
    ).toBe("Removed 1 container.");
  });

  it("says nothing needed removing for a completed run with nothing removed, and gives the reason of a run that did not run", () => {
    expect(removedSentence(runSummary(), t)).toBe("Nothing needed removing.");
    expect(removedSentence(runSummary({ state: "skipped", reason: "deploy_in_progress" }), t)).toBe(
      "A deploy was in progress. TierPlan will try again at the next scheduled run.",
    );
    expect(removedSentence(runSummary({ state: "partial" }), t)).toBe("Nothing removed.");
  });

  it("has no sentence for a run that has not ended", () => {
    expect(removedSentence(runSummary({ state: "running" }), t)).toBe("");
    expect(removedSentence(runSummary({ state: "waiting" }), t)).toBe("");
  });
});
