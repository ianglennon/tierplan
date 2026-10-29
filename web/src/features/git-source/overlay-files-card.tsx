// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackProblem } from "@/api/stacks";

export interface OverlayFilesCardProps {
  missing: readonly string[];
  overlays: readonly string[];
  problems: readonly StackProblem[];
  onAdd: (path: string) => void;
  onRemove: (path: string) => void;
}

export function OverlayFilesCard(_props: OverlayFilesCardProps) {
  return null;
}
