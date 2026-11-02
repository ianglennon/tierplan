// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useCallback, useEffect, useReducer } from "react";

import { api } from "@/api/client";
import {
  isJobGone,
  isJobRunning,
  JOB_REFETCH_MS,
  type StackJob,
  type StackJobItem,
  type StackJobOutputLine,
  stackJobQuery,
} from "@/api/stacks";
import type { EventSourceLike } from "@/features/dashboard/events";
import { apiUrl } from "@/lib/base-path";

// Where the stream of a deploy stands. "connecting" is before the first snapshot,
// "reconnecting" a stream that had been live and dropped, "closed" final: the job ended, vanished
// (gone) or can no longer be read by this person (accessLost).
export type StackStreamStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface StackStreamState {
  job?: StackJob;
  // The output log, oldest first, one line for each sequence number.
  output: StackJobOutputLine[];
  status: StackStreamStatus;
  // The job has ended: the server said done, or sent a snapshot of a job that had.
  finished: boolean;
  // The server no longer has the job (404): it ended more than ten minutes ago, or the server
  // restarted.
  gone: boolean;
  accessLost: boolean;
}

export const initialStackStream: StackStreamState = {
  output: [],
  status: "connecting",
  finished: false,
  gone: false,
  accessLost: false,
};

// What the connection and the fallback read report, in the shape the reducer takes.
export type StackFrame =
  | { type: "snapshot"; job: StackJob }
  | { type: "item"; item: StackJobItem }
  | { type: "output"; line: StackJobOutputLine }
  | { type: "done"; job: StackJob }
  // The whole job, read over HTTP while the stream is not live.
  | { type: "polled"; job: StackJob }
  | { type: "status"; status: StackStreamStatus; gone?: boolean; accessLost?: boolean }
  | { type: "reset" };

const isFinished = (job: StackJob): boolean => job.state !== "running";

// How many lines of output the console keeps, as many as the server keeps for a job.
export const MAX_OUTPUT_LINES = 2000;

// keepNewest cuts a log to the newest MAX_OUTPUT_LINES lines.
function keepNewest(lines: StackJobOutputLine[]): StackJobOutputLine[] {
  return lines.length > MAX_OUTPUT_LINES ? lines.slice(lines.length - MAX_OUTPUT_LINES) : lines;
}

// appendOutput adds a line to a log unless the log already has it or a newer one: the sequence
// number of a line only grows, so a line at or before the newest one is a repeat (a replay after a
// reconnect re-sends what the snapshot held).
function appendOutput(log: StackJobOutputLine[], line: StackJobOutputLine): StackJobOutputLine[] {
  const newest = log.at(-1)?.seq ?? 0;
  return line.seq > newest ? keepNewest([...log, line]) : log;
}

// mergeOutput adds the lines of a log the state does not have yet.
function mergeOutput(
  log: StackJobOutputLine[],
  incoming: readonly StackJobOutputLine[] | null | undefined,
): StackJobOutputLine[] {
  let out = log;
  for (const line of incoming ?? []) out = appendOutput(out, line);
  return out;
}

// reduceStackStream folds one frame into the state. A snapshot replaces the job, an item replaces
// the item of its ID (or adds it, for an item the deploy added while it ran), done ends the job and
// the stream. A finished job never goes back to live, whatever the connection does after. A job read
// over HTTP replaces the job only while the stream is not live, so a slower answer never takes the
// place of a newer frame.
export function reduceStackStream(state: StackStreamState, frame: StackFrame): StackStreamState {
  switch (frame.type) {
    case "reset":
      return initialStackStream;
    case "snapshot":
      return {
        ...state,
        job: frame.job,
        // A snapshot holds the whole log the server keeps: it replaces what was here.
        output: keepNewest([...(frame.job.output ?? [])]),
        status: state.finished && !isFinished(frame.job) ? state.status : "live",
        finished: state.finished || isFinished(frame.job),
      };
    case "item": {
      if (!state.job) return state;
      const items = state.job.items ?? [];
      const known = items.some((i) => i.id === frame.item.id);
      return {
        ...state,
        job: {
          ...state.job,
          items: known
            ? items.map((i) => (i.id === frame.item.id ? frame.item : i))
            : [...items, frame.item],
        },
      };
    }
    case "output":
      return { ...state, output: appendOutput(state.output, frame.line) };
    case "done":
      return {
        ...state,
        job: frame.job,
        output: mergeOutput(state.output, frame.job.output),
        status: "closed",
        finished: true,
      };
    case "polled":
      if (state.finished || state.status === "live") return state;
      return {
        ...state,
        job: frame.job,
        output: keepNewest([...(frame.job.output ?? [])]),
        finished: isFinished(frame.job),
      };
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

// stackJobStreamUrl is the address of a deploy's stream. A browser cannot set Last-Event-ID when a
// stream is reopened by hand, so the last event it saw goes in the query.
export function stackJobStreamUrl(
  envId: string,
  stackId: string,
  jobId: string,
  lastEventId?: string,
): string {
  const q = lastEventId ? `?${new URLSearchParams({ lastEventId })}` : "";
  const path = [envId, stackId, jobId].map(encodeURIComponent);
  return apiUrl(`/api/env/${path[0]}/stacks/${path[1]}/deploys/${path[2]}/stream${q}`);
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then 8 for every
// try after (UI-SPEC "Reconnects").
export const STACK_RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

export interface StackJobStreamOptions {
  envId: string;
  stackId: string;
  jobId: string;
  onFrame: (frame: StackFrame) => void;
  // The real EventSource unless a test supplies one. Where the browser has none the stream is not
  // opened and the console keeps reading the job over HTTP.
  createSource?: (url: string) => EventSourceLike | undefined;
  // Asks the server why a reopened stream was refused, as the HTTP status of a read of the job (0
  // when the server could not be reached). An EventSource cannot see the status of a refusal itself.
  probe?: () => Promise<number>;
}

export interface StackJobStream {
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

async function probeJob(envId: string, stackId: string, jobId: string): Promise<number> {
  try {
    const res = await api.GET("/api/env/{envId}/stacks/{stackId}/deploys/{jobId}", {
      params: { path: { envId, stackId, jobId } },
    });
    return res.response.status;
  } catch {
    return 0;
  }
}

const defaultSource = (url: string): EventSourceLike | undefined =>
  typeof EventSource === "undefined" ? undefined : new EventSource(url);

// openStackJobStream opens the stream of a deploy (GET .../deploys/{jobId}/stream, server-sent
// events) and reports what arrives, with the shape of the pull stream: a drop is handled here, not
// by the browser's own retry, so the wait follows the spec. It says it is reconnecting and opens a
// new connection after 1, 2, 4, then 8 seconds, each time naming the last event it saw. The server
// starts every connection with a snapshot of the whole job, so what was missed is never lost, and
// sends the events after the id it was given, so nothing is sent twice. A connection the server
// refuses is asked about: a job that is gone or that this person may not read ends the stream;
// anything that may pass is retried on the same schedule. The stream closes itself after the done
// frame.
export function openStackJobStream(options: StackJobStreamOptions): StackJobStream {
  const create = options.createSource ?? defaultSource;
  const probe = options.probe ?? (() => probeJob(options.envId, options.stackId, options.jobId));
  let disposed = false;
  let finished = false;
  let attempt = 0;
  let lastEventId = "";
  let retry: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike | undefined;

  const report = (frame: StackFrame): void => {
    if (!disposed) options.onFrame(frame);
  };

  const schedule = (): void => {
    const delay = STACK_RECONNECT_DELAYS_MS[
      Math.min(attempt, STACK_RECONNECT_DELAYS_MS.length - 1)
    ] as number;
    attempt += 1;
    retry = setTimeout(() => connect(), delay);
  };

  const refused = async (): Promise<void> => {
    const status = await probe();
    if (disposed || finished) return;
    if (status === 404) report({ type: "status", status: "closed", gone: true });
    else if (status === 401 || status === 403)
      report({ type: "status", status: "closed", accessLost: true });
    else schedule();
  };

  const connect = (): void => {
    const mine = create(
      stackJobStreamUrl(options.envId, options.stackId, options.jobId, lastEventId || undefined),
    );
    if (!mine) return;
    source = mine;
    const live = (): boolean => !disposed && source === mine;
    const remember = (event: Event): void => {
      const id = (event as MessageEvent).lastEventId;
      if (id) lastEventId = id;
    };

    mine.addEventListener("snapshot", (event) => {
      const job = live() ? parse<StackJob>(event) : undefined;
      if (!job) return;
      attempt = 0;
      report({ type: "snapshot", job });
    });
    mine.addEventListener("item", (event) => {
      const item = live() ? parse<StackJobItem>(event) : undefined;
      if (!item) return;
      remember(event);
      report({ type: "item", item });
    });
    mine.addEventListener("output", (event) => {
      const line = live() ? parse<StackJobOutputLine>(event) : undefined;
      if (!line) return;
      remember(event);
      report({ type: "output", line });
    });
    mine.addEventListener("done", (event) => {
      const job = live() ? parse<StackJob>(event) : undefined;
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
      if (wasRefused) void refused();
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

export interface StackJobFollow extends StackStreamState {
  // The job read over HTTP failed before any job was known.
  loadError: boolean;
  // Reads the job again, for the retry of a load error.
  reload: () => void;
}

// useStackJob follows a deploy: the job at once from one read over HTTP, then live from the stream.
// While the stream is not live (it has not connected, or it dropped and is reconnecting) the read
// goes on every second, so the console keeps its last state and learns the newest one without
// waiting for the connection; once the stream is live, or the job has ended, nothing is polled.
export function useStackJob(envId: string, stackId: string, jobId: string): StackJobFollow {
  const [state, dispatch] = useReducer(reduceStackStream, initialStackStream);
  const base = stackJobQuery(envId, stackId, jobId);
  const quiet = state.status === "live" || state.finished;
  const query = useQuery({
    ...base,
    refetchInterval: (q) => {
      if (isJobGone(q.state.error) || quiet) return false;
      const job = q.state.data;
      return job !== undefined && !isJobRunning(job) ? false : JOB_REFETCH_MS;
    },
  });

  useEffect(() => {
    dispatch({ type: "reset" });
    const stream = openStackJobStream({ envId, stackId, jobId, onFrame: dispatch });
    return () => stream.close();
  }, [envId, stackId, jobId]);

  const polled = query.data;
  useEffect(() => {
    if (polled) dispatch({ type: "polled", job: polled });
  }, [polled]);

  const gone = state.gone || isJobGone(query.error);
  const job = state.job ?? polled;
  const refetch = query.refetch;
  const reload = useCallback(() => void refetch(), [refetch]);
  return {
    ...state,
    job,
    gone,
    loadError: query.isError && !gone && job === undefined,
    reload,
  };
}
