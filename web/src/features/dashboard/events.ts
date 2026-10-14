// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type QueryKey, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  createElement,
  type ReactNode,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";

import { api } from "@/api/client";
import { listKey } from "@/api/resources";
import type { components } from "@/api/schema";
import { apiUrl } from "@/lib/base-path";

export type EventDTO = components["schemas"]["EventDTO"];
export type EventType = EventDTO["type"];

// The data of an `event: invalidate` frame: a change that makes lists stale but is
// not shown in the feed, such as a container's health turning unhealthy. It has no
// id and no attributes.
interface InvalidateFrame {
  type: EventType;
  actorId: string;
  node: string;
}

// What the feed shows (UI-SPEC "Events feed"): newest first, 50 at most.
export const FEED_CAP = 50;

// How the feed is connected, as the Live chip needs to know it (UI-SPEC "Live
// updates"). "loading" means there is no usable connection yet and nothing to
// show; "error" is a failed first load with nothing to show; "polling" means
// the stream was given up on and the list is read every 30 seconds.
export type FeedState = "loading" | "live" | "reconnecting" | "polling" | "error";

export interface EventsSnapshot {
  // Newest first, at most FEED_CAP.
  events: EventDTO[];
  state: FeedState;
  // True once there is something to show or a read has said there is nothing:
  // the load or a poll succeeded, or the stream delivered an event.
  ready: boolean;
}

// Pure helpers.

// A time as seconds plus the fraction padded to nine digits. RFC 3339 times
// from the server carry a fraction of any length (Go trims trailing zeros), so
// plain string comparison and Date.parse alone are both wrong for ordering.
function timeKey(time: string): [number, string] {
  const match = /^(.*?)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$/.exec(time);
  if (!match) return [Date.parse(time) / 1000, ""];
  const seconds = Date.parse(`${match[1]}${match[3]}`) / 1000;
  return [seconds, (match[2] ?? "").padEnd(9, "0")];
}

function compareTime(a: string, b: string): number {
  if (a === b) return 0;
  const [sa, fa] = timeKey(a);
  const [sb, fb] = timeKey(b);
  if (sa !== sb) return sa - sb;
  return fa < fb ? -1 : fa > fb ? 1 : 0;
}

// mergeEvents adds `incoming` to `existing` and returns the newest `cap`.
// `existing` is newest first. `incoming` is in the order the events arrived,
// oldest first, which is how the stream sends them. An id that is already
// known keeps the place it has. Events with the same time stay in arrival
// order, so the one that arrived later is shown above the one before it. The
// inputs are not changed.
export function mergeEvents(existing: EventDTO[], incoming: EventDTO[], cap: number): EventDTO[] {
  const seen = new Set<string>();
  const arrival: EventDTO[] = [];
  // Most recent arrival first: the incoming batch reversed, then what we had.
  for (const e of [...incoming].reverse().concat(existing)) {
    if (seen.has(e.id)) continue;
    seen.add(e.id);
    arrival.push(e);
  }
  // Array.prototype.sort is stable, so equal times keep arrival order.
  arrival.sort((a, b) => compareTime(b.time, a.time));
  return arrival.slice(0, cap);
}

// invalidationKeys are the queries an event of this type makes stale: the
// dashboard counts, the list the event is about and the lists it changes the
// columns of (D-57, RES-14). A container event also refreshes volumes and
// networks (Used by, attached) and images (what uses an image), a network event
// the containers list (its networks). Detail queries share their list's key
// prefix, so an open image page, with its Used by tab, refreshes with its list. A
// service event refreshes the services list and every filtered read of it, whose keys
// extend the list's. A node event refreshes the nodes list, the node pages (their keys extend the
// list's) and the services list, whose key the placement view extends, since a node going down
// or being drained moves tasks. The other swarm objects, and a row of Bayplan's own, have no list
// yet and refresh the summary only.
export function invalidationKeys(type: EventType, envId: string): QueryKey[] {
  const summary: QueryKey = ["env", envId, "summary"];
  switch (type) {
    case "container":
      return [
        summary,
        [...listKey(envId, "containers")],
        [...listKey(envId, "volumes")],
        [...listKey(envId, "networks")],
        [...listKey(envId, "images")],
      ];
    case "network":
      return [summary, [...listKey(envId, "networks")], [...listKey(envId, "containers")]];
    case "volume":
      return [summary, [...listKey(envId, "volumes")]];
    case "image":
      return [summary, [...listKey(envId, "images")]];
    case "service":
      return [summary, [...listKey(envId, "services")]];
    case "node":
      return [summary, [...listKey(envId, "nodes")], [...listKey(envId, "services")]];
    default:
      return [summary];
  }
}

// The connection.

// The part of EventSource the connection uses, so a test can drive it.
export interface EventSourceLike {
  readonly readyState: number;
  addEventListener(type: string, listener: (event: Event) => void): void;
  close(): void;
}

// What a read of the recent list asks for: the oldest time to look back to (an
// RFC 3339 time, which the server clamps to 24 hours) and how many events.
export interface RecentQuery {
  since?: string;
  limit: number;
}

export interface EventsConnectionOptions {
  envId: string;
  // GET /api/env/{envId}/events, newest first. A rejection is a failed read.
  fetchRecent: (query: RecentQuery) => Promise<EventDTO[]>;
  // How many events are kept, newest first. The dashboard's feed keeps FEED_CAP;
  // the Events page keeps 1000.
  cap?: number;
  // Reads of the list ask for events from this time on (RFC 3339). Without it the
  // server decides, which for the dashboard is the recent past.
  since?: string;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
  // Called with a query key that an event made stale; already throttled.
  invalidate: (key: QueryKey) => void;
  onChange: (snapshot: EventsSnapshot) => void;
  pollMs?: number;
  // This many errors inside errorWindowMs end the stream for polling.
  errorLimit?: number;
  errorWindowMs?: number;
  // At most one invalidation of the same key in this time (T-01-92).
  invalidateMs?: number;
}

export interface EventsConnection {
  start(): void;
  // Reads the list again after a failed first read.
  reload(): void;
  close(): void;
}

const EVENT_SOURCE_CLOSED = 2;
// How many event ids are remembered to tell a new event from a repeat.
const SEEN_LIMIT = 1024;

type Link = "connecting" | "live" | "reconnecting" | "polling";

// createEventsConnection is everything one environment's feed does outside
// React: the first read, the EventSource and its error handling, the polling
// fallback, deduplication, and the throttled invalidation of queries. The
// provider wraps it; tests drive it with a fake EventSource and fake timers.
export function createEventsConnection(options: EventsConnectionOptions): EventsConnection {
  const {
    envId,
    fetchRecent,
    cap = FEED_CAP,
    since,
    invalidate,
    onChange,
    pollMs = 30_000,
    errorLimit = 3,
    errorWindowMs = 30_000,
    invalidateMs = 1_000,
  } = options;
  const createSource = options.createSource ?? ((url: string) => new EventSource(url));

  let events: EventDTO[] = [];
  let link: Link = "connecting";
  let loaded = false;
  let loadFailed = false;
  let closed = false;
  let source: EventSourceLike | undefined;
  let pollTimer: ReturnType<typeof setInterval> | undefined;
  let errors: number[] = [];
  const seen = new Set<string>();
  // Room to remember every event that is kept, and some that have left.
  const seenLimit = Math.max(SEEN_LIMIT, cap * 2);
  const throttles = new Map<
    string,
    { last: number; timer?: ReturnType<typeof setTimeout>; key: QueryKey }
  >();

  function derive(): EventsSnapshot {
    const ready = loaded || events.length > 0;
    let state: FeedState;
    if (!ready) state = loadFailed ? "error" : "loading";
    else if (link === "polling") state = "polling";
    else if (link === "live") state = "live";
    else if (link === "reconnecting") state = "reconnecting";
    else state = "loading";
    return { events, state, ready };
  }

  function publish(): void {
    if (!closed) onChange(derive());
  }

  function remember(items: EventDTO[]): EventDTO[] {
    const fresh: EventDTO[] = [];
    for (const e of items) {
      if (seen.has(e.id)) continue;
      seen.add(e.id);
      fresh.push(e);
    }
    // A Set iterates in insertion order, so this forgets the oldest ids.
    for (const id of seen) {
      if (seen.size <= seenLimit) break;
      seen.delete(id);
    }
    return fresh;
  }

  // Runs the invalidation now unless the same key ran less than invalidateMs
  // ago, in which case one trailing run is scheduled for the end of that window.
  function throttled(key: QueryKey): void {
    const id = JSON.stringify(key);
    let entry = throttles.get(id);
    if (!entry) {
      entry = { last: Number.NEGATIVE_INFINITY, key };
      throttles.set(id, entry);
    }
    if (entry.timer !== undefined) return;
    const wait = entry.last + invalidateMs - Date.now();
    if (wait <= 0) {
      entry.last = Date.now();
      invalidate(key);
      return;
    }
    const target = entry;
    target.timer = setTimeout(() => {
      target.timer = undefined;
      target.last = Date.now();
      if (!closed) invalidate(target.key);
    }, wait);
  }

  // `incoming` is oldest first. Events already seen are merged without
  // invalidating anything: they are repeats, or history from before we looked.
  function take(incoming: EventDTO[], live: boolean): void {
    const fresh = remember(incoming);
    events = mergeEvents(events, incoming, cap);
    if (live) {
      for (const e of fresh) for (const key of invalidationKeys(e.type, envId)) throttled(key);
    }
    publish();
  }

  async function read(live: boolean): Promise<void> {
    try {
      const recent = await fetchRecent({ since, limit: cap });
      if (closed) return;
      loaded = true;
      loadFailed = false;
      // The endpoint answers newest first; the merge wants arrival order.
      take([...recent].reverse(), live);
    } catch {
      if (closed) return;
      loadFailed = true;
      publish();
    }
  }

  function startPolling(): void {
    if (pollTimer !== undefined) return;
    link = "polling";
    source?.close();
    source = undefined;
    // Polled events are new to us, so they refresh the lists like streamed ones.
    pollTimer = setInterval(() => void read(true), pollMs);
    publish();
  }

  function onError(): void {
    if (closed || link === "polling") return;
    const now = Date.now();
    errors = errors.filter((t) => now - t < errorWindowMs);
    errors.push(now);
    // The browser retries by itself while readyState is CONNECTING. CLOSED
    // means it gave up (a refused or non-stream response) and will not retry.
    if (errors.length >= errorLimit || source?.readyState === EVENT_SOURCE_CLOSED) {
      startPolling();
      return;
    }
    link = "reconnecting";
    publish();
  }

  function onDocker(event: Event): void {
    if (closed) return;
    let parsed: EventDTO;
    try {
      parsed = JSON.parse((event as MessageEvent<string>).data) as EventDTO;
    } catch {
      return;
    }
    if (typeof parsed.id !== "string" || typeof parsed.time !== "string") return;
    take([parsed], true);
  }

  // An invalidate frame refreshes the lists its type touches, throttled like a
  // docker event, and adds nothing to the feed.
  function onInvalidate(event: Event): void {
    if (closed) return;
    let parsed: InvalidateFrame;
    try {
      parsed = JSON.parse((event as MessageEvent<string>).data) as InvalidateFrame;
    } catch {
      return;
    }
    if (typeof parsed?.type !== "string") return;
    for (const key of invalidationKeys(parsed.type, envId)) throttled(key);
  }

  return {
    start() {
      void read(false);
      // Same origin, so the session cookie goes with it; a GET needs no CSRF token.
      try {
        source = createSource(apiUrl(`/api/env/${encodeURIComponent(envId)}/events/stream`));
      } catch {
        // No EventSource in this runtime: poll, and never claim to be live.
        source = undefined;
        startPolling();
        return;
      }
      source.addEventListener("open", () => {
        if (closed || link === "polling") return;
        const interrupted = link === "reconnecting";
        link = "live";
        publish();
        // The stream was down and is back: changes made in the gap produced no
        // event we saw (the server replays the feed, but not the health and other
        // invalidate-only changes), so refetch everything of the environment once.
        // A first connect has nothing to catch up on. The prefix covers every list
        // and detail query of the environment, and the throttle keeps a flapping
        // connection to one refetch a second (T-04-100).
        if (interrupted) throttled(["env", envId]);
      });
      source.addEventListener("error", onError);
      source.addEventListener("docker", onDocker);
      source.addEventListener("invalidate", onInvalidate);
      publish();
    },
    reload() {
      loadFailed = false;
      publish();
      void read(false);
    },
    close() {
      closed = true;
      source?.close();
      source = undefined;
      if (pollTimer !== undefined) clearInterval(pollTimer);
      pollTimer = undefined;
      for (const entry of throttles.values()) {
        if (entry.timer !== undefined) clearTimeout(entry.timer);
      }
      throttles.clear();
    },
  };
}

// The provider and the hook.

export async function fetchRecentEvents(
  envId: string,
  query: Partial<RecentQuery> = {},
): Promise<EventDTO[]> {
  const { data } = await api.GET("/api/env/{envId}/events", {
    params: { path: { envId }, query },
  });
  if (!data) throw new Error("GET events failed");
  return data.items ?? [];
}

interface EnvEvents extends EventsSnapshot {
  // Reads the list again after the first read failed.
  retry: () => void;
}

const INITIAL: EventsSnapshot = { events: [], state: "loading", ready: false };

const EventsContext = createContext<EnvEvents | undefined>(undefined);

interface EnvEventsProviderProps {
  envId: string;
  // False until the environment is known to answer, so no stream is opened
  // for one that is offline. The children stay mounted either way.
  enabled?: boolean;
  children: ReactNode;
}

// EnvEventsProvider holds the one events subscription of an open environment,
// so the dashboard's feed and every list view share it. It closes everything
// when the environment changes or the page goes away.
export function EnvEventsProvider({ envId, enabled = true, children }: EnvEventsProviderProps) {
  const queryClient = useQueryClient();
  const [snapshot, setSnapshot] = useState<EventsSnapshot>(INITIAL);
  const [connection, setConnection] = useState<EventsConnection | undefined>();

  useEffect(() => {
    setSnapshot(INITIAL);
    if (!enabled) return;
    const next = createEventsConnection({
      envId,
      fetchRecent: (query) => fetchRecentEvents(envId, query),
      invalidate: (key) => void queryClient.invalidateQueries({ queryKey: key }),
      onChange: setSnapshot,
    });
    setConnection(next);
    next.start();
    return () => {
      next.close();
      setConnection(undefined);
    };
  }, [envId, enabled, queryClient]);

  const value = useMemo<EnvEvents>(
    () => ({ ...snapshot, retry: () => connection?.reload() }),
    [snapshot, connection],
  );
  return createElement(EventsContext.Provider, { value }, children);
}

// useEnvEvents is the current environment's event feed: events newest first,
// the connection state and a retry for a failed first read.
export function useEnvEvents(): EnvEvents {
  const value = useContext(EventsContext);
  if (!value) throw new Error("useEnvEvents needs an EnvEventsProvider above it");
  return value;
}
