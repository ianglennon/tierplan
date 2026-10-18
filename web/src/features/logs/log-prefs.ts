// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { z } from "@/lib/zod-config";

// How a person likes to read logs, kept per browser (D-19) and never holding a line of
// any log. The address can override each choice, so a link or a pop-out opens the way
// it was sent (D-22).
export interface LogPrefs {
  wrap: boolean;
  timestamps: boolean;
  utc: boolean;
}

export const DEFAULT_LOG_PREFS: LogPrefs = { wrap: false, timestamps: false, utc: false };

const PREFS_KEY = "bayplan.logs.view";

// loadLogPrefs reads the saved choices. Anything missing, unreadable or of the wrong
// type falls back to the default, and storage that is off (private windows) is fine.
export function loadLogPrefs(): LogPrefs {
  try {
    const raw = JSON.parse(localStorage.getItem(PREFS_KEY) ?? "null") as Partial<LogPrefs> | null;
    return {
      wrap: raw?.wrap === true,
      timestamps: raw?.timestamps === true,
      utc: raw?.utc === true,
    };
  } catch {
    return { ...DEFAULT_LOG_PREFS };
  }
}

export function saveLogPrefs(prefs: LogPrefs): void {
  try {
    // The Task names choice (below) lives in the same place; saving the three choices
    // keeps it.
    const kept = readStored();
    localStorage.setItem(
      PREFS_KEY,
      JSON.stringify(
        typeof kept.taskNames === "boolean" ? { ...prefs, taskNames: kept.taskNames } : prefs,
      ),
    );
  } catch {
    // Storage is full or off; the choice then lasts as long as the page.
  }
}

function readStored(): Partial<LogPrefs> & { taskNames?: unknown } {
  try {
    const raw = JSON.parse(localStorage.getItem(PREFS_KEY) ?? "null") as unknown;
    return raw !== null && typeof raw === "object" ? (raw as Partial<LogPrefs>) : {};
  } catch {
    return {};
  }
}

// Whether a service's log shows the task of each line (plan 06-29, D-32). On until it is
// turned off, and kept per browser with the other choices.
export function loadTaskNames(): boolean {
  return readStored().taskNames !== false;
}

export function saveTaskNames(on: boolean): void {
  try {
    localStorage.setItem(PREFS_KEY, JSON.stringify({ ...readStored(), taskNames: on }));
  } catch {
    // Storage is full or off; the choice then lasts as long as the page.
  }
}

// What the Logs address carries (D-22): the range, the search, and the three toggles.
// Each is optional; a value that does not fit is dropped rather than refused, so an
// old or edited link still opens.
export interface LogViewParams {
  since?: string;
  q?: string;
  ts?: boolean;
  utc?: boolean;
  wrap?: boolean;
  // A service's log adds these (plan 06-29, D-33): false hides the task prefixes, task and
  // node are comma lists the server narrows the stream to, and stopped takes in the tasks
  // that had already ended.
  prefix?: boolean;
  task?: string;
  node?: string;
  stopped?: boolean;
}

// The router turns a value like 404 into a number before the schema sees it.
const text = (max: number) =>
  z.preprocess(
    (v) => (typeof v === "number" ? String(v) : v),
    z.string().min(1).max(max).optional().catch(undefined),
  );

export const logSearchSchema = z.object({
  since: text(100),
  q: text(500),
  ts: z.boolean().optional().catch(undefined),
  utc: z.boolean().optional().catch(undefined),
  wrap: z.boolean().optional().catch(undefined),
});

// A switch in the address: a boolean, or what the router makes of 0 and 1 (it reads every
// value as JSON first, so ?prefix=0 arrives as a number).
const flag = z.preprocess((v) => {
  if (v === 0 || v === "0") return false;
  if (v === 1 || v === "1") return true;
  return v;
}, z.boolean().optional().catch(undefined));

// The address of a service's Logs tab: the container log's, and the prefix switch, the task
// and node lists and the stopped switch.
export const serviceLogSearchSchema = logSearchSchema.extend({
  prefix: flag,
  task: text(2000),
  node: text(2000),
  stopped: flag,
});

// ---- the Show range ----

// What "Show" offers: the last 500 lines (the default), a stretch of recent time, the
// whole log, or a range of the person's own.
export type ShowRange =
  | { kind: "lines" }
  | { kind: "last"; preset: "5m" | "1h" | "24h" }
  | { kind: "all" }
  | { kind: "custom"; from: string; to?: string };

export const SHOW_LINES = 500;
// The most a range asks the server for; it is also the buffer's cap's neighbour.
export const SHOW_RANGE_TAIL = 50_000;

const PRESET_MS = { "5m": 5 * 60_000, "1h": 3_600_000, "24h": 86_400_000 } as const;

// parseShow reads the `since` of the address. Anything it does not understand is the
// default.
export function parseShow(since: string | undefined): ShowRange {
  if (since === "5m" || since === "1h" || since === "24h") return { kind: "last", preset: since };
  if (since === "all") return { kind: "all" };
  if (since) {
    const [from, to] = since.split("..");
    const f = from ? Date.parse(from) : Number.NaN;
    if (!Number.isNaN(f)) {
      if (to === undefined) return { kind: "custom", from: new Date(f).toISOString() };
      const u = Date.parse(to);
      if (!Number.isNaN(u) && u > f) {
        return { kind: "custom", from: new Date(f).toISOString(), to: new Date(u).toISOString() };
      }
    }
  }
  return { kind: "lines" };
}

// formatShow is the `since` of the address for a range; the default has none.
export function formatShow(show: ShowRange): string | undefined {
  switch (show.kind) {
    case "lines":
      return undefined;
    case "last":
      return show.preset;
    case "all":
      return "all";
    case "custom":
      return show.to ? `${show.from}..${show.to}` : show.from;
  }
}

// What the stream is asked for: a time range comes with a tail large enough to hold
// it, because the engine takes the tail first and the range second.
export function streamRange(
  show: ShowRange,
  now: Date,
): { tail: number | "all"; since?: string; until?: string } {
  switch (show.kind) {
    case "lines":
      return { tail: SHOW_LINES };
    case "all":
      return { tail: "all" };
    case "last":
      return {
        tail: SHOW_RANGE_TAIL,
        since: new Date(now.getTime() - PRESET_MS[show.preset]).toISOString(),
      };
    case "custom":
      return { tail: SHOW_RANGE_TAIL, since: show.from, until: show.to };
  }
}

// The value of a datetime-local input for an ISO time, in the browser's time zone,
// and back.
export function toLocalInput(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

export function fromLocalInput(value: string): string | undefined {
  const t = value ? new Date(value).getTime() : Number.NaN;
  return Number.isNaN(t) ? undefined : new Date(t).toISOString();
}
