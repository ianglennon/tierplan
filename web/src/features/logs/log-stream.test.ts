// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  createLogStream,
  type LogStreamOptions,
  type LogStreamState,
  logStreamUrl,
  makeViewerId,
} from "@/features/logs/log-stream";

// The connection to a container's log stream (plan 04-19, OBS-01, D-24, P3 D-10).

const SID = "abcdefghijklmnopqrstuv";

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function open(extra: Partial<LogStreamOptions> = {}) {
  const states: LogStreamState[] = [];
  const lines: unknown[][] = [];
  const stream = createLogStream({
    envId: "e1",
    containerId: "web-id",
    sid: SID,
    onLines: (l) => lines.push(l),
    onState: (s) => states.push(s),
    ...extra,
  });
  const source = FakeEventSource.instances[0];
  if (!source) throw new Error("no source");
  return { stream, source, states, lines };
}

describe("the address", () => {
  it("names the environment, the container, the viewer and, when given, the node and the tail", () => {
    const url = new URL(
      logStreamUrl({ envId: "e 1", containerId: "web/id", sid: SID, nodeId: "n1", tail: 500 }),
      "http://x",
    );
    expect(url.pathname).toBe("/api/env/e%201/containers/web%2Fid/logs/stream");
    expect(url.searchParams.get("sid")).toBe(SID);
    expect(url.searchParams.get("nodeId")).toBe("n1");
    expect(url.searchParams.get("tail")).toBe("500");
  });

  it("leaves the node and the tail out when there are none", () => {
    const url = new URL(logStreamUrl({ envId: "e1", containerId: "c", sid: SID }), "http://x");
    expect(url.searchParams.has("nodeId")).toBe(false);
    expect(url.searchParams.has("tail")).toBe(false);
  });

  it("makes a viewer ID of the shape the server asks for, a different one each time", () => {
    const ids = new Set(Array.from({ length: 20 }, makeViewerId));
    expect(ids.size).toBe(20);
    for (const id of ids) expect(id).toMatch(/^[A-Za-z0-9_-]{22}$/);
  });
});

describe("the state", () => {
  it("is connecting at once, live when the server says ready and tells whether the container has a TTY", () => {
    const onReady = vi.fn();
    const { source, states } = open({ onReady });
    expect(states).toEqual([{ status: "connecting" }]);
    source.open();
    expect(states).toEqual([{ status: "connecting" }]);
    source.sendFrame("ready", { tty: true });
    expect(states.at(-1)).toEqual({ status: "live" });
    expect(onReady).toHaveBeenCalledWith({ tty: true });
  });

  it("is reconnecting when a live stream drops and the browser is trying again, live again at the next ready", () => {
    const { source, states } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
    source.sendFrame("ready", { tty: false });
    expect(states.at(-1)).toEqual({ status: "live" });
  });

  it("says nothing of a failed first attempt the browser is retrying, so the viewer keeps its skeleton", () => {
    const { source, states } = open();
    source.fail();
    expect(states).toEqual([{ status: "connecting" }]);
  });

  it("is closed as failed when the browser gives up, as it does for a refusal", () => {
    const { source, states } = open();
    source.fail(true);
    expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
  });

  it("is closed as access_lost when the server says so, and closes the connection so the browser does not retry", () => {
    const { source, states } = open();
    source.sendFrame("ready", { tty: false });
    source.sendFrame("closed", { reason: "access_lost" });
    expect(states.at(-1)).toEqual({ status: "closed", reason: "access_lost" });
    expect(source.closed).toBe(true);
  });

  it("takes any other reason of a closed event as a failure", () => {
    const { source, states } = open();
    source.sendFrame("closed", { reason: "something new" });
    expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
  });
});

describe("the lines", () => {
  it("hands over the lines of each log event, in order", () => {
    const { source, lines } = open();
    source.sendFrame("ready", { tty: false });
    source.sendFrame(
      "log",
      {
        lines: [
          { t: "1", s: "o", m: "a" },
          { t: "2", s: "e", m: "b" },
        ],
      },
      "2",
    );
    source.sendFrame("log", { lines: [{ t: "3", s: "o", m: "c" }] }, "3");
    expect(lines.map((l) => l.map((x) => (x as { m: string }).m))).toEqual([["a", "b"], ["c"]]);
  });

  it("ignores a frame it cannot read and one with no lines", () => {
    const { source, lines } = open();
    source.sendFrame("log", {});
    source.sendFrame("log", { lines: [] });
    (source as unknown as { emit: (e: Event) => void }).emit(
      new MessageEvent("log", { data: "not json" }),
    );
    expect(lines).toEqual([]);
  });

  it("reports what the server says of the container itself", () => {
    const onContainerState = vi.fn();
    const { source } = open({ onContainerState });
    source.sendFrame("state", { state: "stopped", at: "2026-10-09T12:00:00Z" });
    expect(onContainerState).toHaveBeenCalledWith({ state: "stopped", at: "2026-10-09T12:00:00Z" });
  });
});

describe("closing", () => {
  it("closes the connection when disposed and reports nothing after it", () => {
    const { stream, source, states, lines } = open();
    source.sendFrame("ready", { tty: false });
    const before = states.length;
    stream.close();
    expect(source.closed).toBe(true);
    source.sendFrame("log", { lines: [{ t: "1", s: "o", m: "late" }] });
    source.sendFrame("ready", { tty: false });
    source.fail(true);
    expect(lines).toEqual([]);
    expect(states).toHaveLength(before);
  });

  it("can be closed twice", () => {
    const { stream, source } = open();
    stream.close();
    stream.close();
    expect(source.closed).toBe(true);
  });
});
