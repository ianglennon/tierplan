// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Scaffold for the red step of plan 06-33: the shapes and the entry points, with nothing
// behind them yet.

export const MAX_LIST_STATS = 100;
export const LIST_STATS_DEBOUNCE_MS = 500;

export type ListKind = "services" | "containers";

export interface ListStat {
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  counted: number;
  silent: number;
  limitBytes: number | null;
}

export interface NodeStat {
  cpu: number | null;
  mem: number;
  cpus: number;
  memoryBytes: number;
  notReporting: boolean;
}

export const listStatsUrl = (_envId: string, _kind: ListKind, _ids: readonly string[]): string =>
  "";
export const nodeStatsUrl = (_envId: string): string => "";
export const serviceStatsUrl = (_envId: string, _serviceId: string): string => "";

export function readListRound(
  _kind: ListKind,
  _body: Record<string, unknown> | undefined,
): Map<string, ListStat> {
  return new Map();
}

export function readNodeRound(_body: Record<string, unknown> | undefined): {
  nodes: Map<string, NodeStat>;
  partial: boolean;
} {
  return { nodes: new Map(), partial: false };
}

export function useListStats(
  _envId: string,
  _kind: ListKind,
  _ids: readonly string[],
): ReadonlyMap<string, ListStat> {
  return new Map();
}

export function useNodeStats(
  _envId: string,
  _enabled: boolean,
): { nodes: ReadonlyMap<string, NodeStat>; partial: boolean } {
  return { nodes: new Map(), partial: false };
}
