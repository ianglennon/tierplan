// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { WorkingFile } from "@/features/stack-editor/working-copy";

export interface FileRailProps {
  files: WorkingFile[];
  selected: string;
  readOnly: boolean;
  unused?: ReadonlySet<string>;
  onSelect: (path: string) => void;
  onAdd: (path: string, content?: string) => void;
  onRename: (from: string, to: string) => void;
  onDelete: (path: string) => void;
}

export function FileRail(_props: FileRailProps) {
  return null;
}
