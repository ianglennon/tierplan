// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { NodeDetail } from "@/api/nodes";

export interface NodeLabelsEditorProps {
  envId: string;
  node: Pick<NodeDetail, "id" | "hostname" | "labels" | "version">;
}

export function NodeLabelsEditor(_props: NodeLabelsEditorProps) {
  return null;
}
