// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { imageDetailKey, imageUsedByQuery } from "@/api/images";
import type { components } from "@/api/schema";
import { servicesQuery } from "@/api/services";
import {
  createEventsConnection,
  type EventsSnapshot,
  type FeedState,
  invalidationKeys,
  mergeEvents,
  type RecentQuery,
} from "@/features/dashboard/events";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { resetBasePathForTests } from "@/lib/base-path";

type EventDTO = components["schemas"]["EventDTO"];

function ev(id: string, time: string, over: Partial<EventDTO> = {}): EventDTO {
  return {
    id,
    node: "node-1",
    time,
    type: "container",
    action: "start",
    actorId: `actor-${id}`,
    name: `name-${id}`,
    attributes: {},
    ...over,
  };
}

// announceBase does what the server does when it renders index.html (plan
// 02-03): it puts the base path in a meta tag the SPA reads once.
function announceBase(base: string | null): void {
  for (const el of document.head.querySelectorAll('meta[name="bayplan-base"]')) el.remove();
  resetBasePathForTests();
  if (base === null) return;
  const meta = document.createElement("meta");
  meta.name = "bayplan-base";
  meta.content = base;
  document.head.append(meta);
}

const flush = () => vi.advanceTimersByTimeAsync(0);

function connect(
  fetchRecent: (query: RecentQuery) => Promise<EventDTO[]>,
  extra: { cap?: number; since?: string } = {},
) {
  FakeEventSource.instances = [];
  const snapshots: EventsSnapshot[] = [];
  const invalidated: Array<readonly unknown[]> = [];
  const connection = createEventsConnection({
    envId: "e1",
    fetchRecent,
    ...extra,
    createSource: (url) => new FakeEventSource(url),
    invalidate: (key) => invalidated.push(key),
    onChange: (s) => snapshots.push(s),
  });
  const states = (): FeedState[] =>
    snapshots.map((s) => s.state).filter((s, i, all) => i === 0 || s !== all[i - 1]);
  const last = () => snapshots[snapshots.length - 1] as EventsSnapshot;
  return {
    connection,
    snapshots,
    invalidated,
    states,
    last,
    source: () => FakeEventSource.instances[0],
  };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("mergeEvents", () => {
  it("returns newest first, drops repeated ids and keeps at most the cap", () => {
    const existing = [
      ev("c", "2026-10-02T10:00:03Z"),
      ev("b", "2026-10-02T10:00:02Z"),
      ev("a", "2026-10-02T10:00:01Z"),
    ];
    // Arrival order: oldest first. "b" is already known.
    const incoming = [ev("b", "2026-10-02T10:00:02Z"), ev("d", "2026-10-02T10:00:04Z")];
    expect(mergeEvents(existing, incoming, 50).map((e) => e.id)).toEqual(["d", "c", "b", "a"]);
    expect(mergeEvents(existing, incoming, 2).map((e) => e.id)).toEqual(["d", "c"]);

    const many = Array.from({ length: 80 }, (_, i) =>
      ev(
        `n${i}`,
        `2026-10-02T10:${String(Math.floor(i / 60)).padStart(2, "0")}:${String(i % 60).padStart(2, "0")}Z`,
      ),
    );
    const merged = mergeEvents([], many, 50);
    expect(merged).toHaveLength(50);
    expect(merged[0]?.id).toBe("n79");
    expect(merged[49]?.id).toBe("n30");
  });

  it("keeps arrival order for events with the same time, the later arrival on top", () => {
    const t = "2026-10-02T10:00:00Z";
    const first = mergeEvents([], [ev("x", t), ev("y", t)], 50);
    expect(first.map((e) => e.id)).toEqual(["y", "x"]);
    const next = mergeEvents(first, [ev("z", t)], 50);
    expect(next.map((e) => e.id)).toEqual(["z", "y", "x"]);
  });

  it("orders by sub-second time even when the fraction has a different length", () => {
    const merged = mergeEvents(
      [],
      [
        ev("early", "2026-10-02T10:00:00.5Z"),
        ev("late", "2026-10-02T10:00:00.123456789Z"),
        ev("later", "2026-10-02T10:00:01Z"),
      ],
      50,
    );
    // .5 is later than .123456789, and 01Z is later than both.
    expect(merged.map((e) => e.id)).toEqual(["later", "early", "late"]);
  });

  it("does not change its inputs", () => {
    const existing = [ev("a", "2026-10-02T10:00:01Z")];
    const incoming = [ev("b", "2026-10-02T10:00:02Z")];
    mergeEvents(existing, incoming, 50);
    expect(existing).toHaveLength(1);
    expect(incoming).toHaveLength(1);
  });
});

describe("invalidationKeys", () => {
  const summary = ["env", "e1", "summary"];
  const list = (resource: string) => ["env", "e1", resource];

  it("maps a container event to the summary, containers, volumes, networks and images", () => {
    expect(invalidationKeys("container", "e1")).toEqual([
      summary,
      list("containers"),
      list("volumes"),
      list("networks"),
      list("images"),
    ]);
  });

  it("refreshes an open image page with the images list, since its keys extend the list's", () => {
    const detail = imageDetailKey("e1", `sha256:${"a".repeat(64)}`, undefined);
    const usedBy = imageUsedByQuery("e1", `sha256:${"a".repeat(64)}`).queryKey;
    for (const type of ["container", "image"] as const) {
      const keys = invalidationKeys(type, "e1");
      const images = keys.find((k) => k[2] === "images") ?? [];
      expect(images.length).toBeGreaterThan(0);
      expect(detail.slice(0, images.length)).toEqual(images);
      expect(usedBy.slice(0, images.length)).toEqual(images);
    }
  });

  it("maps a network event to the summary, networks and containers", () => {
    expect(invalidationKeys("network", "e1")).toEqual([
      summary,
      list("networks"),
      list("containers"),
    ]);
  });

  it("maps a volume event to the summary and volumes, and an image event to the summary and images", () => {
    expect(invalidationKeys("volume", "e1")).toEqual([summary, list("volumes")]);
    expect(invalidationKeys("image", "e1")).toEqual([summary, list("images")]);
  });

  it("makes only the summary stale for swarm objects and for a row of Bayplan's own", () => {
    for (const type of ["node", "secret", "config", "bayplan"] as const) {
      expect(invalidationKeys(type, "e1")).toEqual([summary]);
    }
  });

  it("maps a service event to the summary and the services list", () => {
    expect(invalidationKeys("service", "e1")).toEqual([summary, list("services")]);
  });

  it("refreshes every filtered read of the services list, since their keys extend the list's", () => {
    const keys = invalidationKeys("service", "e1");
    const services = keys.find((k) => k[2] === "services") ?? [];
    const filtered = servicesQuery("e1", { q: "web", status: "degraded" }).queryKey;
    expect(filtered.slice(0, services.length)).toEqual(services);
  });
});

describe("createEventsConnection", () => {
  it("opens the stream under the base path the server announced", async () => {
    announceBase("/bayplan");
    try {
      const c = connect(() => Promise.resolve([]));
      c.connection.start();
      expect(c.source()?.url).toBe("/bayplan/api/env/e1/events/stream");
      c.connection.close();
    } finally {
      announceBase(null);
    }
  });

  it("loads, opens the stream on the guarded route and goes live", async () => {
    const c = connect(() => Promise.resolve([ev("a", "2026-10-02T10:00:01Z")]));
    expect(c.states()).toEqual([]);
    c.connection.start();
    expect(c.source()?.url).toBe("/api/env/e1/events/stream");
    expect(c.last().state).toBe("loading");
    expect(c.last().ready).toBe(false);
    await flush();
    expect(c.last().ready).toBe(true);
    expect(c.last().events.map((e) => e.id)).toEqual(["a"]);
    c.source()?.open();
    expect(c.last().state).toBe("live");
  });

  it("goes reconnecting on an error, back to live on reopen, and to polling after 3 errors in 30 seconds", async () => {
    const fetchRecent = vi.fn(() => Promise.resolve([] as EventDTO[]));
    const c = connect(fetchRecent);
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.fail();
    expect(c.last().state).toBe("reconnecting");
    source.open();
    expect(c.last().state).toBe("live");

    // The first error is still inside the window, so these are errors 2 and 3.
    await vi.advanceTimersByTimeAsync(5_000);
    source.fail();
    expect(c.last().state).toBe("reconnecting");
    await vi.advanceTimersByTimeAsync(5_000);
    source.fail();
    expect(c.last().state).toBe("polling");
    expect(source.closed).toBe(true);

    // Polling reads GET /events every 30 seconds and never shows Live again.
    expect(fetchRecent).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetchRecent).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetchRecent).toHaveBeenCalledTimes(3);
    const after = c.states().slice(c.states().indexOf("polling"));
    expect(after).toEqual(["polling"]);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it("does not fall back when the errors are more than 30 seconds apart", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.fail();
    await vi.advanceTimersByTimeAsync(20_000);
    source.fail();
    await vi.advanceTimersByTimeAsync(20_000);
    source.fail();
    expect(c.last().state).toBe("reconnecting");
    expect(source.closed).toBe(false);
  });

  it("polls instead when there is no EventSource to open", async () => {
    const fetchRecent = vi.fn(() => Promise.resolve([] as EventDTO[]));
    const snapshots: EventsSnapshot[] = [];
    createEventsConnection({
      envId: "e1",
      fetchRecent,
      createSource: () => {
        throw new ReferenceError("EventSource is not defined");
      },
      invalidate: () => {},
      onChange: (s) => snapshots.push(s),
    }).start();
    await flush();
    expect(snapshots.at(-1)?.state).toBe("polling");
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetchRecent).toHaveBeenCalledTimes(2);
    expect(snapshots.some((s) => s.state === "live")).toBe(false);
  });

  it("polls straight away when the browser gives up on the stream", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.fail(true);
    expect(c.last().state).toBe("polling");
    expect(source.closed).toBe(true);
  });

  it("shows an event that came from both the load and the stream once", async () => {
    const shared = ev("dup", "2026-10-02T10:00:05Z");
    const c = connect(() => Promise.resolve([shared]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.send(shared);
    source.send(ev("new", "2026-10-02T10:00:06Z"));
    expect(c.last().events.map((e) => e.id)).toEqual(["new", "dup"]);
    // The same frame again after a reconnect.
    source.fail();
    source.open();
    source.send(shared);
    expect(c.last().events.map((e) => e.id)).toEqual(["new", "dup"]);
  });

  it("invalidates the matching queries for a new event, at most once a second per key", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();

    source.send(ev("1", "2026-10-02T10:00:01Z", { type: "container" }));
    expect(c.invalidated).toEqual([
      ["env", "e1", "summary"],
      ["env", "e1", "containers"],
      ["env", "e1", "volumes"],
      ["env", "e1", "networks"],
      ["env", "e1", "images"],
    ]);

    // A burst inside the second collapses into one trailing refetch.
    source.send(ev("2", "2026-10-02T10:00:01Z", { type: "container" }));
    source.send(ev("3", "2026-10-02T10:00:01Z", { type: "container" }));
    source.send(ev("4", "2026-10-02T10:00:01Z", { type: "volume" }));
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(1);
    expect(c.invalidated.filter((k) => k[2] === "volumes")).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(1_000);
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(2);
    expect(c.invalidated.filter((k) => k[2] === "summary")).toHaveLength(2);

    // Nothing more once the window is quiet.
    await vi.advanceTimersByTimeAsync(5_000);
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(2);
  });

  it("refreshes the containers and the summary on an invalidate frame without touching the feed", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    const before = c.last();

    source.sendFrame("invalidate", { type: "container", actorId: "c1", node: "node-1" });
    expect(c.invalidated).toEqual([
      ["env", "e1", "summary"],
      ["env", "e1", "containers"],
      ["env", "e1", "volumes"],
      ["env", "e1", "networks"],
      ["env", "e1", "images"],
    ]);
    expect(c.last().events).toEqual(before.events);
    expect(c.last().events).toHaveLength(0);
  });

  it("throttles invalidate frames like docker events: a burst collapses into one trailing refetch", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();

    for (let i = 0; i < 5; i++) {
      source.sendFrame("invalidate", { type: "container", actorId: `c${i}`, node: "node-1" });
    }
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(c.invalidated.filter((k) => k[2] === "containers")).toHaveLength(2);
  });

  it("ignores an invalidate frame it cannot read", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.sendFrame("invalidate", null);
    source.sendFrame("invalidate", { actorId: "c1" });
    expect(c.invalidated).toEqual([]);
  });

  it("refetches everything of the environment once when the stream comes back after a drop", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    expect(c.invalidated).toEqual([]);

    source.fail();
    expect(c.states()).toContain("reconnecting");
    expect(c.invalidated).toEqual([]);
    source.open();
    expect(c.invalidated).toEqual([["env", "e1"]]);
    expect(c.last().state).toBe("live");

    // An open that was not a recovery does not refetch again.
    source.open();
    expect(c.invalidated).toEqual([["env", "e1"]]);
  });

  it("does not refetch on the first connect", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    (c.source() as FakeEventSource).open();
    await vi.advanceTimersByTimeAsync(5_000);
    expect(c.invalidated).toEqual([]);
  });

  it("refetches once per recovery, and a flapping connection is held to one refetch a second", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();

    for (let i = 0; i < 2; i++) {
      source.fail();
      source.open();
    }
    // The first recovery ran at once; the second waits for the end of the window.
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(2);
    // Long enough that the earlier errors no longer count towards giving up.
    await vi.advanceTimersByTimeAsync(31_000);
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(2);

    // A later, separate drop refetches again.
    source.fail();
    source.open();
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(3);
  });

  it("does not refetch when the stream gave up for polling", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.fail(true);
    expect(c.last().state).toBe("polling");
    // The browser's source is closed; a late open event changes nothing.
    source.open();
    expect(c.last().state).toBe("polling");
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(0);
  });

  it("invalidates once for an event delivered twice, as a resume after a reconnect replays it", async () => {
    const c = connect(() => Promise.resolve([]));
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    const same = ev("r1", "2026-10-02T10:00:01Z", { type: "network" });
    source.send(same);
    source.fail();
    source.open();
    source.send(same);
    await vi.advanceTimersByTimeAsync(5_000);
    // One set of keys for the network event, plus the one reconnect refetch.
    expect(c.invalidated.filter((k) => k[2] === "networks")).toHaveLength(1);
    expect(c.invalidated.filter((k) => k[2] === "summary")).toHaveLength(1);
    expect(c.invalidated.filter((k) => k.length === 2)).toHaveLength(1);
    expect(c.last().events.map((e) => e.id)).toEqual(["r1"]);
  });

  it("does not invalidate for an event it already has", async () => {
    const known = ev("k", "2026-10-02T10:00:01Z");
    const c = connect(() => Promise.resolve([known]));
    c.connection.start();
    await flush();
    (c.source() as FakeEventSource).open();
    (c.source() as FakeEventSource).send(known);
    expect(c.invalidated).toEqual([]);
  });

  it("reports a failed load as an error and recovers on reload", async () => {
    let fail = true;
    const fetchRecent = vi.fn(() =>
      fail ? Promise.reject(new Error("boom")) : Promise.resolve([ev("a", "2026-10-02T10:00:01Z")]),
    );
    const c = connect(fetchRecent);
    c.connection.start();
    await flush();
    expect(c.last().state).toBe("error");
    expect(c.last().ready).toBe(false);

    fail = false;
    c.connection.reload();
    expect(c.last().state).toBe("loading");
    await flush();
    expect(c.last().ready).toBe(true);
    expect(c.last().events).toHaveLength(1);
    expect(c.last().state).not.toBe("error");
  });

  it("keeps showing events when a later poll fails", async () => {
    let fail = false;
    const fetchRecent = vi.fn(() =>
      fail ? Promise.reject(new Error("boom")) : Promise.resolve([ev("a", "2026-10-02T10:00:01Z")]),
    );
    const c = connect(fetchRecent);
    c.connection.start();
    await flush();
    (c.source() as FakeEventSource).fail(true);
    fail = true;
    await vi.advanceTimersByTimeAsync(30_000);
    expect(c.last().state).toBe("polling");
    expect(c.last().events).toHaveLength(1);
  });

  it("closes the stream and stops polling and timers on close", async () => {
    const fetchRecent = vi.fn(() => Promise.resolve([] as EventDTO[]));
    const c = connect(fetchRecent);
    c.connection.start();
    await flush();
    const source = c.source() as FakeEventSource;
    source.open();
    source.send(ev("1", "2026-10-02T10:00:01Z"));
    source.send(ev("2", "2026-10-02T10:00:02Z"));
    c.connection.close();
    expect(source.closed).toBe(true);
    const before = {
      calls: fetchRecent.mock.calls.length,
      inv: c.invalidated.length,
      snaps: c.snapshots.length,
    };
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchRecent.mock.calls.length).toBe(before.calls);
    expect(c.invalidated.length).toBe(before.inv);
    expect(c.snapshots.length).toBe(before.snaps);
    expect(vi.getTimerCount()).toBe(0);
  });

  it("ignores a load that finishes after close", async () => {
    let resolve: (v: EventDTO[]) => void = () => {};
    const c = connect(() => new Promise<EventDTO[]>((r) => (resolve = r)));
    c.connection.start();
    c.connection.close();
    const n = c.snapshots.length;
    resolve([ev("a", "2026-10-02T10:00:01Z")]);
    await flush();
    expect(c.snapshots.length).toBe(n);
  });

  it("asks for the feed's own 50 events and no start time unless told otherwise", async () => {
    const fetchRecent = vi.fn((_: RecentQuery) => Promise.resolve([] as EventDTO[]));
    const c = connect(fetchRecent);
    c.connection.start();
    await flush();
    expect(fetchRecent).toHaveBeenCalledWith({ since: undefined, limit: 50 });
  });

  it("keeps the cap it is given and asks for that many events from the time it is given", async () => {
    const since = "2026-10-02T09:00:00.000Z";
    const history = Array.from({ length: 1200 }, (_, i) =>
      ev(`h${i}`, new Date(Date.parse("2026-10-02T10:00:00Z") + i * 1000).toISOString()),
    );
    const fetchRecent = vi.fn((_: RecentQuery) =>
      Promise.resolve([...history].reverse().slice(0, 1000)),
    );
    const c = connect(fetchRecent, { cap: 1000, since });
    c.connection.start();
    await flush();
    expect(fetchRecent).toHaveBeenCalledWith({ since, limit: 1000 });
    expect(c.last().events).toHaveLength(1000);
    expect(c.last().events[0]?.id).toBe("h1199");

    (c.source() as FakeEventSource).open();
    (c.source() as FakeEventSource).send(ev("live", "2026-10-02T11:00:00Z"));
    expect(c.last().events).toHaveLength(1000);
    expect(c.last().events[0]?.id).toBe("live");
    // The same window is read again when the stream gives way to polling.
    (c.source() as FakeEventSource).fail(true);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(fetchRecent).toHaveBeenLastCalledWith({ since, limit: 1000 });
    expect(c.last().events).toHaveLength(1000);
  });
});
