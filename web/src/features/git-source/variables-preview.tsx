// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { GitStackVariable } from "@/api/git";

// One variable of the table the person fills in: the value they typed and whether it is secret.
export interface TableVar {
  name: string;
  value: string;
  secret: boolean;
}

export interface VariablesPreviewProps {
  vars: readonly GitStackVariable[] | null;
  value: readonly TableVar[];
  onChange: (next: TableVar[]) => void;
  enabled: boolean;
  checking: boolean;
}

export function VariablesPreview(_props: VariablesPreviewProps) {
  return null;
}

export function RequiredVariablesLine(_props: { vars: readonly GitStackVariable[] | null }) {
  return null;
}
