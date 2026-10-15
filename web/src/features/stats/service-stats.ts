// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useCallback, useEffect, useRef, useState } from "react";

import type { EventSourceLike } from "@/features/dashboard/events";
import { serviceStatsUrl } from "@/features/stats/list-stats";
import {
  num,
  openStatsConnection,
  type StatsBody,
  type StatsCloseReason,
  type StatsStatus,
} from "@/features/stats/stats-stream";
import { HISTORY_CAP, type StatsPoint, useStatsHistory } from "@/features/stats/use-stats-history";

// The stats stream of one service (plan 06-33, RES-11, D-29): the sum over its running tasks,
// each task's own sample, and the running tasks it watches. The history is this hook's state
// and goes when the page is left; nothing is stored (D-30).

export interface ServiceStatsTask {
  id: string;
  slot: number;
  node: string;
  nodeId: string;
}

export interface ServiceTaskSample {
  id: string;
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
  noData: boolean;
}

export interface ServiceTotal {
  cpu: number | null;
  mem: number;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
  // Tasks that gave a sample, and tasks on nodes that are not reporting.
  counted: number;
  silent: number;
}

export interface ServiceRound {
  t: number;
  total: ServiceTotal;
  tasks: ServiceTaskSample[];
}

const objectOf = (v: unknown): StatsBody | undefined =>
  typeof v === "object" && v !== null && !Array.isArray(v) ? (v as StatsBody) : undefined;

// readServiceTasks takes the ready or tasks event: the running tasks and the memory limit
// they all share (null when there is none or they differ).
export function readServiceTasks(body: StatsBody | undefined): {
  tasks: ServiceStatsTask[];
  limitBytes: number | null;
} {
  const raw = body?.tasks;
  const tasks: ServiceStatsTask[] = [];
  for (const e of Array.isArray(raw) ? raw : []) {
    const o = objectOf(e);
    if (!o || typeof o.id !== "string") continue;
    tasks.push({
      id: o.id,
      slot: num(o.slot) ?? 0,
      node: typeof o.node === "string" ? o.node : "",
      nodeId: typeof o.nodeId === "string" ? o.nodeId : "",
    });
  }
  const limit = num(body?.limitBytes);
  return { tasks, limitBytes: limit !== null && limit > 0 ? limit : null };
}

// readServiceRound takes one sample event. Only numbers are believed.
export function readServiceRound(body: StatsBody | undefined): ServiceRound | undefined {
  const total = objectOf(body?.total);
  if (!body || !total) return undefined;
  const tasks: ServiceTaskSample[] = [];
  for (const e of Array.isArray(body.tasks) ? body.tasks : []) {
    const o = objectOf(e);
    if (!o || typeof o.id !== "string") continue;
    tasks.push({
      id: o.id,
      cpu: num(o.cpu),
      mem: num(o.mem),
      netRx: num(o.netRx),
      netTx: num(o.netTx),
      blkRead: num(o.blkRead),
      blkWrite: num(o.blkWrite),
      noData: o.noData === true,
    });
  }
  return {
    t: num(body.t) ?? 0,
    total: {
      cpu: num(total.cpu),
      mem: num(total.mem) ?? 0,
      netRx: num(total.netRx),
      netTx: num(total.netTx),
      blkRead: num(total.blkRead),
      blkWrite: num(total.blkWrite),
      counted: num(total.counted) ?? 0,
      silent: num(total.silent) ?? 0,
    },
    tasks,
  };
}

// The point of the history that a round makes: the total of what answered. A round in which
// no task answered is a gap, not a zero.
export function pointOf(round: ServiceRound): StatsPoint | null {
  const { total } = round;
  if (total.counted === 0) return null;
  return {
    t: round.t,
    cpu: total.cpu,
    mem: total.mem,
    netRx: total.netRx,
    netTx: total.netTx,
    blkRead: total.blkRead,
    blkWrite: total.blkWrite,
  };
}

export interface ServiceStats {
  status: StatsStatus;
  closedBy: StatsCloseReason | undefined;
  // The running tasks the stream watches; null until the server has said.
  tasks: ServiceStatsTask[] | null;
  // The memory limit every running task has, when they all have the same one.
  limitBytes: number | null;
  // The latest round, null before the first.
  round: ServiceRound | null;
  // The total's history, and each task's CPU history, the newest last.
  points: StatsPoint[];
  trends: ReadonlyMap<string, ReadonlyArray<number | null>>;
}

// useServiceStats opens the stream of one service while the component is mounted.
export function useServiceStats(
  envId: string,
  serviceId: string,
  createSource?: (url: string) => EventSourceLike,
): ServiceStats {
  const { points, push, markGap, clear } = useStatsHistory();
  const [status, setStatus] = useState<StatsStatus>("connecting");
  const [closedBy, setClosedBy] = useState<StatsCloseReason | undefined>();
  const [tasks, setTasks] = useState<ServiceStatsTask[] | null>(null);
  const [limitBytes, setLimitBytes] = useState<number | null>(null);
  const [round, setRound] = useState<ServiceRound | null>(null);
  const [trends, setTrends] = useState<ReadonlyMap<string, ReadonlyArray<number | null>>>(
    new Map(),
  );
  const sourceRef = useRef(createSource);
  sourceRef.current = createSource;

  const takeTasks = useCallback((body: StatsBody | undefined) => {
    const next = readServiceTasks(body);
    setTasks(next.tasks);
    setLimitBytes(next.limitBytes);
  }, []);

  useEffect(() => {
    setStatus("connecting");
    setClosedBy(undefined);
    setTasks(null);
    setRound(null);
    setTrends(new Map());
    const stream = openStatsConnection({
      url: serviceStatsUrl(envId, serviceId),
      createSource: sourceRef.current,
      onReady: takeTasks,
      on: {
        tasks: takeTasks,
        sample: (body) => {
          const next = readServiceRound(body);
          if (!next) return;
          setRound(next);
          const point = pointOf(next);
          if (point) push(point);
          else markGap(next.t);
          // A task that is not in the round has gone, and its trend with it.
          setTrends((prev) => {
            const out = new Map<string, ReadonlyArray<number | null>>();
            for (const task of next.tasks) {
              const before = prev.get(task.id) ?? [];
              const kept = before.length >= HISTORY_CAP ? before.slice(1) : before;
              out.set(task.id, [...kept, task.noData ? null : task.cpu]);
            }
            return out;
          });
        },
      },
      onGap: markGap,
      onStatus: (s) => {
        setStatus(s.status);
        setClosedBy(s.reason);
      },
    });
    return () => {
      stream.close();
      // The history is this visit's; a stream for another service starts empty.
      clear();
    };
  }, [envId, serviceId, push, markGap, clear, takeTasks]);

  return { status, closedBy, tasks, limitBytes, round, points, trends };
}
