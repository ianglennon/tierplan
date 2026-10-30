// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import {
  LIST_STATS_DEBOUNCE_MS,
  listStatsUrl,
  MAX_LIST_STATS,
  nodeStatsUrl,
  readListRound,
  readNodeRound,
  serviceStatsUrl,
  useListStats,
  useNodeStats,
} from "@/features/stats/list-stats";

// The streams behind the Live stats switch and the node bars (plan 06-33, RES-11, D-31).
// No test here waits for a clock: timers are the test's, and the stream is a FakeEventSource.

beforeEach(() => {
  FakeEventSource.reset();
  vi.useFakeTimers();
  vi.stubGlobal("EventSource", FakeEventSource);
});

afterEach(() => {
  cleanup();
  setHidden(false);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function setHidden(hidden: boolean): void {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

const MIB = 1024 * 1024;

const serviceEntry = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  cpu: 142,
  mem: 1.5 * 1024 * MIB,
  netRx: 1.2 * MIB,
  netTx: 340 * 1024,
  counted: 2,
  silent: 0,
  limitBytes: null,
  ...extra,
});

const lastSource = (): FakeEventSource => {
  const s = FakeEventSource.instances[FakeEventSource.instances.length - 1];
  if (!s) throw new Error("no stream was opened");
  return s;
};

describe("the addresses", () => {
  it("name the environment, the kind and the ids of the rows", () => {
    const url = new URL(listStatsUrl("e 1", "services", ["a", "b"]), "http://x");
    expect(url.pathname).toBe("/api/env/e%201/services/stats/stream");
    expect(url.searchParams.get("ids")).toBe("a,b");
    const containers = new URL(listStatsUrl("e1", "containers", ["n1|c1"]), "http://x");
    expect(containers.pathname).toBe("/api/env/e1/containers/stats/stream");
    expect(containers.searchParams.get("ids")).toBe("n1|c1");
  });

  it("name the node stream and a service's own", () => {
    expect(nodeStatsUrl("e1")).toBe("/api/env/e1/nodes/stats/stream");
    expect(serviceStatsUrl("e1", "svc/web")).toBe("/api/env/e1/services/svc%2Fweb/stats/stream");
  });
});

describe("reading a round", () => {
  it("takes a service's total as the numbers it was sent, and — for a service with nothing counted", () => {
    const round = readListRound("services", {
      t: 1,
      services: [
        serviceEntry("a", { limitBytes: 2 * 1024 * MIB }),
        serviceEntry("b", { counted: 0, silent: 2, cpu: null, mem: 0, netRx: null, netTx: null }),
      ],
    });
    expect(round.get("a")).toEqual({
      cpu: 142,
      mem: 1.5 * 1024 * MIB,
      netRx: 1.2 * MIB,
      netTx: 340 * 1024,
      counted: 2,
      silent: 0,
      limitBytes: 2 * 1024 * MIB,
    });
    expect(round.get("b")?.mem).toBeNull();
    expect(round.get("b")?.silent).toBe(2);
  });

  it("takes a container as one counted task, or one silent when its node has no data", () => {
    const round = readListRound("containers", {
      t: 1,
      containers: [
        { id: "n1|c1", cpu: 5, mem: 100, netRx: 1, netTx: 2, noData: false, stopped: false },
        { id: "n9|c2", cpu: null, mem: null, netRx: null, netTx: null, noData: true },
        { id: "n1|c3", cpu: null, mem: null, netRx: null, netTx: null, stopped: true },
      ],
    });
    expect(round.get("n1|c1")).toMatchObject({ cpu: 5, mem: 100, counted: 1, silent: 0 });
    expect(round.get("n9|c2")).toMatchObject({ mem: null, counted: 0, silent: 1 });
    expect(round.get("n1|c3")).toMatchObject({ mem: null, counted: 0, silent: 0 });
  });

  it("believes only numbers and drops what is not an object of the list", () => {
    const round = readListRound("services", {
      services: [{ id: "a", cpu: "fast", mem: "lots", counted: "two" }, null, { cpu: 1 }],
    });
    expect([...round.keys()]).toEqual(["a"]);
    expect(round.get("a")).toMatchObject({ cpu: null, mem: null, counted: 0, silent: 0 });
    expect(readListRound("services", undefined).size).toBe(0);
  });

  it("reads a node's round, with its capacity and whether it reports", () => {
    const round = readNodeRound({
      t: 1,
      partial: true,
      nodes: [
        {
          id: "n1",
          cpu: 170,
          mem: 450 * MIB,
          cpus: 4,
          memoryBytes: 8 * 1024 * MIB,
          counted: 3,
          silent: 0,
        },
        {
          id: "n2",
          cpu: null,
          mem: 0,
          cpus: 2,
          memoryBytes: 0,
          counted: 0,
          silent: 1,
          notReporting: true,
        },
      ],
    });
    expect(round.partial).toBe(true);
    expect(round.nodes.get("n1")).toEqual({
      cpu: 170,
      mem: 450 * MIB,
      cpus: 4,
      memoryBytes: 8 * 1024 * MIB,
      notReporting: false,
    });
    expect(round.nodes.get("n2")).toMatchObject({ cpu: null, notReporting: true });
  });
});

describe("useListStats", () => {
  it("opens no stream for no rows, and one stream at once for the first rows", () => {
    const { rerender } = renderHook(({ ids }) => useListStats("e1", "services", ids), {
      initialProps: { ids: [] as string[] },
    });
    expect(FakeEventSource.instances).toHaveLength(0);
    rerender({ ids: ["a", "b"] });
    expect(FakeEventSource.instances).toHaveLength(1);
    const url = new URL(lastSource().url, "http://x");
    expect(url.pathname).toBe("/api/env/e1/services/stats/stream");
    expect(url.searchParams.get("ids")).toBe("a,b");
  });

  it("gives the latest round for each row, and nothing for a row that was not sampled", () => {
    const { result } = renderHook(() => useListStats("e1", "services", ["a", "b"]));
    expect(result.current.size).toBe(0);
    act(() => lastSource().sendFrame("ready", { services: 2 }));
    act(() => lastSource().sendFrame("sample", { t: 1, services: [serviceEntry("a")] }));
    expect(result.current.get("a")?.cpu).toBe(142);
    expect(result.current.has("b")).toBe(false);
    act(() =>
      lastSource().sendFrame("sample", { t: 2, services: [serviceEntry("a", { cpu: 7 })] }),
    );
    expect(result.current.get("a")?.cpu).toBe(7);
  });

  it("reopens for another set of rows only after they have settled for 500 ms", () => {
    const { rerender } = renderHook(({ ids }) => useListStats("e1", "services", ids), {
      initialProps: { ids: ["a"] },
    });
    expect(FakeEventSource.instances).toHaveLength(1);
    rerender({ ids: ["a", "b"] });
    act(() => {
      vi.advanceTimersByTime(LIST_STATS_DEBOUNCE_MS - 1);
    });
    rerender({ ids: ["a", "b", "c"] });
    act(() => {
      vi.advanceTimersByTime(LIST_STATS_DEBOUNCE_MS - 1);
    });
    expect(FakeEventSource.instances).toHaveLength(1);
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0]?.closed).toBe(true);
    expect(new URL(lastSource().url, "http://x").searchParams.get("ids")).toBe("a,b,c");
  });

  it("keeps what it knows of the rows that stay while the new stream connects, and drops the rest", () => {
    const { result, rerender } = renderHook(({ ids }) => useListStats("e1", "services", ids), {
      initialProps: { ids: ["a", "b"] },
    });
    act(() =>
      lastSource().sendFrame("sample", { t: 1, services: [serviceEntry("a"), serviceEntry("b")] }),
    );
    rerender({ ids: ["a", "c"] });
    act(() => {
      vi.advanceTimersByTime(LIST_STATS_DEBOUNCE_MS);
    });
    expect(result.current.has("a")).toBe(true);
    expect(result.current.has("b")).toBe(false);
  });

  it("closes the stream at once when the rows go, and forgets the numbers", () => {
    const { result, rerender } = renderHook(({ ids }) => useListStats("e1", "services", ids), {
      initialProps: { ids: ["a"] },
    });
    act(() => lastSource().sendFrame("sample", { t: 1, services: [serviceEntry("a")] }));
    expect(result.current.size).toBe(1);
    rerender({ ids: [] });
    expect(FakeEventSource.instances[0]?.closed).toBe(true);
    expect(result.current.size).toBe(0);
  });

  it("closes the stream when the list is left", () => {
    const { unmount } = renderHook(() => useListStats("e1", "containers", ["n1|c1"]));
    const s = lastSource();
    expect(s.closed).toBe(false);
    unmount();
    expect(s.closed).toBe(true);
  });

  it("asks for no more than 100 rows", () => {
    const many = Array.from({ length: MAX_LIST_STATS + 20 }, (_, i) => `id-${i}`);
    renderHook(() => useListStats("e1", "services", many));
    const ids = new URL(lastSource().url, "http://x").searchParams.get("ids")?.split(",");
    expect(ids).toHaveLength(MAX_LIST_STATS);
    expect(ids?.[0]).toBe("id-0");
  });

  it("lets the stream go after a minute in a hidden tab and keeps the numbers it had", () => {
    const { result } = renderHook(() => useListStats("e1", "services", ["a"]));
    act(() => lastSource().sendFrame("sample", { t: 1, services: [serviceEntry("a")] }));
    act(() => setHidden(true));
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(lastSource().closed).toBe(true);
    expect(result.current.get("a")?.cpu).toBe(142);
  });

  it("reopens with a stream of its own when the environment or kind changes", () => {
    const { rerender } = renderHook(
      ({ env, kind }) => useListStats(env, kind, kind === "services" ? ["a"] : ["n1|c1"]),
      { initialProps: { env: "e1", kind: "services" as "services" | "containers" } },
    );
    rerender({ env: "e1", kind: "containers" });
    expect(FakeEventSource.instances).toHaveLength(2);
    expect(FakeEventSource.instances[0]?.closed).toBe(true);
    expect(new URL(lastSource().url, "http://x").pathname).toBe(
      "/api/env/e1/containers/stats/stream",
    );
  });
});

describe("useNodeStats", () => {
  it("opens the node stream only while it is wanted, and closes it when it is not", () => {
    const { rerender } = renderHook(({ on }) => useNodeStats("e1", on), {
      initialProps: { on: false },
    });
    expect(FakeEventSource.instances).toHaveLength(0);
    rerender({ on: true });
    expect(lastSource().url).toBe("/api/env/e1/nodes/stats/stream");
    const s = lastSource();
    rerender({ on: false });
    expect(s.closed).toBe(true);
  });

  it("gives the latest round by node", () => {
    const { result } = renderHook(() => useNodeStats("e1", true));
    act(() =>
      lastSource().sendFrame("sample", {
        t: 1,
        partial: false,
        nodes: [{ id: "n1", cpu: 50, mem: 100, cpus: 4, memoryBytes: 1000, counted: 1, silent: 0 }],
      }),
    );
    expect(result.current.nodes.get("n1")).toMatchObject({ cpu: 50, mem: 100, cpus: 4 });
    expect(result.current.partial).toBe(false);
  });

  it("forgets the numbers when it is switched off", () => {
    const { result, rerender } = renderHook(({ on }) => useNodeStats("e1", on), {
      initialProps: { on: true },
    });
    act(() =>
      lastSource().sendFrame("sample", {
        t: 1,
        nodes: [{ id: "n1", cpu: 1, mem: 1, cpus: 1, memoryBytes: 1, counted: 1, silent: 0 }],
      }),
    );
    rerender({ on: false });
    expect(result.current.nodes.size).toBe(0);
  });
});
