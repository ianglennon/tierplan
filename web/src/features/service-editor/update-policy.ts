// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { SpecValue } from "@/features/spec-editor/spec-format";

// Stub: the behaviour arrives with the tests that define it.
export function setAutoRollback(spec: SpecValue, _ticked: boolean): SpecValue {
  return spec;
}

export function autoRollbackState(_spec: unknown): { checked: boolean; continues: boolean } {
  return { checked: false, continues: false };
}
