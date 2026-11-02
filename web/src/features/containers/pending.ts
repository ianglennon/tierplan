// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createContext, useContext, useEffect, useMemo, useRef, useState } from "react";

import type { ContainerRow } from "@/api/resources";
import { nodeRowKey } from "@/features/resources/swarm-lists";

// The actions a row can be waiting on. They are the labels of the transitional
// state: "Stopping…", "Removing…".
export type PendingVerb = "start" | "stop" | "restart" | "kill" | "pause" | "unpause" | "remove";

// How long a row keeps its transitional label when nothing confirms the change
// (UI-SPEC "Pending state").
export const PENDING_MAX_MS = 30_000;

// A row's key in the list, and what it looks like to the pending map. A stop
// changes the state and a restart changes the status text ("Up 3 seconds"), so
// both are part of it.
export const pendingKey = (r: Pick<ContainerRow, "node" | "id">) => nodeRowKey(r.node, r.id);
export const pendingFingerprint = (r: Pick<ContainerRow, "state" | "status">) =>
  `${r.state}|${r.status}`;

export interface PendingEntry {
  verb: PendingVerb;
  // When it was marked (ms).
  since: number;
  // What the row looked like then; the label ends when it looks different.
  fingerprint: string;
}

export interface PendingMark {
  key: string;
  name: string;
  fingerprint: string;
}

export interface PendingApi {
  // Marks rows as waiting on a verb.
  mark: (rows: readonly PendingMark[], verb: PendingVerb) => void;
  // Takes the label off, for the rows whose action failed: they return to their
  // real state at once.
  release: (keys: readonly string[]) => void;
}

const NOOP: PendingApi = { mark: () => {}, release: () => {} };

// The list provides the api; a dialog or menu used on its own (a detail page)
// finds none and its marks go nowhere.
export const PendingContext = createContext<PendingApi>(NOOP);

export function usePendingApi(): PendingApi {
  return useContext(PendingContext);
}

// What was last marked, for the one polite announcement ("Stopping web-1").
export interface PendingAnnouncement {
  verb: PendingVerb;
  names: string[];
  // Distinguishes two announcements with the same words.
  seq: number;
}

interface PendingRowsOptions<T> {
  getKey: (row: T) => string;
  // A string that differs when the action has taken effect. A stop changes the
  // state; a restart changes the status text ("Up 3 seconds").
  fingerprint: (row: T) => string;
}

export interface PendingRows {
  pending: ReadonlyMap<string, PendingEntry>;
  api: PendingApi;
  announcement: PendingAnnouncement | null;
}

// usePendingRows keeps the rows that are waiting on an action. A row leaves the
// map when the list shows it differently, when it is gone, when its action
// failed, or after 30 s (D-13).
export function usePendingRows<T>(
  rows: readonly T[],
  { getKey, fingerprint }: PendingRowsOptions<T>,
): PendingRows {
  const [pending, setPending] = useState<ReadonlyMap<string, PendingEntry>>(() => new Map());
  const [announcement, setAnnouncement] = useState<PendingAnnouncement | null>(null);
  const seq = useRef(0);

  const api = useMemo<PendingApi>(
    () => ({
      mark: (marked, verb) => {
        if (marked.length === 0) return;
        const since = Date.now();
        setPending((prev) => {
          const next = new Map(prev);
          for (const m of marked) next.set(m.key, { verb, since, fingerprint: m.fingerprint });
          return next;
        });
        seq.current += 1;
        setAnnouncement({ verb, names: marked.map((m) => m.name), seq: seq.current });
      },
      release: (keys) => {
        if (keys.length === 0) return;
        setPending((prev) => {
          if (!keys.some((k) => prev.has(k))) return prev;
          const next = new Map(prev);
          for (const k of keys) next.delete(k);
          return next;
        });
      },
    }),
    [],
  );

  // The rows as they are now, by key.
  const now = useMemo(() => {
    const out = new Map<string, string>();
    for (const r of rows) out.set(getKey(r), fingerprint(r));
    return out;
  }, [rows, getKey, fingerprint]);

  useEffect(() => {
    if (pending.size === 0) return;
    const t = Date.now();
    const gone: string[] = [];
    for (const [key, entry] of pending) {
      const current = now.get(key);
      if (
        current === undefined ||
        current !== entry.fingerprint ||
        t - entry.since >= PENDING_MAX_MS
      )
        gone.push(key);
    }
    if (gone.length > 0) {
      setPending((prev) => {
        const next = new Map(prev);
        for (const k of gone) next.delete(k);
        return next;
      });
      return;
    }
    // Nothing to drop yet: wake up when the oldest one runs out.
    const oldest = Math.min(...[...pending.values()].map((e) => e.since));
    const timer = setTimeout(
      () => setPending((prev) => new Map(prev)),
      oldest + PENDING_MAX_MS - t,
    );
    return () => clearTimeout(timer);
  }, [pending, now]);

  return { pending, api, announcement };
}
