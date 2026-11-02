// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { PullItem, PullJob } from "@/api/pulls";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  createPullStream,
  initialPullStream,
  type PullFrame,
  pullStreamUrl,
  reducePullStream,
} from "@/features/pulls/pull-stream";
import { waitUnderFakeClock } from "@/test-settle";

// The connection to a pull job's stream (plan 05-01, RES-09, D-26).

const item = (over: Partial<PullItem> = {}): PullItem => ({
  nodeId: "n1",
  name: "node-1",
  state: "waiting",
  code: "",
  message: "",
  current: 0,
  total: 0,
  unit: "",
  done: 0,
  count: 0,
  layers: [],
  digest: "",
  upToDate: false,
  ...over,
});

const job = (over: Partial<PullJob> = {}): PullJob => ({
  id: "j1",
  envId: "e1",
  ref: "docker.io/library/busybox:1.36.1",
  credential: null,
  state: "running",
  createdAt: "2026-10-11T10:00:00Z",
  finishedAt: "",
  items: [item(), item({ nodeId: "n2", name: "node-2" })],
  ...over,
});

describe("the reducer", () => {
  it("starts with nothing and is connecting", () => {
    expect(initialPullStream).toEqual({
      status: "connecting",
      finished: false,
      gone: false,
      accessLost: false,
    });
  });

  it("replaces the job with a snapshot, and is live", () => {
    const next = reducePullStream(initialPullStream, { type: "snapshot", job: job() });
    expect(next.job?.id).toBe("j1");
    expect(next.status).toBe("live");
    expect(next.finished).toBe(false);
  });

  it("takes a snapshot of a finished job as finished", () => {
    const next = reducePullStream(initialPullStream, {
      type: "snapshot",
      job: job({ state: "done" }),
    });
    expect(next.finished).toBe(true);
  });

  it("replaces one item by its node and keeps the others and their order", () => {
    const start = reducePullStream(initialPullStream, { type: "snapshot", job: job() });
    const next = reducePullStream(start, {
      type: "item",
      item: item({ nodeId: "n2", name: "node-2", state: "running", current: 5, total: 10 }),
    });
    expect(next.job?.items?.map((i) => [i.nodeId, i.state])).toEqual([
      ["n1", "waiting"],
      ["n2", "running"],
    ]);
  });

  it("adds an item for a node it did not know, and ignores an item before any snapshot", () => {
    expect(reducePullStream(initialPullStream, { type: "item", item: item() }).job).toBeUndefined();
    const start = reducePullStream(initialPullStream, { type: "snapshot", job: job() });
    const next = reducePullStream(start, { type: "item", item: item({ nodeId: "n3", name: "z" }) });
    expect(next.job?.items?.map((i) => i.nodeId)).toEqual(["n1", "n2", "n3"]);
  });

  it("marks the job finished by a done frame and closes", () => {
    const start = reducePullStream(initialPullStream, { type: "snapshot", job: job() });
    const next = reducePullStream(start, {
      type: "done",
      job: job({ state: "done", finishedAt: "2026-10-11T10:01:00Z" }),
    });
    expect(next.finished).toBe(true);
    expect(next.status).toBe("closed");
    expect(next.job?.state).toBe("done");
  });

  it("records a reconnect, a vanished job and lost access", () => {
    const live = reducePullStream(initialPullStream, { type: "snapshot", job: job() });
    expect(reducePullStream(live, { type: "status", status: "reconnecting" }).status).toBe(
      "reconnecting",
    );
    const gone = reducePullStream(live, { type: "status", status: "closed", gone: true });
    expect(gone).toMatchObject({ status: "closed", gone: true });
    const lost = reducePullStream(live, { type: "status", status: "closed", accessLost: true });
    expect(lost).toMatchObject({ status: "closed", accessLost: true });
  });

  it("does not move a finished job back to live", () => {
    const done = reducePullStream(initialPullStream, {
      type: "done",
      job: job({ state: "failed" }),
    });
    const next = reducePullStream(done, { type: "status", status: "reconnecting" });
    expect(next.status).toBe("closed");
  });
});

describe("the address", () => {
  it("names the job, and the last event seen when there is one", () => {
    const plain = new URL(pullStreamUrl("j 1"), "http://x");
    expect(plain.pathname).toBe("/api/pulls/j%201/stream");
    expect(plain.searchParams.has("lastEventId")).toBe(false);
    const resumed = new URL(pullStreamUrl("j1", "7"), "http://x");
    expect(resumed.searchParams.get("lastEventId")).toBe("7");
  });
});

describe("the connection", () => {
  beforeEach(() => {
    FakeEventSource.reset();
    vi.stubGlobal("EventSource", FakeEventSource);
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  function open(probe?: () => Promise<number>) {
    const frames: PullFrame[] = [];
    const stream = createPullStream({ jobId: "j1", onFrame: (f) => frames.push(f), probe });
    const source = FakeEventSource.instances[0] as FakeEventSource;
    return { stream, source, frames };
  }

  it("turns the named frames into frames, and keeps the last event id", () => {
    const { source, frames } = open();
    source.sendFrame("snapshot", job());
    source.sendFrame("item", item({ state: "running" }), "4");
    expect(frames.map((f) => f.type)).toEqual(["snapshot", "item"]);
    expect(frames[1]).toMatchObject({ type: "item", item: { state: "running" } });
  });

  it("closes the source after the done frame and does not reconnect", () => {
    vi.useFakeTimers();
    const { source, frames } = open();
    source.sendFrame("snapshot", job());
    source.sendFrame("done", job({ state: "done" }), "9");
    expect(frames.at(-1)?.type).toBe("done");
    expect(source.closed).toBe(true);
    source.fail();
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("reconnects after 1 then 2 seconds when it drops, sending the last event id", () => {
    vi.useFakeTimers();
    const { source, frames } = open();
    source.sendFrame("snapshot", job());
    source.sendFrame("item", item({ state: "running" }), "4");
    source.fail();
    expect(frames.at(-1)).toEqual({ type: "status", status: "reconnecting" });
    expect(source.closed).toBe(true);
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    const second = FakeEventSource.instances[1] as FakeEventSource;
    expect(new URL(second.url, "http://x").searchParams.get("lastEventId")).toBe("4");
    second.fail();
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);
  });

  it("turns a refused reopen into gone when the job read is 404", async () => {
    vi.useFakeTimers();
    const probe = vi.fn(async () => 404);
    const { source, frames } = open(probe);
    source.sendFrame("snapshot", job());
    source.fail();
    vi.advanceTimersByTime(1000);
    const second = FakeEventSource.instances[1] as FakeEventSource;
    second.fail(true);
    await waitUnderFakeClock(() => expect(probe).toHaveBeenCalledTimes(1));
    await waitUnderFakeClock(() =>
      expect(frames.at(-1)).toEqual({ type: "status", status: "closed", gone: true }),
    );
  });

  it("turns a refused reopen into lost access when the job read is 403", async () => {
    vi.useFakeTimers();
    const probe = vi.fn(async () => 403);
    const { source, frames } = open(probe);
    source.sendFrame("snapshot", job());
    source.fail(true);
    await waitUnderFakeClock(() =>
      expect(frames.at(-1)).toEqual({ type: "status", status: "closed", accessLost: true }),
    );
  });

  it("keeps trying when the probe finds a server that is only busy", async () => {
    vi.useFakeTimers();
    const probe = vi.fn(async () => 503);
    const { source } = open(probe);
    source.sendFrame("snapshot", job());
    source.fail(true);
    await waitUnderFakeClock(() => expect(probe).toHaveBeenCalledTimes(1));
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeEventSource.instances).toHaveLength(2);
  });

  it("reports nothing after it is closed", () => {
    const { stream, source, frames } = open();
    stream.close();
    source.sendFrame("snapshot", job());
    source.fail();
    expect(frames).toEqual([]);
    expect(source.closed).toBe(true);
  });
});
