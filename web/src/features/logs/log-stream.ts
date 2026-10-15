// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { api } from "@/api/client";
import type { EventSourceLike } from "@/features/dashboard/events";
import type { LogLineData } from "@/features/logs/log-buffer";
import type {
  LogSource,
  ServiceLogFilter,
  ServiceLogTask,
  ServiceMarkerEvent,
  ServiceNodeEvent,
} from "@/features/logs/log-source";
import { apiUrl } from "@/lib/base-path";

// Where a log stream stands, as the viewer and its LiveChip need to know it.
// "connecting" is before the first ready event: there is nothing to claim yet.
// "reconnecting" is the browser retrying a stream that had been live. "closed" is
// final: the server said so (access_lost) or the browser gave up (failed).
export type LogStreamStatus = "connecting" | "live" | "reconnecting" | "closed";
export type LogCloseReason = "access_lost" | "failed" | "unreadable";

export interface LogStreamState {
  status: LogStreamStatus;
  reason?: LogCloseReason;
}

// What the server says when the engine cannot read a container's log back (D-25): the
// log driver and its options. A value is present only for a caller who may reveal
// secrets; for anyone else `masked` is set and there is no value to find.
export interface UnreadableInfo {
  driver: string;
  options: Array<{ name: string; value?: string; masked: boolean }>;
}

// What the container itself is doing, as the server tells it when the engine ends
// the log.
export interface ContainerLogState {
  state: string;
  at: string;
}

// What a stream reads: a container on its node, given as containerId (and nodeId), or any
// source, a service among them (plan 06-29, D-35). A container caller keeps using
// containerId; a service caller gives the source and, to narrow it, the filter. Give one or
// the other; a source wins.
export interface StreamTarget {
  containerId?: string;
  nodeId?: string;
  source?: LogSource;
}

// sourceOf is the source a target names.
export function sourceOf(t: StreamTarget): LogSource {
  if (t.source) return t.source;
  return t.nodeId
    ? { kind: "container", id: t.containerId ?? "", nodeId: t.nodeId }
    : { kind: "container", id: t.containerId ?? "" };
}

export interface LogStreamOptions extends StreamTarget {
  envId: string;
  // The viewer's random ID (makeViewerId): the same on every stream, page and
  // reconnect of one viewer, so the server audits the viewer once (D-24).
  sid: string;
  // How many of the last lines to start with; the server's own default is 500.
  tail?: number | "all";
  // A time range, as ISO times: lines from since on, up to until if given. A range
  // that ends is not followed past its end.
  since?: string;
  until?: string;
  // The tasks, nodes and stopped switch a service's log is narrowed by; ignored for a
  // container.
  service?: ServiceLogFilter;
  onLines: (lines: LogLineData[]) => void;
  onState: (state: LogStreamState) => void;
  onReady?: (info: { tty: boolean; tasks?: ServiceLogTask[] }) => void;
  // A service's tasks changed: one was first seen after the stream opened.
  onTasks?: (tasks: ServiceLogTask[]) => void;
  // A task of the service was replaced, or a slot is waiting for one (D-34), and a node that
  // runs a followed task stopped reporting or is back (D-37).
  onMarker?: (marker: ServiceMarkerEvent) => void;
  onNode?: (notice: ServiceNodeEvent) => void;
  onContainerState?: (state: ContainerLogState) => void;
  // The engine cannot read this container's log. The stream is closed.
  onUnreadable?: (info: UnreadableInfo) => void;
  // The stream could not be resumed after a drop: the container is gone, so lines may
  // be missing. Called once, and the stream then closes (UI-SPEC "Stream dropped").
  onInterrupted?: () => void;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
  // Asks the server why a reopened stream was refused, as the HTTP status of a cheap
  // read of the same container or service (0 when the server could not be reached). An
  // EventSource cannot see the status of a refusal itself. The real read unless a
  // test supplies one.
  probe?: () => Promise<number>;
}

export interface LogStream {
  // Ends the stream and closes the connection. Nothing is reported after it, and a
  // second call does nothing.
  close: () => void;
  // Closes the connection of a stream nobody is looking at, keeping what it knows so
  // that resume can pick up from the last line. A suspended stream retries nothing.
  suspend: () => void;
  // Opens the connection again after suspend, from the last line, saying it is
  // reconnecting until the server answers. It does nothing for a stream that is not
  // suspended.
  resume: () => void;
}

const SID_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

// makeViewerId makes the random ID of a viewer: 22 URL-safe characters, which is what
// the server asks for (16 to 64 of A-Z a-z 0-9 - _).
export function makeViewerId(): string {
  const bytes = new Uint8Array(22);
  crypto.getRandomValues(bytes);
  let id = "";
  for (const b of bytes) id += SID_ALPHABET[b % SID_ALPHABET.length];
  return id;
}

export function logStreamUrl(
  o: StreamTarget &
    Pick<LogStreamOptions, "envId" | "sid" | "tail" | "since" | "until" | "service"> & {
      // Resume after the line with this stamp: the server then sends the lines written
      // since, with no tail, and nothing at or before it.
      after?: string;
    },
): string {
  const src = sourceOf(o);
  const q = new URLSearchParams({ sid: o.sid });
  if (src.kind === "container" && src.nodeId) q.set("nodeId", src.nodeId);
  if (o.after) {
    q.set("after", o.after);
    if (o.until) q.set("until", o.until);
  } else {
    if (o.tail !== undefined) q.set("tail", String(o.tail));
    if (o.since) q.set("since", o.since);
    if (o.until) q.set("until", o.until);
  }
  if (src.kind === "service") {
    if (o.service?.tasks?.length) q.set("task", o.service.tasks.join(","));
    if (o.service?.nodes?.length) q.set("node", o.service.nodes.join(","));
    if (o.service?.stopped) q.set("stopped", "1");
  }
  const what = src.kind === "service" ? "services" : "containers";
  return apiUrl(
    `/api/env/${encodeURIComponent(o.envId)}/${what}/${encodeURIComponent(src.id)}/logs/stream?${q}`,
  );
}

// How long a dropped stream waits before it is opened again: 1, 2 and 4 seconds, then
// 8 for every try after (UI-SPEC "Reconnects").
export const RECONNECT_DELAYS_MS = [1000, 2000, 4000, 8000] as const;

// What tells two lines with the same stamp apart: their stream and their text. A
// reopened stream may repeat the lines it already holds at its seam.
// A service's identical lines from two tasks at one stamp are two lines, so the task is part
// of the key.
const lineKey = (l: LogLineData): string => `${l.s}\u0000${l.task ?? ""}\u0000${l.m}`;

const text = (v: unknown): string | undefined => (typeof v === "string" ? v : undefined);
const count = (v: unknown): number | undefined =>
  typeof v === "number" && Number.isFinite(v) ? v : undefined;

// readMarker takes the marker event of a service's stream for what it should be, or nothing:
// only the fields the viewer uses, and only when they are the right kind of value.
function readMarker(body: unknown): ServiceMarkerEvent | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const b = body as Record<string, unknown>;
  const slot = count(b.slot);
  const old = text(b.old);
  if ((b.kind !== "replaced" && b.kind !== "stopped") || slot === undefined || !old) {
    return undefined;
  }
  const exitCode = count(b.exitCode);
  const next = text(b.new);
  const node = text(b.node);
  const at = text(b.at);
  return {
    kind: b.kind,
    slot,
    old,
    oldState: text(b.oldState) ?? "",
    oldNode: text(b.oldNode) ?? "",
    ...(exitCode !== undefined ? { exitCode } : {}),
    ...(next ? { new: next } : {}),
    ...(node ? { node } : {}),
    ...(at ? { at } : {}),
  };
}

// readNodeNotice does the same for the node event.
function readNodeNotice(body: unknown): ServiceNodeEvent | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const b = body as Record<string, unknown>;
  const node = text(b.node);
  if ((b.kind !== "down" && b.kind !== "disconnected" && b.kind !== "back") || !node) {
    return undefined;
  }
  const at = text(b.at);
  return { kind: b.kind, node, ...(at ? { at } : {}) };
}

function parse<T>(event: Event): T | undefined {
  try {
    return JSON.parse((event as MessageEvent<string>).data) as T;
  } catch {
    return undefined;
  }
}

// probeLog asks the server whether the container's log can still be read, by the status
// of a page of one line (GET .../logs). The page is audited with the viewer's own ID,
// so it writes no row of its own. A service's is asked of its tasks route, which needs the
// same permission.
async function probeLog(o: LogStreamOptions): Promise<number> {
  const src = sourceOf(o);
  try {
    if (src.kind === "service") {
      const res = await api.GET("/api/env/{envId}/services/{serviceId}/tasks", {
        params: { path: { envId: o.envId, serviceId: src.id } },
      });
      return res.response.status;
    }
    const res = await api.GET("/api/env/{envId}/containers/{containerId}/logs", {
      params: {
        path: { envId: o.envId, containerId: src.id },
        query: { sid: o.sid, until: new Date().toISOString(), tail: 1, nodeId: src.nodeId },
      },
    });
    return res.response.status;
  } catch {
    return 0;
  }
}

// createLogStream opens the live log of a container (GET .../logs/stream, server-sent
// events) and reports what arrives.
//
// A drop is handled here, not by the browser's own retry, so the wait follows the
// spec: the stream says it is reconnecting, closes the dead connection, and opens a new
// one after 1, 2, 4, then 8 seconds, each time with the same viewer ID (the server
// audits the viewer once) and `after` set to the newest stamp it holds, so the server
// sends only what came since. Lines at the seam that it already holds are dropped by
// stamp and text, whatever the server sent. A first attempt that fails is not retried:
// the viewer offers Retry. A reopened connection the server refuses is asked about: a
// container that is gone ends the stream with the interrupted notice; anything that may
// pass (a node that is briefly unreachable, a limit) is retried on the same schedule.
export function createLogStream(options: LogStreamOptions): LogStream {
  const create = options.createSource ?? ((url: string) => new EventSource(url));
  const probe = options.probe ?? (() => probeLog(options));
  let disposed = false;
  let everLive = false;
  let attempt = 0;
  let suspended = false;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let source: EventSourceLike;

  // The newest stamp received, and how many lines of each text were received at it.
  let newest = "";
  let atNewest = new Map<string, number>();
  // While set, lines that repeat what was held at a reopen are dropped; it ends with
  // the first line that does not.
  let seam: { stamp: string; counts: Map<string, number> } | null = null;

  const report = (state: LogStreamState): void => {
    if (!disposed) options.onState(state);
  };

  // Notes the lines handed on, for the next resume.
  const note = (lines: LogLineData[]): void => {
    for (const l of lines) {
      if (!l.t) continue;
      if (l.t > newest) {
        newest = l.t;
        atNewest = new Map();
      }
      if (l.t === newest) atNewest.set(lineKey(l), (atNewest.get(lineKey(l)) ?? 0) + 1);
    }
  };

  const unseen = (lines: LogLineData[]): LogLineData[] => {
    if (!seam) return lines;
    const out: LogLineData[] = [];
    for (const l of lines) {
      if (seam) {
        if (l.t && l.t < seam.stamp) continue;
        if (l.t === seam.stamp) {
          const k = lineKey(l);
          const left = seam.counts.get(k) ?? 0;
          if (left > 0) {
            seam.counts.set(k, left - 1);
            continue;
          }
        }
        seam = null;
      }
      out.push(l);
    }
    return out;
  };

  const schedule = (): void => {
    const delay = RECONNECT_DELAYS_MS[Math.min(attempt, RECONNECT_DELAYS_MS.length - 1)] as number;
    attempt += 1;
    retry = setTimeout(reopen, delay);
  };

  const refused = async (): Promise<void> => {
    const status = await probe();
    if (disposed || suspended) return;
    if (status === 404 || status === 409) {
      options.onInterrupted?.();
      report({ status: "closed", reason: "failed" });
    } else if (status === 401) {
      report({ status: "closed", reason: "failed" });
    } else if (status === 403) {
      report({ status: "closed", reason: "access_lost" });
    } else {
      schedule();
    }
  };

  const connect = (resume: boolean): void => {
    const after = resume && newest ? newest : undefined;
    source = create(logStreamUrl({ ...options, after }));
    const mine = source;
    const live = (): boolean => !disposed && !suspended && source === mine;

    mine.addEventListener("ready", (event) => {
      if (!live()) return;
      attempt = 0;
      everLive = true;
      const body = parse<{ tty?: boolean; tasks?: ServiceLogTask[] }>(event);
      options.onReady?.({
        tty: body?.tty === true,
        ...(Array.isArray(body?.tasks) ? { tasks: body.tasks } : {}),
      });
      report({ status: "live" });
    });
    // A service's tasks changed: the list in full, so the viewer need not diff.
    mine.addEventListener("tasks", (event) => {
      if (!live()) return;
      const body = parse<{ tasks?: ServiceLogTask[] }>(event);
      if (Array.isArray(body?.tasks)) options.onTasks?.(body.tasks);
    });
    // A task of the service was replaced, or a node dropped out or came back.
    mine.addEventListener("marker", (event) => {
      if (!live()) return;
      const marker = readMarker(parse<unknown>(event));
      if (marker) options.onMarker?.(marker);
    });
    mine.addEventListener("node", (event) => {
      if (!live()) return;
      const notice = readNodeNotice(parse<unknown>(event));
      if (notice) options.onNode?.(notice);
    });
    mine.addEventListener("log", (event) => {
      if (!live()) return;
      const body = parse<{ lines?: LogLineData[] }>(event);
      if (!body?.lines?.length) return;
      const fresh = unseen(body.lines);
      if (fresh.length === 0) return;
      note(fresh);
      options.onLines(fresh);
    });
    mine.addEventListener("state", (event) => {
      if (!live()) return;
      const body = parse<ContainerLogState>(event);
      if (body) options.onContainerState?.(body);
    });
    // The server ended the stream on purpose. Closing the connection here is what keeps
    // the browser from reconnecting into the refusal that follows.
    mine.addEventListener("closed", (event) => {
      if (!live()) return;
      const body = parse<{ reason?: string }>(event);
      mine.close();
      report({
        status: "closed",
        reason: body?.reason === "access_lost" ? "access_lost" : "failed",
      });
    });
    // The log cannot be read back: not an error, and nothing to retry.
    mine.addEventListener("unreadable", (event) => {
      if (!live()) return;
      const body = parse<Partial<UnreadableInfo>>(event);
      mine.close();
      options.onUnreadable?.({
        driver: typeof body?.driver === "string" ? body.driver : "",
        options: Array.isArray(body?.options) ? body.options : [],
      });
      report({ status: "closed", reason: "unreadable" });
    });
    mine.addEventListener("error", () => {
      if (!live()) return;
      // readyState 2 is the browser giving up on an answer it will not retry (a refusal
      // such as 401, 404 or 429); anything else is a connection that dropped.
      const wasRefused = mine.readyState === 2;
      mine.close();
      if (!everLive) {
        // The first load failed: the viewer says so and offers Retry.
        report({ status: "closed", reason: "failed" });
        return;
      }
      report({ status: "reconnecting" });
      if (wasRefused) void refused();
      else schedule();
    });
  };

  function reopen(): void {
    if (disposed) return;
    seam = newest ? { stamp: newest, counts: new Map(atNewest) } : null;
    connect(true);
  }

  report({ status: "connecting" });
  connect(false);

  return {
    close: () => {
      if (disposed) return;
      disposed = true;
      clearTimeout(retry);
      source.close();
    },
    suspend: () => {
      if (disposed || suspended) return;
      suspended = true;
      clearTimeout(retry);
      source.close();
    },
    resume: () => {
      if (disposed || !suspended) return;
      suspended = false;
      attempt = 0;
      if (everLive) report({ status: "reconnecting" });
      reopen();
    },
  };
}

// plusNanosecond is the stamp of a line one nanosecond later. "Everything before this
// moment" is exclusive, so asking for the page before a line at T by T + 1 ns takes in
// the lines that share T with it; the buffer then drops the ones it holds (Pitfall 12).
export function plusNanosecond(t: string): string {
  const m = /^(.*T\d\d:\d\d:\d\d)\.(\d{1,9})Z$/.exec(t);
  if (!m) return t;
  const frac = Number((m[2] as string).padEnd(9, "0")) + 1;
  if (frac < 1e9) return `${m[1]}.${String(frac).padStart(9, "0")}Z`;
  const next = new Date(`${m[1]}Z`).getTime() + 1000;
  return `${new Date(next).toISOString().slice(0, 19)}.000000000Z`;
}

export interface LogPage {
  lines: LogLineData[];
  // True when no earlier line exists.
  startOfLogs: boolean;
}

export interface LogPageOptions extends StreamTarget {
  envId: string;
  // The tasks, nodes and stopped switch a service's page is narrowed by; ignored for a
  // container.
  service?: ServiceLogFilter;
  sid: string;
  // Only lines written before this stamp.
  until: string;
  tail?: number;
}

// fetchLogPage reads a page of earlier lines (GET .../logs), oldest first. It throws
// when the server refuses or cannot be reached.
export async function fetchLogPage(o: LogPageOptions): Promise<LogPage> {
  const src = sourceOf(o);
  const res = await api.GET("/api/env/{envId}/containers/{containerId}/logs", {
    params: {
      path: { envId: o.envId, containerId: src.id },
      query: {
        sid: o.sid,
        until: o.until,
        tail: o.tail ?? 500,
        nodeId: src.kind === "container" ? src.nodeId : undefined,
      },
    },
  });
  if (!res.data) throw new Error(`Could not read earlier log lines (${res.response.status})`);
  return { lines: res.data.lines as LogLineData[], startOfLogs: res.data.startOfLogs };
}
