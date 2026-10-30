// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { KeyValueRow } from "@/components/key-value-editor";

// The label rules of a node, as the server keeps them (internal/api/nodelabels.go). They are
// shown while the person types, and the server refuses what they would refuse.
export const MAX_LABELS = 64;
export const MAX_LABEL_KEY_BYTES = 256;
export const MAX_LABEL_VALUE_BYTES = 4096;

// The prefixes Docker keeps for itself. Docker accepts a node label that starts with one
// (RESEARCH E15, Pitfall 16), so Coaming is the one that refuses it.
const RESERVED_PREFIXES = ["com.docker.", "io.docker.", "org.dockerproject."] as const;

export type LabelProblem =
  | { kind: "reserved"; prefix: string }
  | { kind: "keyTooLong" }
  | { kind: "valueTooLong" }
  | { kind: "duplicate" };

export interface LabelCheck {
  // The problem of each row that has one, by the row's id.
  rows: Map<string, LabelProblem>;
  // True when the rows name more than MAX_LABELS labels.
  tooMany: boolean;
  // True when there is nothing to fix.
  valid: boolean;
}

// Lower-cases the ASCII letters only. toLowerCase would also fold the Kelvin sign into a k, and a
// name with one in it is not Docker's.
function asciiLower(text: string): string {
  return text.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
}

// reservedPrefixOf is the reserved prefix a label name starts with, compared without regard to the
// case of ASCII letters, or null when it starts with none.
export function reservedPrefixOf(key: string): string | null {
  const lower = asciiLower(key);
  return RESERVED_PREFIXES.find((p) => lower.startsWith(p)) ?? null;
}

// isReservedLabel says whether Docker keeps a label name for itself.
export function isReservedLabel(key: string): boolean {
  return reservedPrefixOf(key) !== null;
}

const encoder = new TextEncoder();
const bytes = (text: string) => encoder.encode(text).length;

// checkLabelRows finds what is wrong with the rows of the label editor. A row with no name is
// left out, as entriesOf leaves it out, and names are trimmed as it trims them; a name used
// twice is flagged on the later row, since the later value is the one that would be kept.
export function checkLabelRows(rows: readonly KeyValueRow[]): LabelCheck {
  const problems = new Map<string, LabelProblem>();
  const seen = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    if (key === "") continue;
    const prefix = reservedPrefixOf(key);
    if (prefix) problems.set(row.id, { kind: "reserved", prefix });
    else if (bytes(key) > MAX_LABEL_KEY_BYTES) problems.set(row.id, { kind: "keyTooLong" });
    else if (bytes(row.value) > MAX_LABEL_VALUE_BYTES)
      problems.set(row.id, { kind: "valueTooLong" });
    else if (seen.has(key)) problems.set(row.id, { kind: "duplicate" });
    seen.add(key);
  }
  const tooMany = seen.size > MAX_LABELS;
  return { rows: problems, tooMany, valid: problems.size === 0 && !tooMany };
}
