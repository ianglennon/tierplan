// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { KeyValueRow } from "@/components/key-value-editor";

// The label rules of a node, as the server keeps them (internal/api/nodelabels.go).
export const MAX_LABELS = 64;
export const MAX_LABEL_KEY_BYTES = 256;
export const MAX_LABEL_VALUE_BYTES = 4096;

export type LabelProblem =
  | { kind: "reserved"; prefix: string }
  | { kind: "keyTooLong" }
  | { kind: "valueTooLong" }
  | { kind: "duplicate" };

export interface LabelCheck {
  rows: Map<string, LabelProblem>;
  tooMany: boolean;
  valid: boolean;
}

export function reservedPrefixOf(_key: string): string | null {
  return null;
}

export function isReservedLabel(_key: string): boolean {
  return false;
}

export function checkLabelRows(_rows: readonly KeyValueRow[]): LabelCheck {
  return { rows: new Map(), tooMany: false, valid: true };
}
