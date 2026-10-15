// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EventSourceLike } from "@/features/dashboard/events";
import type { StatsPoint } from "@/features/stats/use-stats-history";

export type StatsStatus = "connecting" | "live" | "reconnecting" | "paused" | "closed";
export type StatsCloseReason = "access_lost" | "not_running" | "failed";

export interface StatsStreamState {
  status: StatsStatus;
  reason?: StatsCloseReason;
}

export interface StatsReady {
  limitBytes: number | null;
  nanoCpus: number | null;
  running: boolean;
}

export type NodeReporting = "reporting" | "not_reporting";
export type ContainerRunState = "running" | "stopped";

export interface StatsStreamOptions {
  envId: string;
  containerId: string;
  nodeId?: string;
  onReady?: (info: StatsReady) => void;
  onSample: (point: StatsPoint) => void;
  onStatus: (state: StatsStreamState) => void;
  onNode?: (state: NodeReporting) => void;
  onContainerState?: (state: ContainerRunState) => void;
  onGap?: (at: number) => void;
  hiddenMs?: number;
  now?: () => number;
  createSource?: (url: string) => EventSourceLike;
}

export interface StatsStream {
  close: () => void;
}

export const STATS_RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

export const HIDDEN_CLOSE_MS = 60_000;

export function statsStreamUrl(_o: {
  envId: string;
  containerId: string;
  nodeId?: string;
}): string {
  return "";
}

export function createStatsStream(_options: StatsStreamOptions): StatsStream {
  return { close: () => {} };
}
