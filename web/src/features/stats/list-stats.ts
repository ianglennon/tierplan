// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useMemo, useRef, useState } from "react";

import {
  num,
  openStatsConnection,
  type StatsBody,
  type StatsConnectionOptions,
} from "@/features/stats/stats-stream";
import { apiUrl } from "@/lib/base-path";

// The streams behind the Live stats switch of the lists and the node bars of the Placement
// page (plan 06-33, RES-11, D-31). A list opens one stream for the rows that are on screen
// and in an open group, at most 100, and keeps only the latest round: no history is kept for
// lists, and nothing here is written to storage.

// The most rows one list stream is asked for; the server refuses more (T-06-96).
export const MAX_LIST_STATS = 100;
// How long the rows must stay the same before the stream is opened again for them.
export const LIST_STATS_DEBOUNCE_MS = 500;

export type ListKind = "services" | "containers";

// What a list cell shows for a row: the latest round's sum of its running tasks (a container
// is one task). A value is null while it could not be told. `counted` tasks gave a sample;
// `silent` run on a node that is not reporting and are left out. `limitBytes` is the memory
// limit each counted task has, when they all have the same one.
export interface ListStat {
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  counted: number;
  silent: number;
  limitBytes: number | null;
}

// What a node's header shows: the CPU percent (of one core, summed) and the memory its
// running tasks use, against the node's own capacity.
export interface NodeStat {
  cpu: number | null;
  mem: number;
  cpus: number;
  memoryBytes: number;
  notReporting: boolean;
}

const enc = encodeURIComponent;

export function listStatsUrl(envId: string, kind: ListKind, ids: readonly string[]): string {
  return apiUrl(
    `/api/env/${enc(envId)}/${kind}/stats/stream?${new URLSearchParams({ ids: ids.join(",") })}`,
  );
}

export const nodeStatsUrl = (envId: string): string =>
  apiUrl(`/api/env/${enc(envId)}/nodes/stats/stream`);

export const serviceStatsUrl = (envId: string, serviceId: string): string =>
  apiUrl(`/api/env/${enc(envId)}/services/${enc(serviceId)}/stats/stream`);

function entriesOf(body: StatsBody | undefined, key: string): StatsBody[] {
  const raw = body?.[key];
  if (!Array.isArray(raw)) return [];
  return raw.filter(
    (e): e is StatsBody =>
      typeof e === "object" && e !== null && typeof (e as StatsBody).id === "string",
  );
}

// readListRound takes one sample event of a list stream for what it is. Only numbers are
// believed, and an entry that does not name a row is dropped.
export function readListRound(kind: ListKind, body: StatsBody | undefined): Map<string, ListStat> {
  const out = new Map<string, ListStat>();
  for (const e of entriesOf(body, kind)) {
    const id = e.id as string;
    if (kind === "services") {
      const counted = num(e.counted) ?? 0;
      out.set(id, {
        cpu: num(e.cpu),
        // A total of nothing counted is not 0 B, it is nothing.
        mem: counted > 0 ? num(e.mem) : null,
        netRx: num(e.netRx),
        netTx: num(e.netTx),
        counted,
        silent: num(e.silent) ?? 0,
        limitBytes: num(e.limitBytes),
      });
    } else {
      const mem = num(e.mem);
      out.set(id, {
        cpu: num(e.cpu),
        mem,
        netRx: num(e.netRx),
        netTx: num(e.netTx),
        counted: mem === null ? 0 : 1,
        silent: e.noData === true ? 1 : 0,
        limitBytes: null,
      });
    }
  }
  return out;
}

// readNodeRound takes one sample event of the node stream.
export function readNodeRound(body: StatsBody | undefined): {
  nodes: Map<string, NodeStat>;
  partial: boolean;
} {
  const nodes = new Map<string, NodeStat>();
  for (const e of entriesOf(body, "nodes")) {
    nodes.set(e.id as string, {
      cpu: num(e.cpu),
      mem: num(e.mem) ?? 0,
      cpus: num(e.cpus) ?? 0,
      memoryBytes: num(e.memoryBytes) ?? 0,
      notReporting: e.notReporting === true,
    });
  }
  return { nodes, partial: body?.partial === true };
}

export interface StatsStreamTuning {
  // How long the rows must stay the same before the stream is reopened. 500 ms.
  debounceMs?: number;
  // How long the tab may stay hidden before the stream is let go. 60 seconds.
  hiddenMs?: number;
  // The real EventSource unless a test supplies one.
  createSource?: StatsConnectionOptions["createSource"];
}

// useListStats streams the stats of the rows a list shows, and returns the latest round by
// row ID (a service ID, or node|container for a container). `ids` are the rows that are on
// screen and in a group that is open; none closes the stream. A new set of rows reopens it
// once they have settled for 500 ms; the numbers of the rows that stay are kept meanwhile.
// A hidden tab lets the stream go after a minute and opens it again when seen.
export function useListStats(
  envId: string,
  kind: ListKind,
  ids: readonly string[],
  tuning: StatsStreamTuning = {},
): ReadonlyMap<string, ListStat> {
  const key = useMemo(() => ids.slice(0, MAX_LIST_STATS).join(","), [ids]);
  const scope = `${envId}\u0000${kind}`;
  const [settled, setSettled] = useState({ scope, key });
  const [stats, setStats] = useState<ReadonlyMap<string, ListStat>>(new Map());
  const { debounceMs = LIST_STATS_DEBOUNCE_MS, hiddenMs, createSource } = tuning;

  // Opening from nothing, closing and moving to another environment or kind are at once;
  // moving from one set of rows to another waits until the rows stop changing.
  useEffect(() => {
    if (settled.scope === scope && settled.key === key) return;
    if (settled.scope !== scope || key === "" || settled.key === "") {
      setSettled({ scope, key });
      return;
    }
    const timer = setTimeout(() => setSettled({ scope, key }), debounceMs);
    return () => clearTimeout(timer);
  }, [scope, key, settled, debounceMs]);

  // The stream is the effect's: it is closed when the rows, the environment or the kind
  // change, and when the list is left.
  const sourceRef = useRef(createSource);
  sourceRef.current = createSource;
  const settledKey = settled.scope === scope ? settled.key : null;
  useEffect(() => {
    // Waiting for the rows of another environment or kind to be taken in.
    if (settledKey === null) return;
    if (settledKey === "") {
      setStats((prev) => (prev.size === 0 ? prev : new Map()));
      return;
    }
    const wanted = new Set(settledKey.split(","));
    setStats((prev) => {
      const kept = new Map([...prev].filter(([id]) => wanted.has(id)));
      return kept.size === prev.size ? prev : kept;
    });
    const stream = openStatsConnection({
      url: listStatsUrl(envId, kind, settledKey.split(",")),
      on: { sample: (body) => setStats(readListRound(kind, body)) },
      onStatus: () => {},
      hiddenMs,
      createSource: sourceRef.current,
    });
    return () => stream.close();
  }, [envId, kind, settledKey, hiddenMs]);

  return stats;
}

// useNodeStats streams what the tasks use of each node while `enabled`, and returns the
// latest round by swarm node ID. Off closes the stream and forgets the numbers.
export function useNodeStats(
  envId: string,
  enabled: boolean,
  tuning: StatsStreamTuning = {},
): { nodes: ReadonlyMap<string, NodeStat>; partial: boolean } {
  const [round, setRound] = useState<{ nodes: ReadonlyMap<string, NodeStat>; partial: boolean }>({
    nodes: new Map(),
    partial: false,
  });
  const { hiddenMs, createSource } = tuning;
  const sourceRef = useRef(createSource);
  sourceRef.current = createSource;
  useEffect(() => {
    if (!enabled) {
      setRound((prev) => (prev.nodes.size === 0 ? prev : { nodes: new Map(), partial: false }));
      return;
    }
    const stream = openStatsConnection({
      url: nodeStatsUrl(envId),
      on: { sample: (body) => setRound(readNodeRound(body)) },
      onStatus: () => {},
      hiddenMs,
      createSource: sourceRef.current,
    });
    return () => stream.close();
  }, [envId, enabled, hiddenMs]);
  return round;
}
