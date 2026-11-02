// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The placeholder the server puts where it withheld a value (D-39).
export const HIDDEN = "<hidden>";

// The cards a setting is listed under, in the order they are listed.
export const SPEC_GROUPS = [
  "basics",
  "environment",
  "ports",
  "secrets",
  "configs",
  "networks",
  "mounts",
  "resources",
  "placement",
  "restartPolicy",
  "updatePolicy",
  "other",
] as const;

export type SpecGroup = (typeof SPEC_GROUPS)[number];

export type DiffChange = "added" | "changed" | "removed";

export interface DiffRow {
  // The setting's path in the spec, with keyed list entries named in brackets:
  // TaskTemplate.ContainerSpec.Env[DB_URL].
  path: string;
  group: SpecGroup;
  change: DiffChange;
  // The value before and after as text; null where the setting is absent on that side. A
  // withheld value is the placeholder itself, never the real one.
  current: string | null;
  next: string | null;
  // True when either side was withheld. The row then never carries a real value.
  masked: boolean;
  // True when a withheld value was replaced by another: the New column reads that, not the value.
  replaced: boolean;
}

// A path starts with a prefix when the prefix is a whole segment of it.
function under(path: string, prefix: string): boolean {
  return path === prefix || path.startsWith(`${prefix}.`) || path.startsWith(`${prefix}[`);
}

const CS = "TaskTemplate.ContainerSpec";

// specGroupOf names the card a setting belongs to; a setting no card has is under "other".
export function specGroupOf(path: string): SpecGroup {
  if (path === "Name" || under(path, "Labels") || under(path, "Mode") || path === `${CS}.Image`) {
    return "basics";
  }
  if (under(path, `${CS}.Env`)) return "environment";
  if (under(path, "EndpointSpec")) return "ports";
  if (under(path, `${CS}.Secrets`)) return "secrets";
  if (under(path, `${CS}.Configs`)) return "configs";
  if (under(path, "TaskTemplate.Networks") || under(path, "Networks")) return "networks";
  if (under(path, `${CS}.Mounts`)) return "mounts";
  if (under(path, "TaskTemplate.Resources")) return "resources";
  if (under(path, "TaskTemplate.Placement")) return "placement";
  if (under(path, "TaskTemplate.RestartPolicy")) return "restartPolicy";
  if (under(path, "UpdateConfig") || under(path, "RollbackConfig")) return "updatePolicy";
  return "other";
}

type Rec = Record<string, unknown>;

const isRec = (v: unknown): v is Rec => typeof v === "object" && v !== null && !Array.isArray(v);

// The lists whose entries are matched by something they name, not by their place in the list, so
// a reordered list is not a change. Each gives the key of an entry, or undefined for one that
// names none (the whole list is then compared by place).
const KEYED: Record<string, (entry: unknown) => string | undefined> = {
  [`${CS}.Mounts`]: (e) => str(isRec(e) ? e.Target : undefined),
  "EndpointSpec.Ports": (e) =>
    isRec(e) && e.TargetPort !== undefined
      ? `${String(e.TargetPort)}/${str(e.Protocol) ?? "tcp"}`
      : undefined,
  [`${CS}.Secrets`]: (e) => str(isRec(e) && isRec(e.File) ? e.File.Name : undefined),
  [`${CS}.Configs`]: (e) => str(isRec(e) && isRec(e.File) ? e.File.Name : undefined),
  "TaskTemplate.Networks": (e) => str(isRec(e) ? e.Target : undefined),
  Networks: (e) => str(isRec(e) ? e.Target : undefined),
};

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

// text writes a scalar as the person reads it.
function text(v: unknown): string {
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

// Both sides absent, or equal, is no row. A withheld value never leaves with its real
// counterpart: when one side is withheld and the other is not, the row carries the placeholder on
// both sides and says it was replaced.
function leaf(path: string, a: unknown, b: unknown, out: DiffRow[]) {
  if (a === undefined && b === undefined) return;
  const hidden = a === HIDDEN || b === HIDDEN;
  if (hidden) {
    if (a === b) return;
    const both = a !== undefined && b !== undefined;
    out.push({
      path,
      group: specGroupOf(path),
      change: both ? "changed" : a === undefined ? "added" : "removed",
      current: a === undefined ? null : HIDDEN,
      next: b === undefined ? null : HIDDEN,
      masked: true,
      replaced: both,
    });
    return;
  }
  if (a !== undefined && b !== undefined && text(a) === text(b)) return;
  out.push({
    path,
    group: specGroupOf(path),
    change: a === undefined ? "added" : b === undefined ? "removed" : "changed",
    current: a === undefined ? null : text(a),
    next: b === undefined ? null : text(b),
    masked: false,
    replaced: false,
  });
}

const present = (v: unknown) => (v === null ? undefined : v);

// keyedEntries lists a keyed list's entries by key, or undefined when an entry names no key or
// two share one.
function keyedEntries(
  list: unknown[],
  key: (entry: unknown) => string | undefined,
): Map<string, unknown> | undefined {
  const out = new Map<string, unknown>();
  for (const entry of list) {
    const k = key(entry);
    if (k === undefined || out.has(k)) return undefined;
    out.set(k, entry);
  }
  return out;
}

// envEntries lists KEY=value entries by name; a name with no "=" has an empty value.
function envEntries(list: unknown[]): Map<string, unknown> | undefined {
  const out = new Map<string, unknown>();
  for (const entry of list) {
    if (typeof entry !== "string") return undefined;
    const at = entry.indexOf("=");
    const name = at < 0 ? entry : entry.slice(0, at);
    if (name === "" || out.has(name)) return undefined;
    out.set(name, at < 0 ? "" : entry.slice(at + 1));
  }
  return out;
}

function walkKeyed(path: string, a: Map<string, unknown>, b: Map<string, unknown>, out: DiffRow[]) {
  for (const k of [...a.keys(), ...[...b.keys()].filter((x) => !a.has(x))]) {
    walk(`${path}[${k}]`, a.get(k), b.get(k), out);
  }
}

function walk(path: string, a0: unknown, b0: unknown, out: DiffRow[]) {
  const a = present(a0);
  const b = present(b0);
  if (a === undefined && b === undefined) return;
  const aList = Array.isArray(a);
  const bList = Array.isArray(b);
  if ((aList || a === undefined) && (bList || b === undefined) && (aList || bList)) {
    const la = aList ? a : [];
    const lb = bList ? b : [];
    const keyed =
      path === `${CS}.Env`
        ? envEntries
        : (l: unknown[]) => {
            const key = KEYED[path];
            return key ? keyedEntries(l, key) : undefined;
          };
    const ka = keyed(la);
    const kb = keyed(lb);
    if (ka && kb) {
      walkKeyed(path, ka, kb, out);
      return;
    }
    for (let i = 0; i < Math.max(la.length, lb.length); i++)
      walk(`${path}[${i}]`, la[i], lb[i], out);
    return;
  }
  if ((isRec(a) || a === undefined) && (isRec(b) || b === undefined) && (isRec(a) || isRec(b))) {
    const ra = isRec(a) ? a : {};
    const rb = isRec(b) ? b : {};
    const join = (k: string) => (path === "" ? k : `${path}.${k}`);
    for (const k of [...Object.keys(ra), ...Object.keys(rb).filter((x) => !(x in ra))]) {
      walk(join(k), ra[k], rb[k], out);
    }
    return;
  }
  if (typeof a === "object" || typeof b === "object") {
    // One side is a section and the other a plain value: list what went and what came.
    walk(path, a, undefined, out);
    walk(path, undefined, b, out);
    return;
  }
  leaf(path, a, b, out);
}

// diffSpecs lists what differs between two service specs as the manager states them (the
// inspect document's Spec). Environment variables are matched by name, mounts by target, ports
// by target port and protocol, secrets and configs by file name and networks by target; every
// other list is matched by place. A section that was added or removed is listed leaf by leaf.
// A value the server withheld (HIDDEN) is never listed while it is withheld on both sides and
// never leaves in a row. Rows come in the order of SPEC_GROUPS, and in the spec's own order
// within a group. A missing spec reads as an empty one.
export function diffSpecs(current: unknown, next: unknown): DiffRow[] {
  const rows: DiffRow[] = [];
  walk("", current, next, rows);
  const order = (g: SpecGroup) => SPEC_GROUPS.indexOf(g);
  return rows
    .map((row, at) => ({ row, at }))
    .sort((x, y) => order(x.row.group) - order(y.row.group) || x.at - y.at)
    .map((x) => x.row);
}
