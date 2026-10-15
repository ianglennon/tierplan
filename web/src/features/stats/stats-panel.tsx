// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EventSourceLike } from "@/features/dashboard/events";

interface StatsPanelProps {
  envId: string;
  containerId: string;
  nodeId?: string;
  name: string;
  running?: boolean;
  nodeName?: string;
  createSource?: (url: string) => EventSourceLike;
}

export function StatsPanel(_props: StatsPanelProps) {
  return null;
}
