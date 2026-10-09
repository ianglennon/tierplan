// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "zod";

import { apiUrl } from "@/lib/base-path";

// The audit log's filters live in the URL (D-43), so a filtered view is
// linkable and survives a refresh. This file is the one place that knows how
// the URL maps to the API's query parameters: the list and the export both go
// through listParams, so an export is always exactly what the screen shows.

export const RANGES = ["1h", "24h", "7d", "30d", "all", "custom"] as const;
export type Range = (typeof RANGES)[number];

export const DEFAULT_RANGE: Range = "24h";

// Every action key the backend writes (internal/audit). The backend owns the
// list, so an action this file has not heard of still shows, under its raw key
// (UI-SPEC). The Action filter offers these.
export const KNOWN_ACTIONS: readonly string[] = [
  "auth.login",
  "auth.login_failed",
  "auth.locked",
  "auth.unlocked",
  "auth.logout",
  "auth.password_changed",
  "auth.link_refused",
  "setup.token_failed",
  "setup.completed",
  "setup.attempt_denied",
  "settings.updated",
  "environment.added",
  "environment.renamed",
  "environment.removed",
  "agent.enrolled",
  "agent.enrol_refused",
  "agent.cert_renewed",
  "agent.revoked",
  "agent.blocked",
  "agent.unblocked",
  "agent.auto_revoked",
  "agent.updated",
  "agent.update_failed",
  "agent.uninstalled",
  "agent.policy_denied",
  "environment.token_rotated",
  "environment.token_disabled",
  "environment.failover",
  "environment.cluster_adopted",
  "environment.local_folded",
  "environment.auto_update_changed",
  "warning.acknowledged",
  "audit.pruned",
  "audit.reanchored",
  "permission.denied",
  "user.created",
  "user.updated",
  "user.disabled",
  "user.enabled",
  "user.deleted",
  "user.invite_created",
  "user.invite_regenerated",
  "user.invite_accepted",
  "user.password_reset_issued",
  "user.password_reset_completed",
  "user.admin_granted",
  "user.admin_revoked",
  "session.revoked",
  "user.sessions_revoked",
  "team.created",
  "team.renamed",
  "team.deleted",
  "team.member_added",
  "team.member_removed",
  "access.granted",
  "access.updated",
  "access.revoked",
  "owner.transferred",
  "automation.paused",
  "automation.resumed",
  "backup.exported",
  "backup.scheduled_run",
  "backup.failed",
  "backup.checked",
  "backup.restored",
  "backup.schedule_updated",
  "backup.upload_discarded",
  "key.rotated",
  "container.started",
  "container.stopped",
  "container.restarted",
  "container.killed",
  "container.paused",
  "container.unpaused",
  "container.removed",
];

// The actions that can carry an environment, which is all an env Admin's scoped
// view can hold (D-09): refusals, access changes, environment and agent events,
// automations and ownership transfers. Instance-level events (sign-ins, users,
// teams, settings, backups, keys, the log itself) have no environment, so the
// server never returns them to an env Admin and offering them would only build
// a filter that matches nothing. An action in the address that is not here still
// shows in the filter, so a linked view can be cleared.
const ENV_SCOPED_PREFIXES = [
  "access.",
  "environment.",
  "agent.",
  "automation.",
  "owner.",
  "container.",
];
export const ENV_SCOPED_ACTIONS: readonly string[] = KNOWN_ACTIONS.filter(
  (key) => key === "permission.denied" || ENV_SCOPED_PREFIXES.some((p) => key.startsWith(p)),
);

const DAY = /^(\d{4})-(\d{2})-(\d{2})$/;

// TanStack Router JSON-parses each search value that is valid JSON, so a user
// named 123 or true reaches the schema as a number or a boolean. Both read
// back as the text the person typed.
const text = z
  .union([z.string(), z.number(), z.boolean()])
  .optional()
  .transform((v) => (v === undefined || v === "" ? undefined : String(v)));

// One action arrives as a string and several as an array.
const actions = z
  .union([z.string(), z.array(z.string())])
  .optional()
  .transform((v) => {
    const list = (Array.isArray(v) ? v : v === undefined ? [] : [v]).filter((a) => a !== "");
    return list.length > 0 ? list : undefined;
  });

// A custom range's days are calendar days, YYYY-MM-DD, in the browser's time
// zone. Anything else is dropped rather than shown as an error.
const day = z.string().regex(DAY).optional().catch(undefined);

export const auditSearchSchema = z.object({
  user: text,
  action: actions,
  bulk: text,
  range: z.enum(RANGES).default(DEFAULT_RANGE).catch(DEFAULT_RANGE),
  from: day,
  to: day,
});

// The filters as the screen and the helpers hold them. Every member may be left
// out, so a caller can build one by hand; the schema's output is one of these.
export interface AuditSearch {
  user?: string | undefined;
  action?: string[] | undefined;
  // One bulk operation's ID (D-14): only the entries it left.
  bulk?: string | undefined;
  range?: Range | undefined;
  from?: string | undefined;
  to?: string | undefined;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

const HOUR_MS = 3_600_000;
const PRESET_MS = {
  "1h": HOUR_MS,
  "24h": 24 * HOUR_MS,
  "7d": 7 * 24 * HOUR_MS,
  "30d": 30 * 24 * HOUR_MS,
} as const;

// localDay reads YYYY-MM-DD as a day of the browser's time zone, or undefined
// for text that is not a real calendar day (2026-02-31 would roll over).
function localDay(value: string | undefined): { y: number; m: number; d: number } | undefined {
  const match = value ? DAY.exec(value) : null;
  if (!match) return undefined;
  const y = Number(match[1]);
  const m = Number(match[2]);
  const d = Number(match[3]);
  const check = new Date(y, m - 1, d);
  if (check.getFullYear() !== y || check.getMonth() !== m - 1 || check.getDate() !== d) {
    return undefined;
  }
  return { y, m, d };
}

// formatDay writes a Date as the YYYY-MM-DD of its day in the browser's time
// zone, which is how the custom range sits in the URL.
export function formatDay(date: Date): string {
  const pad = (n: number, width = 2) => String(n).padStart(width, "0");
  return `${pad(date.getFullYear(), 4)}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

// parseDay is the local midnight of a YYYY-MM-DD day, for the calendar to show
// as selected. It is undefined for text that is not a real calendar day.
export function parseDay(value: string | undefined): Date | undefined {
  const day = localDay(value);
  return day ? new Date(day.y, day.m - 1, day.d) : undefined;
}

// rangeFor is the from and to the API takes, as UTC ISO strings. A preset
// counts back from now and leaves the end open. A custom range is inclusive
// days in the browser's time zone: the first day from 00:00:00.000 and the last
// through 23:59:59.999, either of which may be left open.
export function rangeFor(
  search: Pick<AuditSearch, "range" | "from" | "to">,
  now: Date,
): { from?: string; to?: string } {
  const range = search.range ?? DEFAULT_RANGE;
  if (range === "all") return {};
  if (range !== "custom") {
    return { from: new Date(now.getTime() - PRESET_MS[range]).toISOString() };
  }
  const out: { from?: string; to?: string } = {};
  const from = localDay(search.from);
  const to = localDay(search.to);
  if (from) out.from = new Date(from.y, from.m - 1, from.d, 0, 0, 0, 0).toISOString();
  if (to) out.to = new Date(to.y, to.m - 1, to.d, 23, 59, 59, 999).toISOString();
  return out;
}

// actionLabel is the human text of an action key. The raw key stays in the
// expanded row and in exports. An unknown key shows as itself.
export function actionLabel(key: string, t: Translate): string {
  return KNOWN_ACTIONS.includes(key) ? t(`audit.actions.${key}`) : key;
}

// listParams are the API's filter parameters for a search: the user, one
// repeated action parameter per action, the bulk ID, and from and to. A cursor and a page
// size are the list's business, not a filter, so they are not here.
export function listParams(search: AuditSearch, now: Date): URLSearchParams {
  const params = new URLSearchParams();
  if (search.user) params.set("user", search.user);
  for (const action of search.action ?? []) params.append("action", action);
  if (search.bulk) params.set("bulk", search.bulk);
  const { from, to } = rangeFor(search, now);
  if (from) params.set("from", from);
  if (to) params.set("to", to);
  return params;
}

// exportUrl is the download address for the entries a search matches. It is a
// plain GET: the browser sends the session cookie, so no CSRF token is needed.
export function exportUrl(format: "csv" | "json", search: AuditSearch, now: Date): string {
  const params = new URLSearchParams({ format });
  for (const [key, value] of listParams(search, now)) params.append(key, value);
  return apiUrl(`/api/audit/export?${params.toString()}`);
}
