// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackJob, StackJobItem, StackJobOutputLine } from "@/api/stacks";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  initialStackStream,
  openStackJobStream,
  reduceStackStream,
  type StackFrame,
  stackJobStreamUrl,
} from "@/features/stack-deploy/stack-job-stream";

// The connection to a deploy's stream (plan 07-17, STK-07, D-13).

const item = (over: Partial<StackJobItem> = {}): StackJobItem => ({
  id: "check",
  label: "Check the stack",
  parent: "",
  kind: "step",
  state: "waiting",
  code: "",
  message: "",
  detail: "",
  current: 0,
  total: 0,
  unit: "",
  result: {},
  ...over,
});

const job = (over: Partial<StackJob> = {}): StackJob => ({
  id: "j1",
  stackId: "s1",
  mode: "deploy",
  startedBy: "ada",
  state: "running",
  createdAt: "2026-10-19T12:00:00Z",
  finishedAt: "",
  items: [item(), item({ id: "services", label: "Deploy services" })],
  output: [],
  ...over,
});

const line = (seq: number, text = `line ${seq}`): StackJobOutputLine => ({
  seq,
  at: "2026-10-19T12:00:03Z",
  text,
});

describe("the reducer", () => {
  it("starts with nothing and is connecting", () => {
    expect(initialStackStream).toEqual({
      output: [],
      status: "connecting",
      finished: false,
      gone: false,
      accessLost: false,
    });
  });

  it("replaces the job with a snapshot, and is live", () => {
    const next = reduceStackStream(initialStackStream, { type: "snapshot", job: job() });
    expect(next.job?.id).toBe("j1");
    expect(next.status).toBe("live");
    expect(next.finished).toBe(false);
  });

  it("takes a snapshot of a finished job as finished", () => {
    const next = reduceStackStream(initialStackStream, {
      type: "snapshot",
      job: job({ state: "done" }),
    });
    expect(next.finished).toBe(true);
  });

  it("replaces one item by its id, adds one it has not seen, and keeps the order", () => {
    const start = reduceStackStream(initialStackStream, { type: "snapshot", job: job() });
    const updated = reduceStackStream(start, {
      type: "item",
      item: item({ id: "services", label: "Deploy services", state: "running" }),
    });
    expect(updated.job?.items?.map((i) => [i.id, i.state])).toEqual([
      ["check", "waiting"],
      ["services", "running"],
    ]);
    const added = reduceStackStream(updated, {
      type: "item",
      item: item({ id: "service:s1_web", label: "web", parent: "services", kind: "service" }),
    });
    expect(added.job?.items?.map((i) => i.id)).toEqual(["check", "services", "service:s1_web"]);
  });

  it("repeating an item changes nothing", () => {
    const start = reduceStackStream(initialStackStream, { type: "snapshot", job: job() });
    const frame: StackFrame = { type: "item", item: item({ state: "done" }) };
    const once = reduceStackStream(start, frame);
    expect(reduceStackStream(once, frame)).toEqual(once);
  });

  it("ignores an item before there is a job", () => {
    expect(reduceStackStream(initialStackStream, { type: "item", item: item() })).toBe(
      initialStackStream,
    );
  });

  it("ends the job and the stream on done, and never goes back to live", () => {
    const start = reduceStackStream(initialStackStream, { type: "snapshot", job: job() });
    const done = reduceStackStream(start, { type: "done", job: job({ state: "done" }) });
    expect(done).toMatchObject({ status: "closed", finished: true });
    expect(reduceStackStream(done, { type: "status", status: "reconnecting" })).toBe(done);
    expect(reduceStackStream(done, { type: "snapshot", job: job() }).status).toBe("closed");
  });

  it("takes a job read over HTTP only while the stream is not live", () => {
    const polled = reduceStackStream(initialStackStream, {
      type: "polled",
      job: job({ state: "running" }),
    });
    expect(polled.job?.state).toBe("running");
    expect(polled.status).toBe("connecting");
    const live = reduceStackStream(polled, { type: "snapshot", job: job({ createdAt: "x" }) });
    expect(reduceStackStream(live, { type: "polled", job: job({ createdAt: "older" }) })).toBe(
      live,
    );
    const dropped = reduceStackStream(live, { type: "status", status: "reconnecting" });
    expect(
      reduceStackStream(dropped, { type: "polled", job: job({ state: "done" }) }),
    ).toMatchObject({ finished: true });
  });

  it("marks a job as gone or unreadable and says it is closed", () => {
    expect(
      reduceStackStream(initialStackStream, { type: "status", status: "closed", gone: true }),
    ).toMatchObject({ gone: true, status: "closed" });
    expect(
      reduceStackStream(initialStackStream, { type: "status", status: "closed", accessLost: true }),
    ).toMatchObject({ accessLost: true });
  });
});

describe("the output log", () => {
  it("takes the log of a snapshot, and replaces its own with the snapshot's after a reconnect", () => {
    const first = reduceStackStream(initialStackStream, {
      type: "snapshot",
      job: job({ output: [line(2), line(3)] }),
    });
    expect(first.output.map((l) => l.seq)).toEqual([2, 3]);
    const again = reduceStackStream(first, {
      type: "snapshot",
      job: job({ output: [line(2), line(3), line(4)] }),
    });
    expect(again.output.map((l) => l.seq)).toEqual([2, 3, 4]);
  });

  it("appends a line, once: a line at or before the newest one is ignored", () => {
    let state = reduceStackStream(initialStackStream, {
      type: "snapshot",
      job: job({ output: [line(5)] }),
    });
    state = reduceStackStream(state, { type: "output", line: line(7) });
    expect(state.output.map((l) => l.seq)).toEqual([5, 7]);
    // A replay after a reconnect repeats lines the snapshot already holds.
    state = reduceStackStream(state, { type: "output", line: line(7) });
    state = reduceStackStream(state, { type: "output", line: line(6) });
    expect(state.output.map((l) => l.seq)).toEqual([5, 7]);
  });

  it("keeps the newest 2,000 lines", () => {
    let state = initialStackStream;
    for (let seq = 1; seq <= 2100; seq++) {
      state = reduceStackStream(state, { type: "output", line: line(seq) });
    }
    expect(state.output).toHaveLength(2000);
    expect(state.output[0]?.seq).toBe(101);
    expect(state.output.at(-1)?.seq).toBe(2100);
  });

  it("completes its log from the done frame without losing a line", () => {
    let state = reduceStackStream(initialStackStream, {
      type: "snapshot",
      job: job({ output: [line(1)] }),
    });
    state = reduceStackStream(state, { type: "output", line: line(2) });
    state = reduceStackStream(state, {
      type: "done",
      job: job({ state: "done", output: [line(1), line(2), line(3)] }),
    });
    expect(state.output.map((l) => l.seq)).toEqual([1, 2, 3]);
  });

  it("forgets the log on reset", () => {
    const state = reduceStackStream(initialStackStream, { type: "output", line: line(1) });
    expect(reduceStackStream(state, { type: "reset" }).output).toEqual([]);
  });
});

describe("the address", () => {
  it("names the job and puts the last event in the query", () => {
    expect(stackJobStreamUrl("e1", "s1", "j1")).toBe("/api/env/e1/stacks/s1/deploys/j1/stream");
    expect(stackJobStreamUrl("e1", "s1", "j1", "12")).toBe(
      "/api/env/e1/stacks/s1/deploys/j1/stream?lastEventId=12",
    );
    expect(stackJobStreamUrl("e 1", "s/1", "j1")).toBe(
      "/api/env/e%201/stacks/s%2F1/deploys/j1/stream",
    );
  });
});

describe("the connection", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  const open = (probe?: () => Promise<number>) => {
    const frames: StackFrame[] = [];
    const stream = openStackJobStream({
      envId: "e1",
      stackId: "s1",
      jobId: "j1",
      onFrame: (f) => frames.push(f),
      createSource: (url) => new FakeEventSource(url),
      probe,
    });
    return { frames, stream };
  };
  const src = (n = 0) => FakeEventSource.instances[n] as FakeEventSource;

  it("reports a snapshot, the items with their ids and done, then closes", () => {
    const { frames } = open();
    src().sendFrame("snapshot", job());
    src().sendFrame("item", item({ state: "running" }), "1");
    src().sendFrame("done", job({ state: "done" }), "2");
    expect(frames.map((f) => f.type)).toEqual(["snapshot", "item", "done"]);
    expect(src().closed).toBe(true);
  });

  it("reports an output line with its sequence and resumes after it", async () => {
    const { frames } = open();
    src().sendFrame("snapshot", job());
    src().sendFrame("output", line(9), "9");
    expect(frames.at(-1)).toEqual({ type: "output", line: line(9) });
    src().fail();
    await vi.advanceTimersByTimeAsync(1000);
    expect(src(1).url).toMatch(/\?lastEventId=9$/);
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
    open(async () => 500);
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
    const frames: StackFrame[] = [];
    expect(() =>
      openStackJobStream({
        envId: "e1",
        stackId: "s1",
        jobId: "j1",
        onFrame: (f) => frames.push(f),
        createSource: () => undefined,
      }),
    ).not.toThrow();
    expect(frames).toEqual([]);
  });
});
