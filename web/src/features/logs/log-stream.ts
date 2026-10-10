// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EventSourceLike } from "@/features/dashboard/events";
import type { LogLineData } from "@/features/logs/log-buffer";
import { apiUrl } from "@/lib/base-path";

// Where a log stream stands, as the viewer and its LiveChip need to know it.
// "connecting" is before the first ready event: there is nothing to claim yet.
// "reconnecting" is the browser retrying a stream that had been live. "closed" is
// final: the server said so (access_lost) or the browser gave up (failed).
export type LogStreamStatus = "connecting" | "live" | "reconnecting" | "closed";
export type LogCloseReason = "access_lost" | "failed";

export interface LogStreamState {
  status: LogStreamStatus;
  reason?: LogCloseReason;
}

// What the container itself is doing, as the server tells it when the engine ends
// the log.
export interface ContainerLogState {
  state: string;
  at: string;
}

export interface LogStreamOptions {
  envId: string;
  containerId: string;
  // The node the container is on; left out on an environment of one node.
  nodeId?: string;
  // The viewer's random ID (makeViewerId): the same on every stream, page and
  // reconnect of one viewer, so the server audits the viewer once (D-24).
  sid: string;
  // How many of the last lines to start with; the server's own default is 500.
  tail?: number | "all";
  onLines: (lines: LogLineData[]) => void;
  onState: (state: LogStreamState) => void;
  onReady?: (info: { tty: boolean }) => void;
  onContainerState?: (state: ContainerLogState) => void;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
}

export interface LogStream {
  // Ends the stream and closes the connection. Nothing is reported after it, and a
  // second call does nothing.
  close: () => void;
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
  o: Pick<LogStreamOptions, "envId" | "containerId" | "nodeId" | "sid" | "tail">,
): string {
  const q = new URLSearchParams({ sid: o.sid });
  if (o.nodeId) q.set("nodeId", o.nodeId);
  if (o.tail !== undefined) q.set("tail", String(o.tail));
  return apiUrl(
    `/api/env/${encodeURIComponent(o.envId)}/containers/${encodeURIComponent(o.containerId)}/logs/stream?${q}`,
  );
}

function parse<T>(event: Event): T | undefined {
  try {
    return JSON.parse((event as MessageEvent<string>).data) as T;
  } catch {
    return undefined;
  }
}

// createLogStream opens the live log of a container (GET .../logs/stream, server-sent
// events) and reports what arrives. The browser reconnects by itself when the
// connection drops and sends the id of the last event it saw, which is the timestamp
// of the last line, so the server resumes there.
export function createLogStream(options: LogStreamOptions): LogStream {
  const create = options.createSource ?? ((url: string) => new EventSource(url));
  let disposed = false;
  let connected = false;

  const report = (state: LogStreamState): void => {
    if (!disposed) options.onState(state);
  };

  const source = create(logStreamUrl(options));
  report({ status: "connecting" });

  source.addEventListener("ready", (event) => {
    if (disposed) return;
    connected = true;
    options.onReady?.({ tty: parse<{ tty: boolean }>(event)?.tty === true });
    report({ status: "live" });
  });
  source.addEventListener("log", (event) => {
    if (disposed) return;
    const body = parse<{ lines?: LogLineData[] }>(event);
    if (body?.lines?.length) options.onLines(body.lines);
  });
  source.addEventListener("state", (event) => {
    if (disposed) return;
    const body = parse<ContainerLogState>(event);
    if (body) options.onContainerState?.(body);
  });
  source.addEventListener("error", () => {
    if (disposed) return;
    // readyState 2 is the browser giving up (a refusal such as 401, 404 or 429, or a
    // stream it will not retry); anything else is it trying again.
    if (source.readyState === 2) {
      report({ status: "closed", reason: "failed" });
    } else if (connected) {
      report({ status: "reconnecting" });
    }
  });

  return {
    close: () => {
      if (disposed) return;
      disposed = true;
      source.close();
    },
  };
}
