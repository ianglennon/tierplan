// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { WARNINGS_KEY } from "@/api/warnings";
import type { EventSourceLike } from "@/features/dashboard/events";
import { apiUrl } from "@/lib/base-path";

// The data of every frame of GET /api/environments/stream but the first. The
// stream is not in the OpenAPI contract (it cannot describe one), so these are
// written by hand from EnvStreamEventBody in internal/api/envstream.go:
//
//   env.status          environment: the environment as the list returns it
//   agent.connected     agentId, node, type (manager, worker or standalone), version
//   agent.disconnected  agentId, node, reason (closed, timeout, replaced or shutdown)
//   agent.refused       code, mode (enrol or hello), node, ip
//   proxy.warning       kind (untrusted_forwarded, url_mismatch or websocket_failing);
//                       envId is empty, and only a caller with settings.read gets it
//
// Every one carries envId and at. The event's id is the SSE id field, which a
// browser exposes as MessageEvent.lastEventId, so it is not in the body.
export interface EnvStreamFrame {
  envId: string;
  at: string;
  environment?: EnvironmentItem;
  agentId?: string;
  node?: string;
  type?: string;
  version?: string;
  reason?: string;
  code?: string;
  mode?: string;
  ip?: string;
  kind?: string;
}

export type EnvEventName =
  | "env.status"
  | "agent.connected"
  | "agent.disconnected"
  | "agent.refused"
  | "proxy.warning";

export interface EnvHealthEvent {
  name: EnvEventName;
  // The broker's id: a number as text, strictly increasing within one server run.
  id: string;
  data: EnvStreamFrame;
}

// How the stream is connected, as the Live chip reads it. "connecting" is
// before the first answer and shows nothing; "polling" means the stream was
// given up on and the list is read every 30 seconds, and shows nothing either
// (D-36, UI-SPEC Live).
export type HealthLive = "connecting" | "live" | "reconnecting" | "polling";

const EVENTS: readonly EnvEventName[] = [
  "env.status",
  "agent.connected",
  "agent.disconnected",
  "agent.refused",
  "proxy.warning",
];

const EVENT_SOURCE_CLOSED = 2;
// How many event keys are remembered to tell a new event from a repeat.
const SEEN_LIMIT = 1024;
const POLL_MS = 30_000;
// This many errors inside ERROR_WINDOW_MS end the stream for polling.
const ERROR_LIMIT = 3;
const ERROR_WINDOW_MS = 30_000;
// While polling, the stream is tried again after this long, so a server
// restart does not leave the tab on polling until it is reloaded.
const RECOVER_MS = 120_000;
// At most one list refresh from the stream's own events in this time.
const REFRESH_MS = 1_000;

type Handler = (event: EnvHealthEvent) => void;

interface HealthStream {
  link: HealthLive;
  subscribe: (envId: string, handler: Handler) => () => void;
}

const StreamContext = createContext<HealthStream | undefined>(undefined);

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

interface ProviderProps {
  children?: ReactNode;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
}

// EnvHealthStreamProvider holds the app's one health stream (D-36): a single
// EventSource for every environment the caller may read, however many places
// show health. It keeps the ["environments"] cache current in place, hands
// agent events to whoever subscribed for that environment, and when the stream
// cannot be had it reads the list every 30 seconds instead. It belongs inside
// QueryClientProvider, once, in the signed-in layout.
export function EnvHealthStreamProvider({ children, createSource }: ProviderProps) {
  const queryClient = useQueryClient();
  const [link, setLink] = useState<HealthLive>("connecting");
  const listeners = useRef(new Map<string, Set<Handler>>());

  const subscribe = useCallback((envId: string, handler: Handler) => {
    const map = listeners.current;
    const set = map.get(envId) ?? new Set<Handler>();
    set.add(handler);
    map.set(envId, set);
    return () => {
      set.delete(handler);
      if (set.size === 0 && map.get(envId) === set) map.delete(envId);
    };
  }, []);

  // The 30-second read while the stream is given up on.
  const polling = link === "polling";
  useQuery({
    ...environmentsQuery,
    enabled: polling,
    refetchInterval: polling ? POLL_MS : false,
  });

  useEffect(() => {
    const make = createSource ?? ((url: string) => new EventSource(url));
    let closed = false;
    // "poll" from the moment the stream is given up on until an open stream
    // answers, which includes a recovery attempt that is still connecting.
    let mode: "stream" | "poll" = "stream";
    let source: EventSourceLike | undefined;
    let errors: number[] = [];
    let recoverTimer: ReturnType<typeof setTimeout> | undefined;
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    let lastRefresh = Number.NEGATIVE_INFINITY;
    const seen = new Set<string>();

    // Reads the whole list. fetchQuery, not invalidate, so it runs even when
    // nothing is looking at the list at the moment.
    function refreshList(): void {
      const wait = lastRefresh + REFRESH_MS - Date.now();
      if (wait > 0) {
        if (refreshTimer === undefined) {
          refreshTimer = setTimeout(() => {
            refreshTimer = undefined;
            if (!closed) refreshList();
          }, wait);
        }
        return;
      }
      lastRefresh = Date.now();
      void queryClient.fetchQuery({ ...environmentsQuery, staleTime: 0 }).catch(() => {});
    }

    // A server restarts its ids from 1, so the same id alone does not make a
    // repeat: the event's own time has to match as well.
    function isRepeat(id: string, at: string): boolean {
      if (id === "") return false;
      const key = `${id}@${at}`;
      if (seen.has(key)) return true;
      seen.add(key);
      // A Set iterates in insertion order, so this forgets the oldest.
      for (const old of seen) {
        if (seen.size <= SEEN_LIMIT) break;
        seen.delete(old);
      }
      return false;
    }

    function applySnapshot(data: Record<string, unknown>): void {
      if (!Array.isArray(data.items)) return;
      // After a connect or a reconnect the snapshot is the truth.
      queryClient.setQueryData(environmentsQuery.queryKey, {
        items: data.items as EnvironmentItem[],
      });
    }

    function applyStatus(environment: EnvironmentItem | undefined): void {
      if (!environment || typeof environment.id !== "string") return;
      const current = queryClient.getQueryData(environmentsQuery.queryKey);
      // Nothing to change until the list has loaded; that read carries it.
      if (!current) return;
      const items = current.items ?? [];
      if (!items.some((e) => e.id === environment.id)) {
        // An environment this tab has not heard of: read the list.
        refreshList();
        return;
      }
      queryClient.setQueryData(environmentsQuery.queryKey, {
        ...current,
        items: items.map((e) => (e.id === environment.id ? environment : e)),
      });
    }

    function onFrame(name: "snapshot" | EnvEventName) {
      return (event: Event) => {
        if (closed) return;
        const message = event as MessageEvent<string>;
        let data: unknown;
        try {
          data = JSON.parse(message.data);
        } catch {
          return;
        }
        if (!isRecord(data)) return;
        if (name === "snapshot") {
          applySnapshot(data);
          return;
        }
        if (typeof data.envId !== "string" || typeof data.at !== "string") return;
        if (isRepeat(message.lastEventId, data.at)) return;
        const frame = data as unknown as EnvStreamFrame;
        if (name === "proxy.warning") {
          // A reverse proxy problem has appeared: the bell reads the warnings
          // again. It belongs to no environment, so nobody subscribed for it.
          void queryClient.invalidateQueries({ queryKey: WARNINGS_KEY });
          return;
        }
        if (name === "env.status") applyStatus(frame.environment);
        // The counts and last-seen times are not part of env.status, so an
        // agent coming or going reads the list again.
        if (name === "agent.connected" || name === "agent.disconnected") refreshList();
        const heard = listeners.current.get(frame.envId);
        if (!heard) return;
        const out: EnvHealthEvent = { name, id: message.lastEventId, data: frame };
        for (const handler of [...heard]) handler(out);
      };
    }

    function giveUp(): void {
      mode = "poll";
      source?.close();
      source = undefined;
      setLink("polling");
      clearTimeout(recoverTimer);
      recoverTimer = setTimeout(() => {
        if (closed) return;
        errors = [];
        open();
      }, RECOVER_MS);
    }

    function onError(): void {
      if (closed) return;
      const now = Date.now();
      errors = errors.filter((t) => now - t < ERROR_WINDOW_MS);
      errors.push(now);
      // The browser retries by itself while readyState is CONNECTING. CLOSED
      // means it gave up (a refused or non-stream response).
      if (errors.length >= ERROR_LIMIT || source?.readyState === EVENT_SOURCE_CLOSED) {
        giveUp();
        return;
      }
      if (mode === "stream") setLink("reconnecting");
    }

    function open(): void {
      try {
        // Same origin, so the session cookie goes with it.
        source = make(apiUrl("/api/environments/stream"));
      } catch {
        // No EventSource in this runtime: poll, and never claim to be live.
        source = undefined;
        giveUp();
        return;
      }
      source.addEventListener("open", () => {
        if (closed) return;
        mode = "stream";
        clearTimeout(recoverTimer);
        setLink("live");
      });
      source.addEventListener("error", onError);
      source.addEventListener("snapshot", onFrame("snapshot"));
      for (const name of EVENTS) source.addEventListener(name, onFrame(name));
    }

    open();
    return () => {
      closed = true;
      source?.close();
      source = undefined;
      clearTimeout(recoverTimer);
      clearTimeout(refreshTimer);
    };
  }, [queryClient, createSource]);

  const value = useMemo<HealthStream>(() => ({ link, subscribe }), [link, subscribe]);
  return <StreamContext.Provider value={value}>{children}</StreamContext.Provider>;
}

function useStream(): HealthStream {
  const value = useContext(StreamContext);
  if (!value) throw new Error("the health stream hooks need an EnvHealthStreamProvider above them");
  return value;
}

// useHealthLive is how the stream is connected, for the Live chip.
export function useHealthLive(): HealthLive {
  return useStream().link;
}

// useEnvHealthEvents calls handler for every agent and status event of one
// environment, in the order they arrive, until the component unmounts. The
// waiting panel uses it for refused attempts. The handler may change on every
// render; the subscription does not.
export function useEnvHealthEvents(envId: string | undefined, handler: Handler): void {
  const { subscribe } = useStream();
  const latest = useRef(handler);
  useEffect(() => {
    latest.current = handler;
  });
  useEffect(() => {
    if (!envId) return;
    return subscribe(envId, (event) => latest.current(event));
  }, [subscribe, envId]);
}
