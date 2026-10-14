// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface Constraint {
  key: string;
  value: string;
  equal: boolean;
}

export interface NodeFacts {
  id: string;
  hostname: string;
  ip: string;
  role: string;
  os: string;
  arch: string;
  labels: Readonly<Record<string, string>>;
  engineLabels: Readonly<Record<string, string>>;
}

export type ParseResult = { ok: true; constraint: Constraint } | { ok: false; error: string };

export type MatchResult = { ok: true; matches: boolean } | { ok: false; error: string };

export function parseConstraint(_s: string): ParseResult {
  return { ok: false, error: "not implemented" };
}

export function matchesConstraint(_c: Constraint, _n: NodeFacts): boolean {
  return false;
}

export function matchesAll(_constraints: readonly string[], _n: NodeFacts): MatchResult {
  return { ok: true, matches: false };
}
