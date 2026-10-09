// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

// How long a changed row stays muted (UI-SPEC "Row change highlight").
export const CHANGED_MS = 1500;
// How long a new row is flagged: the slide-in is 150 ms, so twice that is
// enough for it to have played.
export const ADDED_MS = 300;
// The fade-out of a row that is gone is 150 ms.
export const LEAVE_MS = 150;

export interface FrozenRow<T> {
  row: T;
  // Its fingerprint changed a moment ago.
  changed: boolean;
  // It arrived a moment ago.
  added: boolean;
  // It is gone from the server's answer but still shown: dimmed while the list
  // is frozen, fading out otherwise (leaving).
  removed: boolean;
  leaving: boolean;
}

export interface FrozenRowsOptions<T> {
  // True while something must not move: a row menu or a dialog is open.
  frozen: boolean;
  getKey: (row: T) => string;
  // A string that differs when what the row shows differs.
  fingerprint: (row: T) => string;
  // False until the answer for the current filters is in (and not a stand-in
  // from the previous filters). Rows are passed through unflagged until then.
  ready?: boolean;
  // Changes when the rows are a different list, not an update of this one: the
  // filters, the environment. Nothing is flagged across such a change.
  resetKey?: string;
}

export interface FrozenRows<T> {
  rows: FrozenRow<T>[];
  // New rows kept back while frozen.
  heldCount: number;
  // Gone rows still shown, dimmed, while frozen.
  removedCount: number;
  // Applies the held changes now, even while still frozen.
  release: () => void;
}

interface Entry<T> {
  key: string;
  row: T;
  fp: string;
  removed: boolean;
}

interface Snap<T> {
  source: readonly T[];
  frozen: boolean;
  ready: boolean;
  resetKey: string | undefined;
  entries: Entry<T>[];
  held: number;
  // Key to the time (ms) the flag runs out.
  changed: Map<string, number>;
  added: Map<string, number>;
  leaving: Map<string, number>;
}

function reducedMotion(): boolean {
  return (
    typeof window !== "undefined" &&
    window.matchMedia?.("(prefers-reduced-motion: reduce)").matches === true
  );
}

function fresh<T>(
  next: readonly T[],
  frozen: boolean,
  ready: boolean,
  resetKey: string | undefined,
  getKey: (row: T) => string,
  fingerprint: (row: T) => string,
): Snap<T> {
  return {
    source: next,
    frozen,
    ready,
    resetKey,
    entries: next.map((row) => ({ key: getKey(row), row, fp: fingerprint(row), removed: false })),
    held: 0,
    changed: new Map(),
    added: new Map(),
    leaving: new Map(),
  };
}

function live(map: Map<string, number>, now: number): Map<string, number> {
  const out = new Map<string, number>();
  for (const [key, until] of map) if (until > now) out.set(key, until);
  return out;
}

// advance computes what is shown after the server's answer, the freeze or the
// filters changed. It is pure but for reading the clock it is given.
function advance<T>(
  prev: Snap<T>,
  next: readonly T[],
  frozen: boolean,
  ready: boolean,
  resetKey: string | undefined,
  getKey: (row: T) => string,
  fingerprint: (row: T) => string,
  now: number,
): Snap<T> {
  // Not ready, just became ready, or a different list: start over, quietly.
  if (!ready || !prev.ready || prev.resetKey !== resetKey) {
    return fresh(next, frozen, ready, resetKey, getKey, fingerprint);
  }

  const changed = live(prev.changed, now);
  const added = live(prev.added, now);
  const leaving = live(prev.leaving, now);
  const before = new Map(prev.entries.map((e) => [e.key, e]));
  const nextByKey = new Map(next.map((row) => [getKey(row), row]));

  if (frozen) {
    // Same members, same order. A row that is still there is updated in place;
    // one that is gone stays, marked removed; one that is new waits.
    // A fade-out that began before the freeze finishes on its own.
    const entries = prev.entries.map((e): Entry<T> => {
      const row = nextByKey.get(e.key);
      if (row === undefined) return { ...e, removed: true };
      const fp = fingerprint(row);
      if (fp !== e.fp) changed.set(e.key, now + CHANGED_MS);
      return { key: e.key, row, fp, removed: false };
    });
    let held = 0;
    for (const key of nextByKey.keys()) if (!before.has(key)) held += 1;
    return { source: next, frozen, ready, resetKey, entries, held, changed, added, leaving };
  }

  // Not frozen: the server's answer, in the server's order.
  const entries: Entry<T>[] = [];
  for (const row of next) {
    const key = getKey(row);
    const fp = fingerprint(row);
    const old = before.get(key);
    if (!old) added.set(key, now + ADDED_MS);
    else if (old.fp !== fp) changed.set(key, now + CHANGED_MS);
    entries.push({ key, row, fp, removed: false });
  }
  if (!reducedMotion()) {
    // A row that is gone stays for its fade-out, right after the row that was
    // above it (or first, when nothing above it is left).
    prev.entries.forEach((old, index) => {
      if (nextByKey.has(old.key)) return;
      if (!leaving.has(old.key)) leaving.set(old.key, now + LEAVE_MS);
      let at = 0;
      for (let i = index - 1; i >= 0; i -= 1) {
        const above = prev.entries[i]?.key;
        const found = entries.findIndex((e) => e.key === above);
        if (found >= 0) {
          at = found + 1;
          break;
        }
      }
      entries.splice(at, 0, { ...old, removed: true });
    });
  }
  return { source: next, frozen, ready, resetKey, entries, held: 0, changed, added, leaving };
}

// prune takes the rows whose fade-out is over, and the flags that ran out.
function prune<T>(snap: Snap<T>, now: number): Snap<T> {
  const leaving = live(snap.leaving, now);
  const changed = live(snap.changed, now);
  const added = live(snap.added, now);
  const gone = (e: Entry<T>) => snap.leaving.has(e.key) && !leaving.has(e.key);
  const same =
    leaving.size === snap.leaving.size &&
    changed.size === snap.changed.size &&
    added.size === snap.added.size;
  if (same) return snap;
  return { ...snap, entries: snap.entries.filter((e) => !gone(e)), leaving, changed, added };
}

// useFrozenRows is the list behind D-05 and D-50: rows that change in place and
// never jump under the pointer.
//
// Not frozen, it gives the rows as the server sent them and marks what
// changed (for 1.5 s), what is new (it slides in) and what is gone (it fades
// out). Frozen, it keeps the members and the order it had: a changed row is
// still updated in place, a new row is held back and counted, and a gone row
// stays, marked removed. When the freeze ends, or release() is called,
// everything held applies at once.
//
// `rows` must keep its identity while the answer does not change: memoise it.
export function useFrozenRows<T>(rows: readonly T[], options: FrozenRowsOptions<T>): FrozenRows<T> {
  const { frozen, getKey, fingerprint, ready = true, resetKey } = options;
  const [snap, setSnap] = useState<Snap<T>>(() =>
    fresh(rows, false, ready, resetKey, getKey, fingerprint),
  );

  // The answer, the freeze or the filters changed since the last render: work
  // out what is shown now. Set during render, the way React allows for state
  // that is derived from props.
  let current = snap;
  if (
    snap.source !== rows ||
    snap.frozen !== frozen ||
    snap.ready !== ready ||
    snap.resetKey !== resetKey
  ) {
    current = advance(snap, rows, frozen, ready, resetKey, getKey, fingerprint, Date.now());
    setSnap(current);
  }

  // The flags and the fade-outs run out on their own; wake up for the next one.
  useEffect(() => {
    const times = [
      ...current.changed.values(),
      ...current.added.values(),
      ...current.leaving.values(),
    ];
    if (times.length === 0) return;
    const delay = Math.max(0, Math.min(...times) - Date.now()) + 5;
    const id = setTimeout(() => setSnap((s) => prune(s, Date.now())), delay);
    return () => clearTimeout(id);
  }, [current]);

  // The latest functions, so release keeps one identity.
  const fns = useRef({ getKey, fingerprint });
  fns.current = { getKey, fingerprint };
  const release = useCallback(() => {
    setSnap((s) => ({
      ...advance(
        s,
        s.source,
        false,
        s.ready,
        s.resetKey,
        fns.current.getKey,
        fns.current.fingerprint,
        Date.now(),
      ),
      // Still frozen if the caller says so: the next arrival is held again.
      frozen: s.frozen,
    }));
  }, []);

  return useMemo(() => {
    const shown = current.entries.map(
      (e): FrozenRow<T> => ({
        row: e.row,
        changed: current.changed.has(e.key),
        added: current.added.has(e.key),
        removed: e.removed,
        leaving: current.leaving.has(e.key),
      }),
    );
    return {
      rows: shown,
      heldCount: current.held,
      removedCount: shown.filter((r) => r.removed && !r.leaving).length,
      release,
    };
  }, [current, release]);
}
