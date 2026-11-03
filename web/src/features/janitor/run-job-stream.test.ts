// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { JanitorJobFrame, JanitorJobItemFrame } from "@/api/janitor";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  initialJanitorStream,
  type JanitorStreamFrame,
  janitorRunStreamUrl,
  openJanitorRunStream,
  progressOf,
  reduceJanitorStream,
} from "@/features/janitor/run-job-stream";

// The connection to a clean-up's stream (plan 11-16, JAN-09, D-14).

const item = (over: Partial<JanitorJobItemFrame> = {}): JanitorJobItemFrame => ({
  id: "container/n1/c1",
  label: "web.1.abc",
  kind: "container",
  state: "waiting",
  code: "",
  message: "",
  result: {},
  ...over,
});

const four = (): JanitorJobItemFrame[] =>
  [1, 2, 3, 4].map((n) => item({ id: `container/n1/c${n}`, label: `web.${n}.abc` }));

const job = (over: Partial<JanitorJobFrame> = {}): JanitorJobFrame => ({
  id: "j1",
  runId: "r1",
  state: "running",
  trigger: "manual",
  by: "Ada",
  createdAt: "2026-11-03T12:00:00Z",
  finishedAt: "",
  done: 0,
  total: 4,
  items: four(),
  ...over,
});

describe("the reducer", () => {
  it("starts with nothing and is connecting", () => {
    expect(initialJanitorStream).toEqual({
      status: "connecting",
      finished: false,
      gone: false,
      accessLost: false,
    });
  });

  it("takes a snapshot as the job and is live", () => {
    const next = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    expect(next.job?.id).toBe("j1");
    expect(next.status).toBe("live");
    expect(next.finished).toBe(false);
    expect(progressOf(next.job as JanitorJobFrame)).toEqual({ done: 0, total: 4 });
  });

  it("takes a snapshot of a job that has ended as finished", () => {
    const next = reduceJanitorStream(initialJanitorStream, {
      type: "snapshot",
      job: job({ state: "done" }),
    });
    expect(next.finished).toBe(true);
  });

  it("counts the removals that have ended from the items, so an item moves the progress", () => {
    let state = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    state = reduceJanitorStream(state, {
      type: "item",
      item: item({ id: "container/n1/c1", label: "web.1.abc", state: "running" }),
    });
    // Working on one is not done with it.
    expect(state.job).toMatchObject({ done: 0, total: 4 });
    state = reduceJanitorStream(state, {
      type: "item",
      item: item({ id: "container/n1/c1", label: "web.1.abc", state: "done" }),
    });
    state = reduceJanitorStream(state, {
      type: "item",
      item: item({ id: "container/n1/c2", label: "web.2.abc", state: "failed", code: "in_use" }),
    });
    expect(state.job).toMatchObject({ done: 2, total: 4 });
    expect(state.job?.items.map((i) => i.state)).toEqual(["done", "failed", "waiting", "waiting"]);
  });

  it("adds an item it has not seen, keeping the order, and ignores one before there is a job", () => {
    expect(reduceJanitorStream(initialJanitorStream, { type: "item", item: item() })).toBe(
      initialJanitorStream,
    );
    const start = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    const added = reduceJanitorStream(start, {
      type: "item",
      item: item({ id: "network/n1/net1", label: "net1", kind: "network" }),
    });
    expect(added.job?.items.map((i) => i.label)).toEqual([
      "web.1.abc",
      "web.2.abc",
      "web.3.abc",
      "web.4.abc",
      "net1",
    ]);
    expect(added.job?.total).toBe(5);
  });

  it("repeating an item changes nothing", () => {
    const start = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    const frame: JanitorStreamFrame = { type: "item", item: item({ state: "done" }) };
    const once = reduceJanitorStream(start, frame);
    expect(reduceJanitorStream(once, frame)).toEqual(once);
  });

  it("ends the job and the stream on done, and never goes back to live", () => {
    const start = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    const done = reduceJanitorStream(start, {
      type: "done",
      job: job({ state: "done", finishedAt: "2026-11-03T12:00:09Z" }),
    });
    expect(done).toMatchObject({ status: "closed", finished: true });
    expect(reduceJanitorStream(done, { type: "status", status: "reconnecting" })).toBe(done);
    expect(reduceJanitorStream(done, { type: "snapshot", job: job() }).status).toBe("closed");
  });

  it("marks a job as gone or unreadable, and forgets everything on reset", () => {
    expect(
      reduceJanitorStream(initialJanitorStream, { type: "status", status: "closed", gone: true }),
    ).toMatchObject({ gone: true, status: "closed" });
    expect(
      reduceJanitorStream(initialJanitorStream, {
        type: "status",
        status: "closed",
        accessLost: true,
      }),
    ).toMatchObject({ accessLost: true });
    const live = reduceJanitorStream(initialJanitorStream, { type: "snapshot", job: job() });
    expect(reduceJanitorStream(live, { type: "reset" })).toEqual(initialJanitorStream);
  });
});

describe("the address", () => {
  it("names the run and puts the last event in the query", () => {
    expect(janitorRunStreamUrl("e1", "r1")).toBe("/api/env/e1/janitor/runs/r1/stream");
    expect(janitorRunStreamUrl("e1", "r1", "12")).toBe(
      "/api/env/e1/janitor/runs/r1/stream?lastEventId=12",
    );
    expect(janitorRunStreamUrl("e 1", "r/1")).toBe("/api/env/e%201/janitor/runs/r%2F1/stream");
  });
});

describe("the connection", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  const open = (probe?: () => Promise<number>) => {
    const frames: JanitorStreamFrame[] = [];
    const stream = openJanitorRunStream({
      envId: "e1",
      runId: "r1",
      onFrame: (f) => frames.push(f),
      createSource: (url) => new FakeEventSource(url),
      probe,
    });
    return { frames, stream };
  };
  const src = (n = 0) => FakeEventSource.instances[n] as FakeEventSource;

  it("reports a snapshot, the items and done, then closes", () => {
    const { frames } = open();
    src().sendFrame("snapshot", job());
    src().sendFrame("item", item({ state: "running" }), "1");
    src().sendFrame("done", job({ state: "done" }), "2");
    expect(frames.map((f) => f.type)).toEqual(["snapshot", "item", "done"]);
    expect(src().closed).toBe(true);
  });

  it("reconnects after 1, 2, 4 then 8 seconds, naming the last event it saw", async () => {
    const { frames } = open();
    src().sendFrame("snapshot", job());
    src().sendFrame("item", item(), "5");
    src().fail();
    expect(frames.at(-1)).toEqual({ type: "status", status: "reconnecting" });
    expect(FakeEventSource.instances).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(src(1).url).toMatch(/\?lastEventId=5$/);
    // No snapshot arrived, so the wait doubles.
    src(1).fail();
    await vi.advanceTimersByTimeAsync(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeEventSource.instances).toHaveLength(3);
    src(2).fail();
    await vi.advanceTimersByTimeAsync(4000);
    expect(FakeEventSource.instances).toHaveLength(4);
    src(3).fail();
    await vi.advanceTimersByTimeAsync(8000);
    expect(FakeEventSource.instances).toHaveLength(5);
    src(4).fail();
    await vi.advanceTimersByTimeAsync(8000);
    expect(FakeEventSource.instances).toHaveLength(6);
  });

  it("starts the wait over once a snapshot arrives", async () => {
    open();
    src().fail();
    await vi.advanceTimersByTimeAsync(1000);
    src(1).fail();
    await vi.advanceTimersByTimeAsync(2000);
    src(2).sendFrame("snapshot", job());
    src(2).fail();
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(4);
  });

  it("ends the stream for a job that is gone and for one it may not read, and retries anything else", async () => {
    for (const [status, expected] of [
      [404, { type: "status", status: "closed", gone: true }],
      [403, { type: "status", status: "closed", accessLost: true }],
    ] as const) {
      FakeEventSource.reset();
      const { frames } = open(async () => status);
      src().fail(true);
      await vi.advanceTimersByTimeAsync(0);
      expect(frames.at(-1)).toEqual(expected);
      expect(FakeEventSource.instances).toHaveLength(1);
    }
    FakeEventSource.reset();
    open(async () => 429);
    src().fail(true);
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(2);
  });

  it("reports nothing after close, and never reconnects", async () => {
    const { frames, stream } = open();
    stream.close();
    src().sendFrame("snapshot", job());
    src().fail();
    await vi.advanceTimersByTimeAsync(10000);
    expect(frames).toEqual([]);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(src().closed).toBe(true);
  });

  it("does not open a stream where the browser has no EventSource", () => {
    const frames: JanitorStreamFrame[] = [];
    expect(() =>
      openJanitorRunStream({
        envId: "e1",
        runId: "r1",
        onFrame: (f) => frames.push(f),
        createSource: () => undefined,
      }),
    ).not.toThrow();
    expect(frames).toEqual([]);
  });
});
