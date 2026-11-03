// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useReducer } from "react";

import type { JanitorJobFrame, JanitorJobItemFrame } from "@/api/janitor";
import type { EventSourceLike } from "@/features/dashboard/events";
import { apiUrl } from "@/lib/base-path";

// Where the stream of a clean-up stands. "connecting" is before the first snapshot, "reconnecting" a
// stream that had been live and dropped, "closed" final: the job ended, vanished (gone) or can no
// longer be read by this person (accessLost).
export type JanitorStreamStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface JanitorStreamState {
  job?: JanitorJobFrame;
  status: JanitorStreamStatus;
  // The job has ended: the server said done, or sent a snapshot of a job that had.
  finished: boolean;
  // The server no longer holds the job (404): it ended more than ten minutes ago or the server
  // restarted. The run record still says how it ended.
  gone: boolean;
  accessLost: boolean;
}

export const initialJanitorStream: JanitorStreamState = {
  status: "connecting",
  finished: false,
  gone: false,
  accessLost: false,
};

export type JanitorStreamFrame =
  | { type: "snapshot"; job: JanitorJobFrame }
  | { type: "item"; item: JanitorJobItemFrame }
  | { type: "done"; job: JanitorJobFrame }
  | { type: "status"; status: JanitorStreamStatus; gone?: boolean; accessLost?: boolean }
  | { type: "reset" };

const isFinished = (job: JanitorJobFrame): boolean => job.state !== "running";

const ended = (item: JanitorJobItemFrame): boolean =>
  item.state !== "waiting" && item.state !== "running";

// progressOf is how many removals of the job have ended and how many it planned, counted from the
// items so that an item frame moves it without waiting for a new snapshot.
export function progressOf(job: Pick<JanitorJobFrame, "items">): { done: number; total: number } {
  const items = job.items ?? [];
  return { done: items.filter(ended).length, total: items.length };
}

// withProgress puts the counts of the items into a job, so the job always agrees with itself.
function withProgress(job: JanitorJobFrame): JanitorJobFrame {
  return { ...job, items: job.items ?? [], ...progressOf(job) };
}

// reduceJanitorStream folds one frame into the state. A snapshot replaces the job, an item replaces
// the item of its ID (or adds it, for an item the run planned after the snapshot), done ends the job
// and the stream. A finished job never goes back to live, whatever the connection does after.
export function reduceJanitorStream(
  state: JanitorStreamState,
  frame: JanitorStreamFrame,
): JanitorStreamState {
  switch (frame.type) {
    case "reset":
      return initialJanitorStream;
    case "snapshot":
      return {
        ...state,
        job: withProgress(frame.job),
        status: state.finished && !isFinished(frame.job) ? state.status : "live",
        finished: state.finished || isFinished(frame.job),
      };
    case "item": {
      if (!state.job) return state;
      const items = state.job.items;
      const known = items.some((i) => i.id === frame.item.id);
      return {
        ...state,
        job: withProgress({
          ...state.job,
          items: known
            ? items.map((i) => (i.id === frame.item.id ? frame.item : i))
            : [...items, frame.item],
        }),
      };
    }
    case "done":
      return { ...state, job: withProgress(frame.job), status: "closed", finished: true };
    case "status":
      if (state.finished) return state;
      return {
        ...state,
        status: frame.status,
        gone: frame.gone ?? state.gone,
        accessLost: frame.accessLost ?? state.accessLost,
      };
  }
}

// janitorRunStreamUrl is the address of a run's stream. A browser cannot set Last-Event-ID when a
// stream is reopened by hand, so the last event it saw goes in the query.
export function janitorRunStreamUrl(envId: string, runId: string, lastEventId?: string): string {
  const q = lastEventId ? `?${new URLSearchParams({ lastEventId })}` : "";
  const path = [envId, runId].map(encodeURIComponent);
  return apiUrl(`/api/env/${path[0]}/janitor/runs/${path[1]}/stream${q}`);
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then 8 for every
// try after (UI-SPEC "Reconnects", as the deploy console).
export const JANITOR_RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

export interface JanitorRunStreamOptions {
  envId: string;
  runId: string;
  onFrame: (frame: JanitorStreamFrame) => void;
  // The real EventSource unless a test supplies one. Where the browser has none the stream is not
  // opened and the page keeps reading the run over HTTP.
  createSource?: (url: string) => EventSourceLike | undefined;
  // Asks the server why a reopened stream was refused, as an HTTP status (0 when the server could
  // not be reached). An EventSource cannot see the status of a refusal itself.
  probe?: (url: string) => Promise<number>;
}

export interface JanitorRunStream {
  // Ends the stream and closes the connection. Nothing is reported after it.
  close: () => void;
}

function parse<T>(event: Event): T | undefined {
  try {
    return JSON.parse((event as MessageEvent<string>).data) as T;
  } catch {
    return undefined;
  }
}

// probeStream opens the stream's address once and drops the connection as soon as the server has
// answered, to learn the status of a refusal.
async function probeStream(url: string): Promise<number> {
  const abort = new AbortController();
  try {
    const res = await fetch(url, {
      signal: abort.signal,
      headers: { Accept: "text/event-stream" },
    });
    return res.status;
  } catch {
    return 0;
  } finally {
    abort.abort();
  }
}

const defaultSource = (url: string): EventSourceLike | undefined =>
  typeof EventSource === "undefined" ? undefined : new EventSource(url);

// openJanitorRunStream opens the stream of a run (server-sent events) and reports what arrives, with
// the shape of the deploy console's: a drop is handled here and not by the browser's own retry, so
// the wait follows the spec. It says it is reconnecting and opens a new connection after 1, 2, 4,
// then 8 seconds, each time naming the last event it saw. The server starts every connection with a
// snapshot of the whole job, so nothing that was missed is lost. A connection the server refuses is
// asked about: a job that is gone (404) or that this person may not read ends the stream, anything
// else is retried on the same schedule. The stream closes itself after the done frame.
export function openJanitorRunStream(options: JanitorRunStreamOptions): JanitorRunStream {
  const create = options.createSource ?? defaultSource;
  const probe = options.probe ?? probeStream;
  let disposed = false;
  let finished = false;
  let attempt = 0;
  let lastEventId = "";
  let retry: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike | undefined;

  const report = (frame: JanitorStreamFrame): void => {
    if (!disposed) options.onFrame(frame);
  };

  const schedule = (): void => {
    const delay = JANITOR_RECONNECT_DELAYS_MS[
      Math.min(attempt, JANITOR_RECONNECT_DELAYS_MS.length - 1)
    ] as number;
    attempt += 1;
    retry = setTimeout(() => connect(), delay);
  };

  const refused = async (url: string): Promise<void> => {
    const status = await probe(url);
    if (disposed || finished) return;
    if (status === 404) report({ type: "status", status: "closed", gone: true });
    else if (status === 401 || status === 403)
      report({ type: "status", status: "closed", accessLost: true });
    else schedule();
  };

  const connect = (): void => {
    const url = janitorRunStreamUrl(options.envId, options.runId, lastEventId || undefined);
    const mine = create(url);
    if (!mine) return;
    source = mine;
    const live = (): boolean => !disposed && source === mine;
    const remember = (event: Event): void => {
      const id = (event as MessageEvent).lastEventId;
      if (id) lastEventId = id;
    };

    mine.addEventListener("snapshot", (event) => {
      const job = live() ? parse<JanitorJobFrame>(event) : undefined;
      if (!job) return;
      attempt = 0;
      report({ type: "snapshot", job });
    });
    mine.addEventListener("item", (event) => {
      const item = live() ? parse<JanitorJobItemFrame>(event) : undefined;
      if (!item) return;
      remember(event);
      report({ type: "item", item });
    });
    mine.addEventListener("done", (event) => {
      const job = live() ? parse<JanitorJobFrame>(event) : undefined;
      if (!job) return;
      finished = true;
      mine.close();
      report({ type: "done", job });
    });
    mine.addEventListener("error", () => {
      if (!live() || finished) return;
      // readyState 2 is the browser giving up on an answer it will not retry (a refusal such as
      // 403, 404 or 429); anything else is a connection that dropped.
      const wasRefused = mine.readyState === 2;
      mine.close();
      report({ type: "status", status: "reconnecting" });
      if (wasRefused) void refused(url);
      else schedule();
    });
  };

  connect();

  return {
    close: () => {
      if (disposed) return;
      disposed = true;
      clearTimeout(retry);
      source?.close();
    },
  };
}

// useJanitorRunStream follows a run's job while enabled: the state of the stream, the job as it
// stands and whether it has ended. The page enables it only while the run record says the run is
// waiting or working, and falls back to the record alone when the stream is gone, closed, or the
// browser has none.
export function useJanitorRunStream(
  envId: string,
  runId: string,
  enabled = true,
): JanitorStreamState {
  const [state, dispatch] = useReducer(reduceJanitorStream, initialJanitorStream);
  useEffect(() => {
    dispatch({ type: "reset" });
    if (!enabled) return;
    const stream = openJanitorRunStream({ envId, runId, onFrame: dispatch });
    return () => stream.close();
  }, [envId, runId, enabled]);
  return state;
}
