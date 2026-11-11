// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useReducer } from "react";

import { api } from "@/api/client";
import type { EventSourceLike } from "@/features/dashboard/events";
import { apiUrl } from "@/lib/base-path";

// The steps of a certificate run, in the order the server sends them (UI-SPEC "Run panel").
export const ACME_STEPS = [
  "account",
  "order",
  "challenge",
  "validate",
  "finalize",
  "stored",
] as const;

// The steps of a DNS-01 run, in the order the server sends them (internal/acme/dns_steps.go
// stepItemsDNS01): Propagation sits between Challenge and Validation, and Clean-up comes last, after
// Stored, because the order withdraws its answers once the certificate is kept (UI-SPEC "Run panel -
// DNS-01 steps"). Clean-up runs whether the order succeeded or failed and reports on its own.
export const ACME_STEPS_DNS01 = [
  "account",
  "order",
  "challenge",
  "propagation",
  "validate",
  "finalize",
  "stored",
  "cleanup",
] as const;
export type ACMEStepId = (typeof ACME_STEPS)[number] | (typeof ACME_STEPS_DNS01)[number];

// The challenge a run proves its names with.
export type ACMEChallenge = "http-01" | "dns-01";

// stepsFor is the list of steps a run draws, from the run's challenge. A run that names none is an
// HTTP-01 run, so a frame from before DNS-01 existed still draws its six.
export function stepsFor(challenge: string | undefined): readonly ACMEStepId[] {
  return challenge === "dns-01" ? ACME_STEPS_DNS01 : ACME_STEPS;
}

// Where a step is. "skipped" is a step that was not needed (the account was already registered) or
// not reached after a failure.
export type ACMEStepState = "waiting" | "running" | "done" | "failed" | "cancelled" | "skipped";

// One step as the stream sends it (an item frame). code is the class of a failure, message a fixed
// sentence; neither carries anything the CA said.
export interface ACMEStep {
  id: string;
  label: string;
  state: ACMEStepState;
  code?: string;
  message?: string;
  // What a DNS-01 step is about, set while it runs (the server leaves a value out when it has none):
  // the TXT record, the certificate name, the name the record was delegated to and the resolvers asked.
  record?: string;
  host?: string;
  target?: string;
  resolvers?: string[] | null;
  // How many TXT records the Clean-up step could not delete; set with the code left_over.
  leftOver?: number;
}

export type ACMERunStateName = "running" | "done" | "failed" | "cancelled";

// A run as the stream sends it in the snapshot and done frames. The server derives state from the
// steps, so a success reads "done" even when its Account step was skipped.
export interface ACMERun {
  jobId: string;
  kind: "issue" | "renew";
  trigger: "admin" | "schedule" | "resume";
  // What the run proves its names with; a frame without one is HTTP-01.
  challenge?: ACMEChallenge;
  state: ACMERunStateName;
  startedAt: string;
  items: ACMEStep[] | null;
}

// Where the stream of a run stands. "connecting" is before the first snapshot, "reconnecting" a stream
// that had been live and dropped, "closed" final: the run ended, vanished (gone) or can no longer be
// read by this person (accessLost).
export type ACMERunStreamStatus = "connecting" | "live" | "reconnecting" | "closed";

export interface ACMERunStreamState {
  run?: ACMERun;
  status: ACMERunStreamStatus;
  // The run has ended: the server said done, or sent a snapshot of a run that had.
  finished: boolean;
  // The server no longer has the run (404): it ended more than ten minutes ago, or the server restarted.
  gone: boolean;
  accessLost: boolean;
}

export const initialACMERunStream: ACMERunStreamState = {
  status: "connecting",
  finished: false,
  gone: false,
  accessLost: false,
};

// What the connection reports, in the shape the reducer takes.
export type ACMERunFrame =
  | { type: "snapshot"; run: ACMERun }
  | { type: "item"; item: ACMEStep }
  | { type: "done"; run: ACMERun }
  | { type: "status"; status: ACMERunStreamStatus; gone?: boolean; accessLost?: boolean };

const isFinished = (run: ACMERun): boolean => run.state !== "running";

// reduceACMERunStream folds one frame into the state. A snapshot replaces the run, an item replaces
// the step of its id, done ends the run and the stream. A finished run never goes back to live,
// whatever the connection does after.
export function reduceACMERunStream(
  state: ACMERunStreamState,
  frame: ACMERunFrame,
): ACMERunStreamState {
  switch (frame.type) {
    case "snapshot":
      return {
        ...state,
        run: frame.run,
        status: state.finished && !isFinished(frame.run) ? state.status : "live",
        finished: state.finished || isFinished(frame.run),
      };
    case "item": {
      if (!state.run) return state;
      const items = state.run.items ?? [];
      const known = items.some((i) => i.id === frame.item.id);
      return {
        ...state,
        run: {
          ...state.run,
          items: known
            ? items.map((i) => (i.id === frame.item.id ? frame.item : i))
            : [...items, frame.item],
        },
      };
    }
    case "done":
      return { ...state, run: frame.run, status: "closed", finished: true };
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

// acmeRunStreamUrl is the address of a run's stream. A browser cannot set Last-Event-ID when a stream
// is reopened by hand, so the last event it saw goes in the query.
export function acmeRunStreamUrl(jobId: string, lastEventId?: string): string {
  const q = lastEventId ? `?${new URLSearchParams({ lastEventId })}` : "";
  return apiUrl(`/api/https/acme/runs/${encodeURIComponent(jobId)}/stream${q}`);
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then 8 for every try
// after (UI-SPEC "Reconnects").
export const ACME_RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

export interface ACMERunStreamOptions {
  jobId: string;
  onFrame: (frame: ACMERunFrame) => void;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
  // Asks the server why a reopened stream was refused, as the HTTP status of a read of the run (0 when
  // the server could not be reached). An EventSource cannot see the status of a refusal itself.
  probe?: () => Promise<number>;
}

export interface ACMERunStream {
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

// probeRun reads the run the only way the API offers: through the status. A run the status no longer
// lists as active has ended and is gone from the stream, which is what a 404 on the stream means.
async function probeRun(jobId: string): Promise<number> {
  try {
    const { data, response } = await api.GET("/api/https/status");
    if (!response.ok) return response.status;
    return data?.acme?.activeRun?.jobId === jobId ? 200 : 404;
  } catch {
    return 0;
  }
}

// createACMERunStream opens the stream of a certificate run (GET /api/https/acme/runs/{jobId}/stream,
// server-sent events) and reports what arrives. A drop is handled here, not by the browser's own
// retry, so the wait follows the spec: the stream says it is reconnecting and opens a new connection
// after 1, 2, 4, then 8 seconds, each time naming the last event it saw. The server starts every
// connection with a snapshot, so what was missed is never lost. A connection the server refuses is
// asked about: a run that is gone or that this person may not read ends the stream; anything that may
// pass is retried on the same schedule. The stream closes itself after the done frame.
export function createACMERunStream(options: ACMERunStreamOptions): ACMERunStream {
  const create = options.createSource ?? ((url: string) => new EventSource(url));
  const probe = options.probe ?? (() => probeRun(options.jobId));
  let disposed = false;
  let finished = false;
  let attempt = 0;
  let lastEventId = "";
  let retry: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike;

  const report = (frame: ACMERunFrame): void => {
    if (!disposed) options.onFrame(frame);
  };

  const schedule = (): void => {
    const delay = ACME_RECONNECT_DELAYS_MS[
      Math.min(attempt, ACME_RECONNECT_DELAYS_MS.length - 1)
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
    source = create(acmeRunStreamUrl(options.jobId, lastEventId || undefined));
    const mine = source;
    const live = (): boolean => !disposed && source === mine;
    const remember = (event: Event): void => {
      const id = (event as MessageEvent).lastEventId;
      if (id) lastEventId = id;
    };

    mine.addEventListener("snapshot", (event) => {
      const run = live() ? parse<ACMERun>(event) : undefined;
      if (!run) return;
      attempt = 0;
      report({ type: "snapshot", run });
    });
    mine.addEventListener("item", (event) => {
      const item = live() ? parse<ACMEStep>(event) : undefined;
      if (!item) return;
      remember(event);
      report({ type: "item", item });
    });
    mine.addEventListener("done", (event) => {
      const run = live() ? parse<ACMERun>(event) : undefined;
      if (!run) return;
      finished = true;
      mine.close();
      report({ type: "done", run });
    });
    mine.addEventListener("error", () => {
      if (!live() || finished) return;
      // readyState 2 is the browser giving up on an answer it will not retry (a refusal such as 403,
      // 404 or 429); anything else is a connection that dropped.
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

// useACMERun follows a certificate run while jobId is set, and gives the state of the stream. The
// stream is opened again for another run and closed when the run is cleared or the component goes.
export function useACMERun(jobId: string | null): ACMERunStreamState {
  const [state, dispatch] = useReducer(
    (current: ACMERunStreamState, action: ACMERunFrame | { type: "reset" }) =>
      action.type === "reset" ? initialACMERunStream : reduceACMERunStream(current, action),
    initialACMERunStream,
  );
  useEffect(() => {
    dispatch({ type: "reset" });
    if (!jobId) return;
    const stream = createACMERunStream({ jobId, onFrame: dispatch });
    return () => stream.close();
  }, [jobId]);
  return state;
}
