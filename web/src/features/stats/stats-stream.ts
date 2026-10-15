// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EventSourceLike } from "@/features/dashboard/events";
import type { StatsPoint } from "@/features/stats/use-stats-history";
import { apiUrl } from "@/lib/base-path";

// Where a stats stream stands. "connecting" is before the first ready event, "live" while
// samples arrive, "reconnecting" a stream that had been live (or was refused softly) and is
// being opened again, "paused" a stream let go because the tab was hidden, and "closed"
// final: the server said so, or the browser was refused.
export type StatsStatus = "connecting" | "live" | "reconnecting" | "paused" | "closed";
export type StatsCloseReason = "access_lost" | "not_running" | "failed";

export interface StatsStreamState {
  status: StatsStatus;
  reason?: StatsCloseReason;
}

// The first event of a stream: the container's own limits, null when it has none, and
// whether there is anything to sample.
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
  // The container's node stopped reporting, or reports again.
  onNode?: (state: NodeReporting) => void;
  // The container stopped, or runs again.
  onContainerState?: (state: ContainerRunState) => void;
  // The lines of the charts should break here: the stream was reopened after a drop or after
  // the tab was hidden, so what lies between is missing.
  onGap?: (at: number) => void;
  // How long the tab may stay hidden before the stream is let go. 60 seconds (D-30).
  hiddenMs?: number;
  // The clock of a gap, in milliseconds.
  now?: () => number;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
}

export interface StatsStream {
  // Ends the stream. Nothing is reported after it, and a second call does nothing.
  close: () => void;
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then 8
// for every try after (UI-SPEC "Reconnects").
export const STATS_RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

// A tab hidden this long closes its stream; it opens again when the tab is seen (D-30).
export const HIDDEN_CLOSE_MS = 60_000;

export function statsStreamUrl(o: { envId: string; containerId: string; nodeId?: string }): string {
  const q = o.nodeId ? `?${new URLSearchParams({ nodeId: o.nodeId })}` : "";
  return apiUrl(
    `/api/env/${encodeURIComponent(o.envId)}/containers/${encodeURIComponent(o.containerId)}/stats/stream${q}`,
  );
}

function parse(event: Event): Record<string, unknown> | undefined {
  try {
    const body: unknown = JSON.parse((event as MessageEvent<string>).data);
    return typeof body === "object" && body !== null
      ? (body as Record<string, unknown>)
      : undefined;
  } catch {
    return undefined;
  }
}

const num = (v: unknown): number | null => (typeof v === "number" && Number.isFinite(v) ? v : null);

// readSample takes a sample event for what it is, or nothing: only numbers are believed.
function readSample(body: Record<string, unknown>): StatsPoint | undefined {
  const t = num(body.t);
  const mem = num(body.mem);
  if (t === null || mem === null) return undefined;
  return {
    t,
    mem,
    cpu: num(body.cpu),
    netRx: num(body.netRx),
    netTx: num(body.netTx),
    blkRead: num(body.blkRead),
    blkWrite: num(body.blkWrite),
  };
}

// createStatsStream opens the live stats of a container (GET .../stats/stream, server-sent
// events) and reports what arrives. The history is the caller's: this keeps nothing.
//
// A drop is handled here, not by the browser's own retry, so the wait follows the spec:
// the stream says it is reconnecting and opens a new one after 1, 2, 4, then 8 seconds.
// A refusal the browser will not retry (401, 403, 404, 429) ends it. A tab that stays hidden
// for 60 seconds lets the stream go and says paused; seen again, it reopens, and the gap is
// reported so the charts break their lines there.
export function createStatsStream(options: StatsStreamOptions): StatsStream {
  const create = options.createSource ?? ((url: string) => new EventSource(url));
  const hiddenMs = options.hiddenMs ?? HIDDEN_CLOSE_MS;
  const now = options.now ?? Date.now;
  let disposed = false;
  let everLive = false;
  let attempt = 0;
  let paused = false;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let hiddenTimer: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike | undefined;

  const report = (state: StatsStreamState): void => {
    if (!disposed) options.onStatus(state);
  };

  const connect = (): void => {
    const mine = create(statsStreamUrl(options));
    source = mine;
    const live = (): boolean => !disposed && !paused && source === mine;

    mine.addEventListener("ready", (event) => {
      if (!live()) return;
      attempt = 0;
      // What lies between the last sample before a drop and the first after it is missing.
      if (everLive) options.onGap?.(now());
      everLive = true;
      const body = parse(event);
      options.onReady?.({
        limitBytes: num(body?.limitBytes),
        nanoCpus: num(body?.nanoCpus),
        running: body?.running === true,
      });
      report({ status: "live" });
    });
    mine.addEventListener("sample", (event) => {
      if (!live()) return;
      const body = parse(event);
      const point = body && readSample(body);
      if (point) options.onSample(point);
    });
    mine.addEventListener("node", (event) => {
      if (!live()) return;
      const state = parse(event)?.state;
      if (state === "reporting" || state === "not_reporting") options.onNode?.(state);
    });
    mine.addEventListener("state", (event) => {
      if (!live()) return;
      const state = parse(event)?.state;
      if (state === "running" || state === "stopped") options.onContainerState?.(state);
    });
    // The server ended the stream on purpose. Closing the connection here is what keeps the
    // browser from reconnecting into the refusal that follows.
    mine.addEventListener("closed", (event) => {
      if (!live()) return;
      const reason = parse(event)?.reason;
      mine.close();
      report({
        status: "closed",
        reason: reason === "access_lost" || reason === "not_running" ? reason : "failed",
      });
    });
    mine.addEventListener("error", () => {
      if (!live()) return;
      // readyState 2 is the browser giving up on an answer it will not retry; anything else
      // is a connection that dropped.
      const refused = mine.readyState === 2;
      mine.close();
      // Whatever this connection still says is not for us: the next one is the stream.
      source = undefined;
      if (refused) {
        report({ status: "closed", reason: "failed" });
        return;
      }
      report({ status: "reconnecting" });
      const delay = STATS_RECONNECT_DELAYS_MS[
        Math.min(attempt, STATS_RECONNECT_DELAYS_MS.length - 1)
      ] as number;
      attempt += 1;
      retry = setTimeout(connect, delay);
    });
  };

  const pause = (): void => {
    if (disposed || paused) return;
    paused = true;
    clearTimeout(retry);
    source?.close();
    report({ status: "paused" });
  };

  const unpause = (): void => {
    if (disposed || !paused) return;
    paused = false;
    attempt = 0;
    report({ status: everLive ? "reconnecting" : "connecting" });
    connect();
  };

  const onVisibility = (): void => {
    if (document.hidden) {
      clearTimeout(hiddenTimer);
      hiddenTimer = setTimeout(pause, hiddenMs);
    } else {
      clearTimeout(hiddenTimer);
      unpause();
    }
  };
  document.addEventListener("visibilitychange", onVisibility);

  report({ status: "connecting" });
  connect();
  if (document.hidden) onVisibility();

  return {
    close: () => {
      if (disposed) return;
      disposed = true;
      clearTimeout(retry);
      clearTimeout(hiddenTimer);
      document.removeEventListener("visibilitychange", onVisibility);
      source?.close();
    },
  };
}
