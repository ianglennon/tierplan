// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  createLogStream,
  fetchLogPage,
  type LogStreamOptions,
  type LogStreamState,
  logStreamUrl,
  makeViewerId,
  plusNanosecond,
} from "@/features/logs/log-stream";
import { waitUnderFakeClock } from "@/test-settle";

// The connection to a container's log stream (plan 04-19, OBS-01, D-24, P3 D-10).

const SID = "abcdefghijklmnopqrstuv";

beforeEach(() => {
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
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

  it("is reconnecting when a live stream drops, and live again at the next ready of the new connection", () => {
    vi.useFakeTimers();
    const { source, states } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
    vi.advanceTimersByTime(1000);
    const next = FakeEventSource.instances[1] as FakeEventSource;
    next.sendFrame("ready", { tty: false });
    expect(states.at(-1)).toEqual({ status: "live" });
  });

  it("is closed as failed when the very first attempt fails, so the viewer can offer Retry, and is not retried", () => {
    vi.useFakeTimers();
    const { source, states } = open();
    source.fail();
    expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
    expect(source.closed).toBe(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("is closed as failed when the browser gives up on the first attempt, as it does for a refusal", () => {
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

describe("a range", () => {
  it("is asked for with since and until", () => {
    const { source } = open({
      since: "2026-10-09T10:00:00.000Z",
      until: "2026-10-09T11:00:00.000Z",
      tail: 50000,
    });
    const url = new URL(source.url, "http://x");
    expect(url.searchParams.get("since")).toBe("2026-10-09T10:00:00.000Z");
    expect(url.searchParams.get("until")).toBe("2026-10-09T11:00:00.000Z");
    expect(url.searchParams.get("tail")).toBe("50000");
  });
});

describe("plusNanosecond", () => {
  it("adds one nanosecond to an engine stamp, carrying into the second", () => {
    expect(plusNanosecond("2026-10-09T12:00:00.000000000Z")).toBe("2026-10-09T12:00:00.000000001Z");
    expect(plusNanosecond("2026-10-09T12:00:00.123456789Z")).toBe("2026-10-09T12:00:00.123456790Z");
    expect(plusNanosecond("2026-10-09T12:59:59.999999999Z")).toBe("2026-10-09T13:00:00.000000000Z");
    expect(plusNanosecond("2026-12-31T23:59:59.999999999Z")).toBe("2027-01-01T00:00:00.000000000Z");
  });

  it("pads a stamp that has fewer digits and leaves anything else alone", () => {
    expect(plusNanosecond("2026-10-09T12:00:00.5Z")).toBe("2026-10-09T12:00:00.500000001Z");
    expect(plusNanosecond("not a stamp")).toBe("not a stamp");
  });
});

describe("a log the engine cannot read", () => {
  it("reports the driver and its options, closes the connection and closes the stream", () => {
    const onUnreadable = vi.fn();
    const { source, states } = open({ onUnreadable });
    source.sendFrame("unreadable", { driver: "none", options: [{ name: "tag", masked: true }] });
    expect(onUnreadable).toHaveBeenCalledWith({
      driver: "none",
      options: [{ name: "tag", masked: true }],
    });
    expect(source.closed).toBe(true);
    expect(states.at(-1)).toEqual({ status: "closed", reason: "unreadable" });
  });

  it("copes with a body that is not what it should be", () => {
    const onUnreadable = vi.fn();
    const { source } = open({ onUnreadable });
    source.sendFrame("unreadable", { driver: 7, options: "nope" });
    expect(onUnreadable).toHaveBeenCalledWith({ driver: "", options: [] });
  });
});

// Reconnecting (plan 04-21, UI-SPEC "Reconnects", D-24, Pitfall 12): a dropped stream is
// reopened by the page itself, after 1, 2, 4 and then 8 seconds, with the same viewer ID,
// resuming from the stamp of the last line it holds.
describe("reconnecting", () => {
  const T1 = "2026-10-09T12:00:01.000000000Z";
  const T2 = "2026-10-09T12:00:02.000000000Z";

  const send = (s: FakeEventSource, ...lines: Array<{ t: string; s?: string; m: string }>) =>
    s.sendFrame("log", { lines: lines.map((l) => ({ s: "o", ...l })) }, lines.at(-1)?.t ?? "");
  const current = () => FakeEventSource.instances.at(-1) as FakeEventSource;
  const query = (s: FakeEventSource) => new URL(s.url, "http://x").searchParams;

  it("closes the dropped connection and opens a new one after one second, with the same viewer ID, from the last line", () => {
    vi.useFakeTimers();
    const { source, lines } = open({ nodeId: "n1", tail: 500 });
    source.sendFrame("ready", { tty: false });
    send(source, { t: T1, m: "a" }, { t: T2, m: "b" });
    source.fail();
    expect(source.closed).toBe(true);
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);
    const q = query(current());
    expect(q.get("sid")).toBe(SID);
    expect(q.get("nodeId")).toBe("n1");
    expect(q.get("after")).toBe(T2);
    expect(q.has("tail")).toBe(false);
    expect(q.has("since")).toBe(false);
    send(current(), { t: "2026-10-09T12:00:03.000000000Z", m: "c" });
    expect(lines.map((l) => l.map((x) => (x as { m: string }).m))).toEqual([["a", "b"], ["c"]]);
  });

  it("backs off 1, 2, 4 and then 8 seconds while the reopened connections keep failing, staying at 8", () => {
    vi.useFakeTimers();
    const { source } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    const waits: number[] = [];
    for (let want = 1; want <= 6; want++) {
      const before = FakeEventSource.instances.length;
      let waited = 0;
      while (FakeEventSource.instances.length === before && waited < 20_000) {
        vi.advanceTimersByTime(1000);
        waited += 1000;
      }
      waits.push(waited);
      current().fail();
    }
    expect(waits).toEqual([1000, 2000, 4000, 8000, 8000, 8000]);
  });

  it("starts again at one second after a reconnect succeeded", () => {
    vi.useFakeTimers();
    const { source } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    vi.advanceTimersByTime(1000);
    current().fail();
    vi.advanceTimersByTime(2000);
    current().sendFrame("ready", { tty: false });
    current().fail();
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(3);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(4);
  });

  it("keeps saying Reconnecting while it retries, with no chip lost and no closed state", () => {
    vi.useFakeTimers();
    const { source, states } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    vi.advanceTimersByTime(1000);
    current().fail();
    expect(states.filter((s) => s.status === "closed")).toEqual([]);
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
  });

  it("reopens a stream that has not yet had a line as it was first opened, with no after", () => {
    vi.useFakeTimers();
    const { source } = open({ tail: 500 });
    source.sendFrame("ready", { tty: false });
    source.fail();
    vi.advanceTimersByTime(1000);
    const q = query(current());
    expect(q.has("after")).toBe(false);
    expect(q.get("tail")).toBe("500");
  });

  it("keeps the end of a range when it resumes", () => {
    vi.useFakeTimers();
    const { source } = open({
      since: "2026-10-09T10:00:00.000Z",
      until: "2026-10-09T13:00:00.000Z",
      tail: 50000,
    });
    source.sendFrame("ready", { tty: false });
    send(source, { t: T1, m: "a" });
    source.fail();
    vi.advanceTimersByTime(1000);
    const q = query(current());
    expect(q.get("until")).toBe("2026-10-09T13:00:00.000Z");
    expect(q.get("after")).toBe(T1);
    expect(q.has("since")).toBe(false);
  });

  it("drops the lines of the seam that it already holds, by stamp and text, and keeps a different line at the same stamp", () => {
    vi.useFakeTimers();
    const { source, lines } = open();
    source.sendFrame("ready", { tty: false });
    send(source, { t: T1, m: "a" }, { t: T2, m: "b" }, { t: T2, m: "b" });
    source.fail();
    vi.advanceTimersByTime(1000);
    const next = current();
    next.sendFrame("ready", { tty: false });
    send(
      next,
      { t: T1, m: "a" },
      { t: T2, m: "b" },
      { t: T2, m: "b" },
      { t: T2, m: "other" },
      { t: "2026-10-09T12:00:03.000000000Z", m: "c" },
    );
    expect(
      lines
        .slice(1)
        .flat()
        .map((l) => (l as { m: string }).m),
    ).toEqual(["other", "c"]);
  });

  it("stops dropping once a new line has come, so later equal lines are kept", () => {
    vi.useFakeTimers();
    const { source, lines } = open();
    source.sendFrame("ready", { tty: false });
    send(source, { t: T2, m: "b" });
    source.fail();
    vi.advanceTimersByTime(1000);
    const next = current();
    send(next, { t: "2026-10-09T12:00:03.000000000Z", m: "b" });
    send(next, { t: T2, m: "b" });
    expect(lines.slice(1).flat()).toHaveLength(2);
  });

  it("does nothing more once closed, and cancels a retry that was waiting", () => {
    vi.useFakeTimers();
    const { stream, source, states } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    const seen = states.length;
    stream.close();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(states).toHaveLength(seen);
  });

  describe("when the reopened connection is refused", () => {
    const refuse = async (status: number) => {
      vi.useFakeTimers();
      const probe = vi.fn(async () => status);
      const onInterrupted = vi.fn();
      const o = open({ probe, onInterrupted });
      o.source.sendFrame("ready", { tty: false });
      send(o.source, { t: T1, m: "a" });
      o.source.fail();
      vi.advanceTimersByTime(1000);
      current().fail(true);
      await vi.advanceTimersByTimeAsync(0);
      return { ...o, probe, onInterrupted };
    };

    it("reports the logs interrupted once and gives up when the container is gone (404)", async () => {
      const { states, onInterrupted, probe } = await refuse(404);
      expect(probe).toHaveBeenCalledTimes(1);
      expect(onInterrupted).toHaveBeenCalledTimes(1);
      expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
      vi.advanceTimersByTime(60_000);
      expect(FakeEventSource.instances).toHaveLength(2);
    });

    it("keeps retrying with the backoff when the node is only unreachable (503)", async () => {
      const { states, onInterrupted } = await refuse(503);
      expect(onInterrupted).not.toHaveBeenCalled();
      expect(states.at(-1)).toEqual({ status: "reconnecting" });
      vi.advanceTimersByTime(2000);
      expect(FakeEventSource.instances).toHaveLength(3);
    });

    it("keeps retrying when the probe itself cannot reach the server", async () => {
      const { states, onInterrupted } = await refuse(0);
      expect(onInterrupted).not.toHaveBeenCalled();
      expect(states.at(-1)).toEqual({ status: "reconnecting" });
    });

    it("says the access changed when the server refuses with 403", async () => {
      const { states } = await refuse(403);
      expect(states.at(-1)).toEqual({ status: "closed", reason: "access_lost" });
    });

    it("gives up without the interrupted row when the session is gone (401)", async () => {
      const { states, onInterrupted } = await refuse(401);
      expect(onInterrupted).not.toHaveBeenCalled();
      expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
    });

    it("ignores the probe's answer when the stream was closed meanwhile", async () => {
      vi.useFakeTimers();
      let answer: (n: number) => void = () => {};
      const probe = vi.fn(() => new Promise<number>((r) => (answer = r)));
      const onInterrupted = vi.fn();
      const { stream, source, states } = open({ probe, onInterrupted });
      source.sendFrame("ready", { tty: false });
      source.fail();
      vi.advanceTimersByTime(1000);
      current().fail(true);
      stream.close();
      answer(404);
      await vi.advanceTimersByTimeAsync(0);
      expect(onInterrupted).not.toHaveBeenCalled();
      expect(states.at(-1)).toEqual({ status: "reconnecting" });
    });
  });
});

// Pausing a stream nobody is looking at (plan 04-21, D-22): the pop-out closes it after a
// minute hidden and reopens it from the last line when it is seen again.
describe("suspending", () => {
  const T1 = "2026-10-09T12:00:01.000000000Z";
  const current = () => FakeEventSource.instances.at(-1) as FakeEventSource;

  it("closes the connection and does not retry while suspended", () => {
    vi.useFakeTimers();
    const { stream, source, states } = open();
    source.sendFrame("ready", { tty: false });
    const seen = states.length;
    stream.suspend();
    expect(source.closed).toBe(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(states).toHaveLength(seen);
  });

  it("reopens from the last line, with the same viewer ID, and says it is reconnecting until it is back", () => {
    vi.useFakeTimers();
    const { stream, source, states } = open({ tail: 500 });
    source.sendFrame("ready", { tty: false });
    source.sendFrame("log", { lines: [{ t: T1, s: "o", m: "a" }] }, T1);
    stream.suspend();
    stream.resume();
    expect(FakeEventSource.instances).toHaveLength(2);
    const q = new URL(current().url, "http://x").searchParams;
    expect(q.get("after")).toBe(T1);
    expect(q.get("sid")).toBe(SID);
    expect(q.has("tail")).toBe(false);
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
    current().sendFrame("ready", { tty: false });
    expect(states.at(-1)).toEqual({ status: "live" });
  });

  it("drops what it holds at the seam when it resumes", () => {
    vi.useFakeTimers();
    const { stream, source, lines } = open();
    source.sendFrame("ready", { tty: false });
    source.sendFrame("log", { lines: [{ t: T1, s: "o", m: "a" }] }, T1);
    stream.suspend();
    stream.resume();
    current().sendFrame("log", {
      lines: [
        { t: T1, s: "o", m: "a" },
        { t: "2026-10-09T12:00:02.000000000Z", s: "o", m: "b" },
      ],
    });
    expect(lines.flat().map((l) => (l as { m: string }).m)).toEqual(["a", "b"]);
  });

  it("does nothing when resumed without being suspended, or after close", () => {
    vi.useFakeTimers();
    const { stream, source } = open();
    source.sendFrame("ready", { tty: false });
    stream.resume();
    expect(FakeEventSource.instances).toHaveLength(1);
    stream.suspend();
    stream.close();
    stream.resume();
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("cancels a retry that was waiting when it is suspended", () => {
    vi.useFakeTimers();
    const { stream, source } = open();
    source.sendFrame("ready", { tty: false });
    source.fail();
    stream.suspend();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    stream.resume();
    expect(FakeEventSource.instances).toHaveLength(2);
  });
});

// A service's merged log (plan 06-29, OBS-02, D-35, D-37): the same stream, addressed by the
// service, narrowed by the filters and naming the task of every line.
describe("a service's log", () => {
  const web = { kind: "service", id: "web" } as const;

  function openService(extra: Partial<LogStreamOptions> = {}) {
    const states: LogStreamState[] = [];
    const lines: unknown[][] = [];
    const stream = createLogStream({
      envId: "e1",
      source: web,
      sid: SID,
      onLines: (l) => lines.push(l),
      onState: (s) => states.push(s),
      ...extra,
    } as LogStreamOptions);
    const source = FakeEventSource.instances.at(-1);
    if (!source) throw new Error("no source");
    return { stream, source, states, lines };
  }

  it("is addressed by the service, with the viewer, the tail and the filters, and no node", () => {
    const url = new URL(
      logStreamUrl({
        envId: "e 1",
        source: { kind: "service", id: "web/api" },
        sid: SID,
        tail: 500,
        service: { tasks: ["t1", "t2"], nodes: ["alpha"], stopped: true },
      }),
      "http://x",
    );
    expect(url.pathname).toBe("/api/env/e%201/services/web%2Fapi/logs/stream");
    expect(url.searchParams.get("sid")).toBe(SID);
    expect(url.searchParams.get("tail")).toBe("500");
    expect(url.searchParams.get("task")).toBe("t1,t2");
    expect(url.searchParams.get("node")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("1");
    expect(url.searchParams.has("nodeId")).toBe(false);
  });

  it("leaves out filters that are empty and stopped when it is off", () => {
    const url = new URL(
      logStreamUrl({
        envId: "e1",
        source: web,
        sid: SID,
        service: { tasks: [], nodes: [], stopped: false },
      }),
      "http://x",
    );
    expect(url.searchParams.has("task")).toBe(false);
    expect(url.searchParams.has("node")).toBe(false);
    expect(url.searchParams.has("stopped")).toBe(false);
  });

  it("resumes with after and the same filters, and no tail", () => {
    vi.useFakeTimers();
    const { source } = openService({ tail: 500, service: { tasks: ["t1"] } });
    source.sendFrame("ready", { tty: false, tasks: [] });
    source.sendFrame(
      "log",
      {
        lines: [
          { t: "2026-10-14T10:00:00.000000000Z", s: "o", m: "x", task: "t1", slot: 1, node: "a" },
        ],
      },
      "2026-10-14T10:00:00.000000000Z",
    );
    source.fail();
    vi.advanceTimersByTime(1000);
    const next = FakeEventSource.instances.at(-1) as FakeEventSource;
    const url = new URL(next.url, "http://x");
    expect(url.searchParams.get("after")).toBe("2026-10-14T10:00:00.000000000Z");
    expect(url.searchParams.has("tail")).toBe(false);
    expect(url.searchParams.get("task")).toBe("t1");
  });

  it("tells onReady the tasks it was given and onTasks each time the list changes", () => {
    const onReady = vi.fn();
    const onTasks = vi.fn();
    const { source } = openService({ onReady, onTasks });
    const tasks = [{ id: "t1", slot: 1, node: "alpha", state: "running" }];
    source.sendFrame("ready", { tty: false, tasks });
    expect(onReady).toHaveBeenCalledWith({ tty: false, tasks });
    const more = [...tasks, { id: "t2", slot: 2, node: "beta", state: "running" }];
    source.sendFrame("tasks", { tty: false, tasks: more });
    expect(onTasks).toHaveBeenCalledWith(more);
  });

  it("hands on the task, slot and node of every line", () => {
    const { source, lines } = openService();
    source.sendFrame("ready", { tty: false, tasks: [] });
    const line = {
      t: "2026-10-14T10:00:00.000000000Z",
      s: "o",
      m: "x",
      task: "t1",
      slot: 2,
      node: "alpha",
    };
    source.sendFrame("log", { lines: [line] }, line.t);
    expect(lines).toEqual([[line]]);
  });

  it("keeps two identical lines of different tasks that share a stamp apart at a reopen", () => {
    vi.useFakeTimers();
    const { source, lines } = openService();
    source.sendFrame("ready", { tty: false, tasks: [] });
    const t = "2026-10-14T10:00:00.000000000Z";
    const a = { t, s: "o", m: "tick", task: "t1", slot: 1, node: "alpha" };
    const b = { t, s: "o", m: "tick", task: "t2", slot: 2, node: "beta" };
    source.sendFrame("log", { lines: [a] }, t);
    source.fail();
    vi.advanceTimersByTime(1000);
    const next = FakeEventSource.instances.at(-1) as FakeEventSource;
    next.sendFrame("ready", { tty: false, tasks: [] });
    next.sendFrame("log", { lines: [a, b] }, t);
    expect(lines.flat()).toEqual([a, b]);
  });

  it("asks the tasks route why a reopened stream was refused, and ends on a service that is gone", () => {
    vi.useFakeTimers();
    const onInterrupted = vi.fn();
    const probe = vi.fn().mockResolvedValue(404);
    const { source, states } = openService({ onInterrupted, probe });
    source.sendFrame("ready", { tty: false, tasks: [] });
    source.fail(true);
    return waitUnderFakeClock(() => {
      expect(probe).toHaveBeenCalled();
      expect(onInterrupted).toHaveBeenCalled();
      expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
    });
  });
});

// The marker and node events of a service's stream (plan 06-30, D-34, D-37).
describe("a service's markers and node notices", () => {
  const web = { kind: "service", id: "web" } as const;

  function openService(extra: Partial<LogStreamOptions> = {}) {
    const markers: unknown[] = [];
    const notices: unknown[] = [];
    const stream = createLogStream({
      envId: "e1",
      source: web,
      sid: SID,
      onLines: () => {},
      onState: () => {},
      onMarker: (m) => markers.push(m),
      onNode: (n) => notices.push(n),
      ...extra,
    } as LogStreamOptions);
    const source = FakeEventSource.instances.at(-1);
    if (!source) throw new Error("no source");
    return { stream, source, markers, notices };
  }

  it("hands on a marker event", () => {
    const { source, markers } = openService();
    source.sendFrame("ready", { tty: false, tasks: [] });
    const m = {
      kind: "replaced",
      slot: 2,
      old: "t1",
      oldState: "failed",
      oldNode: "alpha",
      exitCode: 1,
      new: "t2",
      node: "beta",
      at: "2026-10-14T10:00:02.000000000Z",
    };
    source.sendFrame("marker", m);
    expect(markers).toEqual([m]);
  });

  it("hands on a stopped marker that has no new task", () => {
    const { source, markers } = openService();
    source.sendFrame("marker", {
      kind: "stopped",
      slot: 2,
      old: "t1",
      oldState: "rejected",
      oldNode: "alpha",
    });
    expect(markers).toEqual([
      { kind: "stopped", slot: 2, old: "t1", oldState: "rejected", oldNode: "alpha" },
    ]);
  });

  it("hands on a node event", () => {
    const { source, notices } = openService();
    source.sendFrame("node", { kind: "down", node: "beta", at: "2026-10-14T10:00:02.000Z" });
    expect(notices).toEqual([{ kind: "down", node: "beta", at: "2026-10-14T10:00:02.000Z" }]);
  });

  it("ignores events that are not what the server sends", () => {
    const { source, markers, notices } = openService();
    source.sendFrame("marker", { kind: "exploded", slot: 1, old: "t1" });
    source.sendFrame("marker", { kind: "replaced", slot: "x" });
    source.sendFrame("marker", "nonsense");
    source.sendFrame("node", { kind: "sideways", node: "beta" });
    source.sendFrame("node", { kind: "down" });
    expect(markers).toEqual([]);
    expect(notices).toEqual([]);
  });

  it("says nothing once it is closed", () => {
    const { stream, source, markers } = openService();
    stream.close();
    source.sendFrame("marker", {
      kind: "stopped",
      slot: 1,
      old: "t1",
      oldState: "failed",
      oldNode: "a",
    });
    expect(markers).toEqual([]);
  });
});

// A page of earlier lines of a service's log (plan 06-31, OBS-02, D-35).
describe("a page of a service's log", () => {
  function stubPage(body: unknown = { lines: [], startOfLogs: true }) {
    const fetchMock = vi.fn(async (_req: Request) => Response.json(body));
    vi.stubGlobal("fetch", fetchMock);
    return fetchMock;
  }
  const requestUrl = (m: ReturnType<typeof stubPage>) =>
    new URL((m.mock.calls[0] as [Request])[0].url);

  it("asks the service's page route with the viewer, the tail and the filters, and no node", async () => {
    const m = stubPage();
    await fetchLogPage({
      envId: "e 1",
      source: { kind: "service", id: "web/api" },
      sid: SID,
      until: "2026-10-14T10:00:05.000000001Z",
      tail: 300,
      service: { tasks: ["t1", "t2"], nodes: ["alpha"], stopped: true },
    });
    const url = requestUrl(m);
    expect(url.pathname).toBe("/api/env/e%201/services/web%2Fapi/logs");
    expect(url.searchParams.get("sid")).toBe(SID);
    expect(url.searchParams.get("until")).toBe("2026-10-14T10:00:05.000000001Z");
    expect(url.searchParams.get("tail")).toBe("300");
    expect(url.searchParams.get("task")).toBe("t1,t2");
    expect(url.searchParams.get("node")).toBe("alpha");
    expect(url.searchParams.get("stopped")).toBe("true");
    expect(url.searchParams.has("nodeId")).toBe(false);
  });

  it("leaves out the filters that are not set", async () => {
    const m = stubPage();
    await fetchLogPage({
      envId: "e1",
      source: { kind: "service", id: "web" },
      sid: SID,
      until: "2026-10-14T10:00:05.000000001Z",
    });
    const url = requestUrl(m);
    expect(url.searchParams.get("tail")).toBe("500");
    expect(["task", "node", "stopped"].some((k) => url.searchParams.has(k))).toBe(false);
  });

  it("gives back the lines of the page, with the task of each, and whether it is the start", async () => {
    stubPage({
      lines: [
        { t: "2026-10-14T10:00:01.000000000Z", s: "o", m: "hi", task: "t1", slot: 1, node: "a" },
      ],
      startOfLogs: true,
    });
    const page = await fetchLogPage({
      envId: "e1",
      source: { kind: "service", id: "web" },
      sid: SID,
      until: "2026-10-14T10:00:05.000000001Z",
    });
    expect(page.startOfLogs).toBe(true);
    expect(page.lines).toEqual([
      { t: "2026-10-14T10:00:01.000000000Z", s: "o", m: "hi", task: "t1", slot: 1, node: "a" },
    ]);
  });

  it("still reads a container's page from the container's route", async () => {
    const m = stubPage();
    await fetchLogPage({
      envId: "e1",
      containerId: "c1",
      nodeId: "n1",
      sid: SID,
      until: "2026-10-14T10:00:05.000000001Z",
    });
    const url = requestUrl(m);
    expect(url.pathname).toBe("/api/env/e1/containers/c1/logs");
    expect(url.searchParams.get("nodeId")).toBe("n1");
  });

  it("throws when the server refuses", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => Response.json({}, { status: 503 })),
    );
    await expect(
      fetchLogPage({
        envId: "e1",
        source: { kind: "service", id: "web" },
        sid: SID,
        until: "2026-10-14T10:00:05.000000001Z",
      }),
    ).rejects.toThrow();
  });
});
