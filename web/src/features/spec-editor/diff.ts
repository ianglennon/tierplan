// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

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

// specGroupOf names the card a setting belongs to.
export function specGroupOf(_path: string): SpecGroup {
  return "other";
}

// diffSpecs lists what differs between two service specs as the manager states them.
export function diffSpecs(_current: unknown, _next: unknown): DiffRow[] {
  return [];
}
