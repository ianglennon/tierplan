// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { SwarmMark } from "@/features/logs/log-source";

export type Translate = (key: string, options?: Record<string, unknown>) => string;

// swarmMarkerText is what a marker row says (plan 06-30, D-34, D-37).
export function swarmMarkerText(_t: Translate, _mark: SwarmMark, _service: string): string {
  return "";
}

export interface ReplacementMarkerProps {
  text: string;
  kind?: string;
}

// ReplacementMarker is the centred, muted row between lines where a task was replaced, a slot
// is waiting for a task or a node cannot be reached.
export function ReplacementMarker(_props: ReplacementMarkerProps) {
  return null;
}
