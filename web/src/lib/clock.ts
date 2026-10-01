// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// formatClock is the HH:MM of an RFC 3339 time in the browser's own locale and
// 24-hour or 12-hour habit (D-66). It returns undefined for a missing or
// unreadable time so callers can fall back to wording without a time.
export function formatClock(iso: string | undefined): string | undefined {
  if (!iso) return undefined;
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return undefined;
  return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(date);
}
