// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { isSlotMark, type SwarmMark } from "@/features/logs/log-source";

// How many lines the browser keeps (D-17). Past it the oldest are dropped and counted,
// and the viewer says so, because the page would otherwise grow with the log for as
// long as it stays open.
export const BUFFER_CAP = 50_000;

// One line of a container's or a service's log as the stream sends it. t is the engine's timestamp,
// s is "o" for stdout and "e" for stderr, and m is the text exactly as the program wrote
// it: it is only ever rendered as text, never as markup.
export interface LogLineData {
  t: string;
  s: "o" | "e";
  m: string;
  // A line of a service's log also says which task wrote it: the task's ID, its slot (0
  // for a task of a global service) and the host name of its node (plan 06-29, D-35).
  task?: string;
  slot?: number;
  node?: string;
  // A stack's log merges the lines of several services, so each names its own (plan 07-29, D-22).
  service?: string;
}

// A line in the buffer, with the number that names it for as long as it is held.
// The numbers grow by one per line and are never reused, so a row keeps its key when
// older lines are dropped from the top.
export interface BufferedLine extends LogLineData {
  seq: number;
  // Set on a row that is not a line of the log but a mark the viewer put between lines:
  // the container started again, or stopped. t is when, m is empty.
  marker?: ContainerMarker;
  // What a "swarm" marker row says: a task of a service was replaced, a slot is waiting for
  // one, or a node cannot be reached (plan 06-30, D-34, D-37).
  swarm?: SwarmMark;
}

// What a marker row says happened to the container (D-20), or to the stream that
// follows it: it was cut and could not be resumed, so lines may be missing.
export type ContainerMarker = "restarted" | "stopped" | "interrupted" | "swarm";

type Listener = () => void;

// LogBuffer holds the lines of one viewer, newest last, up to its cap. It is a plain
// object a React component reads through useSyncExternalStore (subscribe and
// getVersion), so a burst of lines is one render, not one per line.
export class LogBuffer {
  private items: BufferedLine[] = [];
  // The first live entry of items; the entries before it were dropped and are only
  // removed from the array once they outnumber the cap, so a drop is not a copy.
  private start = 0;
  private nextSeq = 0;
  // Lines added at the top take the numbers below zero, counting down, so they sort
  // before everything held and never collide with a number in use.
  private prevSeq = -1;
  private pushedLines = 0;
  private dropped = 0;
  private version = 0;
  private listeners = new Set<Listener>();

  constructor(readonly cap: number = BUFFER_CAP) {}

  get length(): number {
    return this.items.length - this.start;
  }

  // How many lines were dropped from the top since the buffer was made or cleared.
  get droppedCount(): number {
    return this.dropped;
  }

  // The line at index i, 0 being the oldest held.
  at(i: number): BufferedLine {
    return this.items[this.start + i] as BufferedLine;
  }

  // The sequence number of the oldest line held, or the number the next line will get
  // when there is none.
  get firstSeq(): number {
    return this.length > 0 ? this.at(0).seq : this.nextSeq;
  }

  // How many lines have ever been pushed at the end since the buffer was made. It
  // only grows, so the difference between two readings is how many lines arrived
  // between them, whatever was dropped meanwhile.
  get pushed(): number {
    return this.pushedLines;
  }

  push(lines: readonly LogLineData[]): void {
    if (lines.length === 0) return;
    for (const l of lines) this.items.push({ ...l, seq: this.nextSeq++ });
    this.pushedLines += lines.length;
    this.trim();
    this.bump();
  }

  // Drops the oldest rows past the cap and counts them.
  private trim(): void {
    const excess = this.length - this.cap;
    if (excess > 0) {
      this.start += excess;
      this.dropped += excess;
      if (this.start > this.cap) {
        this.items = this.items.slice(this.start);
        this.start = 0;
      }
    }
  }

  // The newest row held, if any.
  get last(): BufferedLine | undefined {
    return this.length > 0 ? this.at(this.length - 1) : undefined;
  }

  // Adds a marker row (the container restarted or stopped) after the lines held. at is
  // when it happened, as an ISO time. The lines are kept (D-20).
  pushMarker(kind: ContainerMarker, at: string): void {
    this.items.push({ t: at, s: "o", m: "", seq: this.nextSeq++, marker: kind });
    this.trim();
    this.bump();
  }

  // Puts a swarm marker row where the old task's last line was: after the last line that is
  // not newer than at. A marker about a task that already has one is updated in place (a
  // slot that was waiting for a task now has one), and a node notice that repeats the one
  // before it is dropped (a reconnect tells again). A marker older than every line held has
  // nothing to sit beside and is not kept. It counts as a line for the jump pill.
  insertSwarmMarker(mark: SwarmMark, at: string): void {
    for (let i = this.items.length - 1; i >= this.start; i--) {
      const row = this.items[i] as BufferedLine;
      const held = row.swarm;
      if (!held) continue;
      if (isSlotMark(mark)) {
        if (!isSlotMark(held) || held.old !== mark.old) continue;
        if (held.kind === mark.kind && held.new === mark.new) return;
        this.items[i] = { ...row, swarm: mark };
        this.bump();
        return;
      }
      if (isSlotMark(held) || held.node !== mark.node) continue;
      // The last thing said about this node: the same again says nothing new.
      if (held.kind === mark.kind) return;
      break;
    }
    const when = at || new Date().toISOString();
    let i = this.items.length;
    while (i > this.start && (this.items[i - 1] as BufferedLine).t > when) i--;
    if (i === this.start && this.length > 0) return;
    this.items.splice(i, 0, {
      t: when,
      s: "o",
      m: "",
      seq: this.nextSeq++,
      marker: "swarm",
      swarm: mark,
    });
    this.pushedLines++;
    this.trim();
    this.bump();
  }

  // Adds a page of earlier lines at the top (Load earlier lines). The page may repeat
  // lines already held at the oldest line's stamp: a request for "everything before
  // this moment" is made one nanosecond late so no line sharing that stamp is missed
  // (Pitfall 12), and the lines already held are told apart by stamp and text, each
  // text counted, so two identical lines written at the same nanosecond are both kept.
  // Returns how many lines were added.
  prepend(page: readonly LogLineData[]): number {
    let first: BufferedLine | undefined;
    for (let i = 0; i < this.length; i++) {
      const l = this.at(i);
      if (!l.marker) {
        first = l;
        break;
      }
    }
    const held = new Map<string, number>();
    if (first) {
      for (let i = 0; i < this.length; i++) {
        const l = this.at(i);
        if (l.marker) continue;
        if (l.t !== first.t) break;
        const k = `${l.s}\u0000${l.task ?? ""}\u0000${l.m}`;
        held.set(k, (held.get(k) ?? 0) + 1);
      }
    }
    const fresh: LogLineData[] = [];
    for (const l of page) {
      if (first && l.t > first.t) continue;
      if (first && l.t === first.t) {
        const k = `${l.s}\u0000${l.task ?? ""}\u0000${l.m}`;
        const n = held.get(k) ?? 0;
        if (n > 0) {
          held.set(k, n - 1);
          continue;
        }
      }
      fresh.push(l);
    }
    if (fresh.length === 0) return 0;
    const added: BufferedLine[] = new Array(fresh.length);
    for (let i = fresh.length - 1; i >= 0; i--) {
      added[i] = { ...(fresh[i] as LogLineData), seq: this.prevSeq-- };
    }
    this.items = added.concat(this.items.slice(this.start));
    this.start = 0;
    this.trim();
    this.bump();
    return fresh.length;
  }

  clear(): void {
    this.items = [];
    this.start = 0;
    this.dropped = 0;
    this.prevSeq = -1;
    this.bump();
  }

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getVersion = (): number => this.version;

  private bump(): void {
    this.version++;
    for (const l of this.listeners) l();
  }
}

// The time of day of a line's stamp, HH:mm:ss.SSS, in the browser's time zone or in
// UTC. The engine's stamp has nine digits; a millisecond is all that is shown.
export function stampTime(t: string, utc: boolean): string {
  const d = new Date(t.replace(/(\.\d{3})\d*Z$/, "$1Z"));
  if (Number.isNaN(d.getTime())) return "";
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return utc
    ? `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}.${p(d.getUTCMilliseconds(), 3)}`
    : `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

// The calendar day of a line's stamp as YYYY-MM-DD in the chosen zone: two lines are
// on the same day when these are equal.
export function stampDay(t: string, utc: boolean): string {
  // A day never changes inside a minute in any time zone, so the first 16 characters
  // (to the minute) are the cache key and a log of thousands of lines parses a date
  // once per minute, not once per line.
  const key = `${utc ? "u" : "l"}${t.slice(0, 16)}`;
  const hit = dayCache.get(key);
  if (hit !== undefined) return hit;
  const d = new Date(t.replace(/(\.\d{3})\d*Z$/, "$1Z"));
  let day = "";
  if (!Number.isNaN(d.getTime())) {
    const p = (n: number) => String(n).padStart(2, "0");
    day = utc
      ? `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())}`
      : `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  }
  if (dayCache.size > 5000) dayCache.clear();
  dayCache.set(key, day);
  return day;
}

const dayCache = new Map<string, string>();

// clockTime is the hour and minute of an ISO time in the browser's time zone, as
// HH:MM, for the words on a marker row.
export function clockTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}
