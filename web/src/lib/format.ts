// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Time and size formatting for the resource views. Times arrive in UTC (D-66)
// and are shown in the browser's own locale and time zone through Intl, so
// nothing here knows a language or a date order.

type Translate = (key: string, options?: Record<string, unknown>) => string;

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

function parse(iso: string | undefined): Date | undefined {
  if (!iso) return undefined;
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? undefined : date;
}

// formatRelative is the D-66 short form: "just now" under a minute, minutes
// under an hour, hours under a day, days under a week, then an absolute date.
// A time slightly ahead of the clock (skew between the daemon and this
// browser) reads "just now" rather than a negative age. An unreadable time is
// an empty string, which callers show as a dash.
export function formatRelative(iso: string, now: Date, t: Translate): string {
  const date = parse(iso);
  if (!date) return "";
  const age = now.getTime() - date.getTime();
  if (age < MINUTE) return t("dashboard.time.justNow");
  if (age < HOUR) return t("dashboard.time.minutesAgo", { count: Math.floor(age / MINUTE) });
  if (age < DAY) return t("dashboard.time.hoursAgo", { count: Math.floor(age / HOUR) });
  if (age < 7 * DAY) return t("dashboard.time.daysAgo", { count: Math.floor(age / DAY) });
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
  }).format(date);
}

// formatAbsolute is the full local date and time with the zone name, for the
// tooltip behind a relative time.
export function formatAbsolute(iso: string | undefined): string {
  const date = parse(iso);
  if (!date) return "";
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  }).format(date);
}

// formatAbsoluteUtc is the same moment as formatAbsolute but in UTC, with "UTC"
// as the zone name, for the audit log's detail rows (D-66: stored in UTC).
export function formatAbsoluteUtc(iso: string | undefined): string {
  const date = parse(iso);
  if (!date) return "";
  return new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZone: "UTC",
    timeZoneName: "short",
  }).format(date);
}

// formatClock is the HH:MM of an RFC 3339 time in the browser's own locale and
// 24-hour or 12-hour habit (D-66). It returns undefined for a missing or
// unreadable time so callers can fall back to wording without a time.
export function formatClock(iso: string | undefined): string | undefined {
  const date = parse(iso);
  if (!date) return undefined;
  return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(date);
}

const UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

// formatBytes shows a size in binary units with at most one decimal:
// 1536 is "1.5 KiB" and 734003200 is "700 MiB". A size that is not a
// non-negative number is an empty string.
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "";
  const number = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  let value = n;
  let unit = 0;
  while (value >= 1024 && unit < UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  // 1,023.96 KiB would print as "1,024 KiB"; say "1 MiB" instead.
  if (unit > 0 && unit < UNITS.length - 1 && Math.round(value * 10) / 10 >= 1024) {
    value /= 1024;
    unit++;
  }
  return `${number.format(unit === 0 ? Math.round(value) : value)} ${UNITS[unit]}`;
}

// formatClockOffset is how far a clock is from the server's: a signed whole
// number of seconds rounded toward zero, with a unit ("+2 s", "−45 s", and
// "0 s" with no sign). The minus is U+2212, which lines up with the plus in a
// column. A value that is not a finite number is an empty string.
export function formatClockOffset(seconds: number): string {
  if (!Number.isFinite(seconds)) return "";
  const whole = Math.trunc(seconds);
  if (whole === 0) return "0 s";
  return `${whole > 0 ? "+" : "−"}${Math.abs(whole)} s`;
}
