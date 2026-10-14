// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { NodeSearch, NodeSearchPatch } from "@/features/nodes/node-search";

interface NodeListProps {
  envId: string;
  search: NodeSearch;
  onSearchChange: (patch: NodeSearchPatch) => void;
}

export function NodeList(_props: NodeListProps) {
  return null;
}
