// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  createStatsStream,
  HIDDEN_CLOSE_MS,
  STATS_RECONNECT_DELAYS_MS,
  type StatsStreamOptions,
  type StatsStreamState,
  statsStreamUrl,
} from "@/features/stats/stats-stream";
import type { StatsPoint } from "@/features/stats/use-stats-history";

// The connection to a container's stats stream (plan 06-32, RES-11, D-30). No test here
// waits for a clock: timers are the test's.

beforeEach(() => {
  FakeEventSource.reset();
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  setHidden(false);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

function open(extra: Partial<StatsStreamOptions> = {}) {
  const states: StatsStreamState[] = [];
  const samples: StatsPoint[] = [];
  const gaps: number[] = [];
  const stream = createStatsStream({
    envId: "e1",
    containerId: "web-id",
    onSample: (p) => samples.push(p),
    onStatus: (s) => states.push(s),
    onGap: (at) => gaps.push(at),
    now: () => 1234,
    ...extra,
  });
  const source = (n = 0): FakeEventSource => {
    const s = FakeEventSource.instances[n];
    if (!s) throw new Error(`no source ${n}`);
    return s;
  };
  return { stream, source, states, samples, gaps };
}

const SAMPLE = { t: 1000, cpu: 12.5, mem: 4096, netRx: 1, netTx: 2, blkRead: 3, blkWrite: 4 };

describe("the address", () => {
  it("names the environment and the container, and the node when given", () => {
    const url = new URL(
      statsStreamUrl({ envId: "e 1", containerId: "web/id", nodeId: "n1" }),
      "http://x",
    );
    expect(url.pathname).toBe("/api/env/e%201/containers/web%2Fid/stats/stream");
    expect(url.searchParams.get("nodeId")).toBe("n1");
    expect(new URL(statsStreamUrl({ envId: "e1", containerId: "c" }), "http://x").search).toBe("");
  });
});

describe("the state", () => {
  it("is connecting at once and live when the server says ready, with the container's limits", () => {
    const onReady = vi.fn();
    const { source, states } = open({ onReady });
    expect(states).toEqual([{ status: "connecting" }]);
    source().sendFrame("ready", { limitBytes: 2048, nanoCpus: null, running: true });
    expect(states.at(-1)).toEqual({ status: "live" });
    expect(onReady).toHaveBeenCalledWith({ limitBytes: 2048, nanoCpus: null, running: true });
  });

  it("hands on a sample and believes only numbers", () => {
    const { source, samples } = open();
    source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source().sendFrame("sample", SAMPLE);
    source().sendFrame("sample", { ...SAMPLE, t: 2000, cpu: null, netRx: null });
    source().sendFrame("sample", { t: "soon", mem: 1 });
    source().sendFrame("sample", { ...SAMPLE, t: 3000, cpu: "lots" });
    expect(samples).toEqual([
      SAMPLE,
      { ...SAMPLE, t: 2000, cpu: null, netRx: null },
      { ...SAMPLE, t: 3000, cpu: null },
    ]);
  });

  it("reports the node and the container as the server tells them", () => {
    const onNode = vi.fn();
    const onContainerState = vi.fn();
    const { source } = open({ onNode, onContainerState });
    source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source().sendFrame("node", { state: "not_reporting" });
    source().sendFrame("node", { state: "reporting" });
    source().sendFrame("node", { state: "elsewhere" });
    source().sendFrame("state", { state: "stopped" });
    source().sendFrame("state", { state: "running" });
    expect(onNode.mock.calls).toEqual([["not_reporting"], ["reporting"]]);
    expect(onContainerState.mock.calls).toEqual([["stopped"], ["running"]]);
  });

  it.each([
    ["access_lost", "access_lost"],
    ["not_running", "not_running"],
    ["anything else", "failed"],
  ])("is closed for good when the server closes it (%s)", (said, reason) => {
    const { source, states } = open();
    source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source().sendFrame("closed", { reason: said });
    expect(states.at(-1)).toEqual({ status: "closed", reason });
    expect(source().closed).toBe(true);
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});

describe("a drop", () => {
  it("says reconnecting and opens again after 1, 2, 4 and then 8 seconds, marking a gap when it is live again", () => {
    const { source, states, gaps } = open();
    expect(STATS_RECONNECT_DELAYS_MS).toEqual([1000, 2000, 4000, 8000]);
    source(0).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source(0).fail();
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
    vi.advanceTimersByTime(999);
    expect(FakeEventSource.instances).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(2);

    // It drops again before saying ready: the wait doubles.
    source(1).fail();
    vi.advanceTimersByTime(1999);
    expect(FakeEventSource.instances).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeEventSource.instances).toHaveLength(3);
    source(2).fail();
    vi.advanceTimersByTime(4000);
    expect(FakeEventSource.instances).toHaveLength(4);
    source(3).fail();
    vi.advanceTimersByTime(8000);
    expect(FakeEventSource.instances).toHaveLength(5);
    source(4).fail();
    vi.advanceTimersByTime(8000);
    expect(FakeEventSource.instances).toHaveLength(6);

    expect(gaps).toEqual([]);
    source(5).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    expect(states.at(-1)).toEqual({ status: "live" });
    expect(gaps).toEqual([1234]);

    // A good connection starts the schedule over.
    source(5).fail();
    vi.advanceTimersByTime(1000);
    expect(FakeEventSource.instances).toHaveLength(7);
  });

  it("is not a gap when the first connection is the one that is ready", () => {
    const { source, gaps } = open();
    source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    expect(gaps).toEqual([]);
  });

  it("is closed for good when the browser is refused and will not retry", () => {
    const { source, states } = open();
    source().fail(true);
    expect(states.at(-1)).toEqual({ status: "closed", reason: "failed" });
    vi.advanceTimersByTime(60_000);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("ignores what a stream it has let go of still says", () => {
    const { source, samples } = open();
    source(0).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source(0).fail();
    source(0).sendFrame("sample", SAMPLE);
    expect(samples).toEqual([]);
  });
});

describe("a hidden tab", () => {
  it("lets the stream go after 60 seconds, says paused, and opens it again when seen, with a gap", () => {
    const { source, states, gaps } = open();
    expect(HIDDEN_CLOSE_MS).toBe(60_000);
    source(0).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    setHidden(true);
    vi.advanceTimersByTime(59_999);
    expect(source(0).closed).toBe(false);
    expect(states.at(-1)).toEqual({ status: "live" });
    vi.advanceTimersByTime(1);
    expect(source(0).closed).toBe(true);
    expect(states.at(-1)).toEqual({ status: "paused" });

    // Nothing is opened while it is hidden, however long.
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(1);

    setHidden(false);
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(states.at(-1)).toEqual({ status: "reconnecting" });
    source(1).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    expect(states.at(-1)).toEqual({ status: "live" });
    expect(gaps).toEqual([1234]);
  });

  it("keeps the stream when the tab is seen again within the minute", () => {
    const { source, states } = open();
    source(0).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    setHidden(true);
    vi.advanceTimersByTime(30_000);
    setHidden(false);
    vi.advanceTimersByTime(120_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(states.at(-1)).toEqual({ status: "live" });
  });

  it("opens nothing more once it is paused, whatever it was waiting for", () => {
    const { source, states } = open();
    source(0).sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    source(0).fail();
    setHidden(true);
    vi.advanceTimersByTime(HIDDEN_CLOSE_MS);
    expect(states.at(-1)).toEqual({ status: "paused" });
    const opened = FakeEventSource.instances.length;
    vi.advanceTimersByTime(600_000);
    expect(FakeEventSource.instances).toHaveLength(opened);
  });
});

describe("close", () => {
  it("ends the stream, reports nothing after it and stops listening to the tab", () => {
    const { stream, source, states } = open();
    source().sendFrame("ready", { limitBytes: null, nanoCpus: null, running: true });
    const seen = states.length;
    stream.close();
    stream.close();
    expect(source().closed).toBe(true);
    source().fail();
    setHidden(true);
    vi.advanceTimersByTime(HIDDEN_CLOSE_MS * 2);
    expect(states).toHaveLength(seen);
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});
