// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { WorkingVar } from "@/features/stack-editor/working-copy";

export type BulkParse = { ok: true; rows: WorkingVar[] } | { ok: false; line: number };

// RED stubs: the real parser and component arrive in the GREEN commit.
export function toBulkText(_rows: readonly WorkingVar[]): string {
  return "";
}

export function fromBulkText(_text: string, _rows: readonly WorkingVar[]): BulkParse {
  return { ok: false, line: 0 };
}

export function BulkVariableText(_props: Record<string, never>) {
  return null;
}
