// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// How many lines the browser keeps (D-17). Past it the oldest are dropped and counted,
// and the viewer says so, because the page would otherwise grow with the log for as
// long as it stays open.
export const BUFFER_CAP = 50_000;

// One line of a container's log as the stream sends it. t is the engine's timestamp,
// s is "o" for stdout and "e" for stderr, and m is the text exactly as the program wrote
// it: it is only ever rendered as text, never as markup.
export interface LogLineData {
  t: string;
  s: "o" | "e";
  m: string;
}

// A line in the buffer, with the number that names it for as long as it is held.
// The numbers grow by one per line and are never reused, so a row keeps its key when
// older lines are dropped from the top.
export interface BufferedLine extends LogLineData {
  seq: number;
}

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

  push(lines: readonly LogLineData[]): void {
    if (lines.length === 0) return;
    for (const l of lines) this.items.push({ ...l, seq: this.nextSeq++ });
    const excess = this.length - this.cap;
    if (excess > 0) {
      this.start += excess;
      this.dropped += excess;
      if (this.start > this.cap) {
        this.items = this.items.slice(this.start);
        this.start = 0;
      }
    }
    this.bump();
  }

  clear(): void {
    this.items = [];
    this.start = 0;
    this.dropped = 0;
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
