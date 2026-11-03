// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { JanitorRunSummary, JanitorStatus } from "@/api/janitor";
import { countParts, formatSize, type Translate } from "@/features/janitor/preview-summary";

// The words that say what the janitor did and when it runs, shared by the Status card, the run
// history and the dashboard card. The server decides what a run did; these only put it in words.

const pad = (n: number) => String(n).padStart(2, "0");

// The five-field expression a Daily or Weekly preset writes: "M H * * *" or "M H * * D".
const DAILY_OR_WEEKLY = /^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-6])$/;

function weekdayName(day: number): string {
  // 1 January 2023 was a Sunday, so day 0 is Sunday.
  return new Intl.DateTimeFormat(undefined, { weekday: "long", timeZone: "UTC" }).format(
    new Date(Date.UTC(2023, 0, 1 + day)),
  );
}

// scheduleSummary is the schedule in a few words: "Daily at 03:00", "Weekly on Monday at 03:00",
// "Hourly", "Every 6 hours", or for an expression written by hand, the expression itself. The
// preset the server named is trusted only when the expression is the one that preset writes.
export function scheduleSummary(
  status: Pick<JanitorStatus, "schedule" | "schedulePreset">,
  t: Translate,
): string {
  const { schedule, schedulePreset } = status;
  if (schedulePreset === "hourly") return t("janitorStatus.schedule.hourly");
  if (schedulePreset === "every_6h") return t("janitorStatus.schedule.every6h");
  const match = DAILY_OR_WEEKLY.exec(schedule);
  if (match && (schedulePreset === "daily" || schedulePreset === "weekly")) {
    const time = `${pad(Number(match[2]))}:${pad(Number(match[1]))}`;
    const day = match[3];
    if (schedulePreset === "daily" && day === "*") {
      return t("janitorStatus.schedule.daily", { time });
    }
    if (schedulePreset === "weekly" && day !== "*") {
      return t("janitorStatus.schedule.weekly", { day: weekdayName(Number(day)), time });
    }
  }
  return t("janitorStatus.schedule.custom", { cron: schedule });
}

// scheduleLine is the schedule summary with the zone it is read in: "Daily at 03:00, server time
// (Europe/London)."
export function scheduleLine(
  status: Pick<JanitorStatus, "schedule" | "schedulePreset" | "timezone">,
  t: Translate,
): string {
  return t("janitorStatus.schedule.line", {
    summary: scheduleSummary(status, t),
    timezone: status.timezone,
  });
}

// formatServerTime is a moment as the wall clock of the server's time zone shows it. The server
// writes its times with the UTC offset in force at that moment, so the offset is what is trusted: a
// zone name is whatever the server's TZ holds, and an abbreviation such as BST or IST can mean a
// different zone to Intl than to the server. The name is used only for a time that carries no offset.
export function formatServerTime(iso: string, timezone: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  const options: Intl.DateTimeFormatOptions = {
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
  };
  const offset = /([+-])(\d{2}):(\d{2})$/.exec(iso);
  const zulu = iso.endsWith("Z");
  if (offset || zulu) {
    const minutes = offset
      ? (offset[1] === "-" ? -1 : 1) * (Number(offset[2]) * 60 + Number(offset[3]))
      : 0;
    // The date moved by the offset, read as UTC, is the server's wall clock.
    return new Intl.DateTimeFormat(undefined, { ...options, timeZone: "UTC" }).format(
      new Date(date.getTime() + minutes * 60_000),
    );
  }
  try {
    return new Intl.DateTimeFormat(undefined, { ...options, timeZone: timezone }).format(date);
  } catch {
    return new Intl.DateTimeFormat(undefined, { ...options, timeZone: "UTC" }).format(date);
  }
}

// formatUntil is how far away a moment in the future is: "in 5 hours", "tomorrow". A moment that has
// passed (the clock moved on since the status was read) reads as "in a moment" in the same way, so
// a stale next run never says "ago".
export function formatUntil(iso: string, nowMs: number): string {
  const target = Date.parse(iso);
  if (Number.isNaN(target)) return "";
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
  const minutes = Math.round((target - nowMs) / 60_000);
  if (minutes < 1) return rtf.format(0, "minute");
  if (minutes < 60) return rtf.format(minutes, "minute");
  const hours = Math.round(minutes / 60);
  if (hours < 48) return rtf.format(hours, "hour");
  return rtf.format(Math.round(hours / 24), "day");
}

// formatStarted is the time a run started, in the browser's own zone, short enough for a table cell.
export function formatStarted(iso: string): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

// What a run took away: "14 containers, 3 networks, 2 images", zero parts left out, empty when
// nothing was removed.
export function removedParts(run: Pick<JanitorRunSummary, "counts">, t: Translate): string[] {
  return countParts(
    {
      containers: run.counts.removedContainers,
      networks: run.counts.removedNetworks,
      images: run.counts.removedImages,
    },
    t,
  );
}

// removedSentence is "Removed 14 containers, 3 networks, 2 images, ~1.2 GB." for a run that removed
// something, and otherwise why it did not: the reason a skipped or failed run gives, "Nothing needed
// removing." for a run that finished with nothing to do, "Nothing removed." for any other. A run that
// has not ended has no sentence.
export function removedSentence(
  run: Pick<JanitorRunSummary, "counts" | "estBytes" | "state" | "reason">,
  t: Translate,
): string {
  if (run.state === "waiting" || run.state === "running") return "";
  const parts = removedParts(run, t);
  if (parts.length > 0) {
    if (run.estBytes !== null) {
      parts.push(t("janitor.counts.size", { size: formatSize(run.estBytes, t) }));
    }
    return t("janitorRun.summary.removed", { parts: parts.join(", ") });
  }
  if (run.reason) return t(`janitorRun.reason.${run.reason}`);
  return run.state === "completed"
    ? t("janitorStatus.status.last.nothing")
    : t("janitorRun.summary.removedNone");
}
