// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useReducer } from "react";

import { api } from "@/api/client";
import type { PullItem, PullJob } from "@/api/pulls";
import type { EventSourceLike } from "@/features/dashboard/events";
import { apiUrl } from "@/lib/base-path";

// Where the stream of a pull stands. "connecting" is before the first snapshot,
// "reconnecting" a stream that had been live and dropped, "closed" final: the job ended,
// vanished (gone) or can no longer be read by this person (accessLost).
export type PullStreamStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface PullStreamState {
  job?: PullJob;
  status: PullStreamStatus;
  // The job has ended: the server said done, or sent a snapshot of a job that had.
  finished: boolean;
  // The server no longer has the job (404): it ended more than ten minutes ago, or the
  // server restarted.
  gone: boolean;
  accessLost: boolean;
}

export const initialPullStream: PullStreamState = {
  status: "connecting",
  finished: false,
  gone: false,
  accessLost: false,
};

// What the connection reports, in the shape the reducer takes.
export type PullFrame =
  | { type: "snapshot"; job: PullJob }
  | { type: "item"; item: PullItem }
  | { type: "done"; job: PullJob }
  | { type: "status"; status: PullStreamStatus; gone?: boolean; accessLost?: boolean };

// reducePullStream folds one frame into the state. A snapshot replaces the job, an item
// replaces the item of its node, done ends the job and the stream. A finished job never
// goes back to live, whatever the connection does after.
export function reducePullStream(state: PullStreamState, frame: PullFrame): PullStreamState {
  switch (frame.type) {
    case "snapshot":
      return {
        ...state,
        job: frame.job,
        status: state.finished && !isFinished(frame.job) ? state.status : "live",
        finished: state.finished || isFinished(frame.job),
      };
    case "item": {
      if (!state.job) return state;
      const items = state.job.items ?? [];
      const known = items.some((i) => i.nodeId === frame.item.nodeId);
      return {
        ...state,
        job: {
          ...state.job,
          items: known
            ? items.map((i) => (i.nodeId === frame.item.nodeId ? frame.item : i))
            : [...items, frame.item],
        },
      };
    }
    case "done":
      return { ...state, job: frame.job, status: "closed", finished: true };
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

const isFinished = (job: PullJob): boolean => job.state !== "running";

// pullStreamUrl is the address of a job's stream. A browser cannot set Last-Event-ID when
// a stream is reopened by hand, so the last event it saw goes in the query.
export function pullStreamUrl(jobId: string, lastEventId?: string): string {
  const q = lastEventId ? `?${new URLSearchParams({ lastEventId })}` : "";
  return apiUrl(`/api/pulls/${encodeURIComponent(jobId)}/stream${q}`);
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then 8
// for every try after (UI-SPEC "Reconnects").
export const RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

export interface PullStreamOptions {
  jobId: string;
  onFrame: (frame: PullFrame) => void;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
  // Asks the server why a reopened stream was refused, as the HTTP status of a read of
  // the job (0 when the server could not be reached). An EventSource cannot see the
  // status of a refusal itself.
  probe?: () => Promise<number>;
}

export interface PullStream {
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

async function probeJob(jobId: string): Promise<number> {
  try {
    const res = await api.GET("/api/pulls/{jobId}", { params: { path: { jobId } } });
    return res.response.status;
  } catch {
    return 0;
  }
}

// createPullStream opens the stream of a pull job (GET /api/pulls/{jobId}/stream, server-sent
// events) and reports what arrives. A drop is handled here, not by the browser's own retry,
// so the wait follows the spec: the stream says it is reconnecting and opens a new
// connection after 1, 2, 4, then 8 seconds, each time naming the last event it saw. The
// server starts every connection with a snapshot, so what was missed is never lost. A
// connection the server refuses is asked about: a job that is gone or that this person may
// not read ends the stream; anything that may pass is retried on the same schedule. The
// stream closes itself after the done frame.
export function createPullStream(options: PullStreamOptions): PullStream {
  const create = options.createSource ?? ((url: string) => new EventSource(url));
  const probe = options.probe ?? (() => probeJob(options.jobId));
  let disposed = false;
  let finished = false;
  let attempt = 0;
  let lastEventId = "";
  let retry: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike;

  const report = (frame: PullFrame): void => {
    if (!disposed) options.onFrame(frame);
  };

  const schedule = (): void => {
    const delay = RECONNECT_DELAYS_MS[Math.min(attempt, RECONNECT_DELAYS_MS.length - 1)] as number;
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
    source = create(pullStreamUrl(options.jobId, lastEventId || undefined));
    const mine = source;
    const live = (): boolean => !disposed && source === mine;
    const remember = (event: Event): void => {
      const id = (event as MessageEvent).lastEventId;
      if (id) lastEventId = id;
    };

    mine.addEventListener("snapshot", (event) => {
      const job = live() ? parse<PullJob>(event) : undefined;
      if (!job) return;
      attempt = 0;
      report({ type: "snapshot", job });
    });
    mine.addEventListener("item", (event) => {
      const item = live() ? parse<PullItem>(event) : undefined;
      if (!item) return;
      remember(event);
      report({ type: "item", item });
    });
    mine.addEventListener("done", (event) => {
      const job = live() ? parse<PullJob>(event) : undefined;
      if (!job) return;
      finished = true;
      mine.close();
      report({ type: "done", job });
    });
    mine.addEventListener("error", () => {
      if (!live() || finished) return;
      // readyState 2 is the browser giving up on an answer it will not retry (a refusal
      // such as 403, 404 or 429); anything else is a connection that dropped.
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
      source.close();
    },
  };
}

// usePullJob follows a pull job while jobId is set, and gives the state of the stream.
// The stream is opened again for another job and closed when the job is cleared or the
// component goes.
export function usePullJob(jobId: string | null): PullStreamState {
  const [state, dispatch] = useReducer(
    (current: PullStreamState, action: PullFrame | { type: "reset" }) =>
      action.type === "reset" ? initialPullStream : reducePullStream(current, action),
    initialPullStream,
  );
  useEffect(() => {
    dispatch({ type: "reset" });
    if (!jobId) return;
    const stream = createPullStream({ jobId, onFrame: dispatch });
    return () => stream.close();
  }, [jobId]);
  return state;
}
