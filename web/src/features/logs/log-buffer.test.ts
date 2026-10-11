// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it, vi } from "vitest";

import { BUFFER_CAP, LogBuffer, type LogLineData } from "@/features/logs/log-buffer";

// The browser's buffer of log lines (plan 04-19, D-17): bounded, counting what it drops.

function lines(n: number, from = 0): LogLineData[] {
  return Array.from({ length: n }, (_, i) => ({
    t: `2026-10-09T12:00:00.${String(from + i).padStart(9, "0")}Z`,
    s: "o" as const,
    m: `line ${from + i}`,
  }));
}

describe("LogBuffer", () => {
  it("keeps at most 50 000 lines by default", () => {
    expect(BUFFER_CAP).toBe(50_000);
    expect(new LogBuffer().cap).toBe(50_000);
  });

  it("holds lines oldest first and numbers each one for as long as it is held", () => {
    const b = new LogBuffer();
    b.push(lines(3));
    expect(b.length).toBe(3);
    expect(b.at(0).m).toBe("line 0");
    expect(b.at(2).m).toBe("line 2");
    expect([0, 1, 2].map((i) => b.at(i).seq)).toEqual([0, 1, 2]);
    expect(b.droppedCount).toBe(0);
  });

  it("drops the oldest lines past the cap and counts them", () => {
    const b = new LogBuffer(10);
    b.push(lines(8));
    b.push(lines(5, 8));
    expect(b.length).toBe(10);
    expect(b.droppedCount).toBe(3);
    expect(b.at(0).m).toBe("line 3");
    expect(b.at(9).m).toBe("line 12");
    // A line keeps its number when the ones before it go.
    expect(b.at(0).seq).toBe(3);
    expect(b.firstSeq).toBe(3);
  });

  it("stays at the cap through many pushes and counts every line it let go", () => {
    const b = new LogBuffer(100);
    for (let i = 0; i < 50; i++) b.push(lines(30, i * 30));
    expect(b.length).toBe(100);
    expect(b.droppedCount).toBe(1500 - 100);
    expect(b.at(0).m).toBe("line 1400");
    expect(b.at(99).m).toBe("line 1499");
  });

  it("keeps the newest lines when one push is larger than the cap", () => {
    const b = new LogBuffer(5);
    b.push(lines(12));
    expect(b.length).toBe(5);
    expect(b.droppedCount).toBe(7);
    expect(b.at(0).m).toBe("line 7");
  });

  it("tells its listeners once per push and changes its version, and says nothing for an empty push", () => {
    const b = new LogBuffer();
    const listener = vi.fn();
    const off = b.subscribe(listener);
    const v0 = b.getVersion();
    b.push([]);
    expect(listener).not.toHaveBeenCalled();
    expect(b.getVersion()).toBe(v0);
    b.push(lines(50));
    expect(listener).toHaveBeenCalledTimes(1);
    expect(b.getVersion()).not.toBe(v0);
    off();
    b.push(lines(1, 50));
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("forgets everything on clear, the dropped count included", () => {
    const b = new LogBuffer(3);
    b.push(lines(5));
    b.clear();
    expect(b.length).toBe(0);
    expect(b.droppedCount).toBe(0);
  });

  it("holds a marker row between lines, numbered like a line, and counts it towards the cap", () => {
    const b = new LogBuffer(4);
    b.push(lines(2));
    b.pushMarker("stopped", "2026-10-09T12:34:56.000Z");
    b.pushMarker("restarted", "2026-10-09T12:40:00.000Z");
    expect(b.length).toBe(4);
    expect(b.at(2).marker).toBe("stopped");
    expect(b.last?.marker).toBe("restarted");
    expect(b.at(2).seq).toBe(2);
    b.push(lines(1, 2));
    expect(b.droppedCount).toBe(1);
    expect(b.at(0).m).toBe("line 1");
  });

  describe("prepend", () => {
    const at = (n: number, m: string): LogLineData => ({
      t: `2026-10-09T12:00:${String(n).padStart(2, "0")}.000000000Z`,
      s: "o",
      m,
    });
    const texts = (b: LogBuffer) => Array.from({ length: b.length }, (_, i) => b.at(i).m);

    it("puts a page of earlier lines above the held ones, numbered below them", () => {
      const b = new LogBuffer();
      b.push([at(5, "five"), at(6, "six")]);
      const added = b.prepend([at(1, "one"), at(2, "two")]);
      expect(added).toBe(2);
      expect(texts(b)).toEqual(["one", "two", "five", "six"]);
      const seqs = Array.from({ length: 4 }, (_, i) => b.at(i).seq);
      expect(new Set(seqs).size).toBe(4);
      expect(seqs.slice(0, 2).every((n) => n < 0)).toBe(true);
      // A later line still gets a number past the end.
      b.push([at(7, "seven")]);
      expect(b.at(4).seq).toBeGreaterThan(b.at(3).seq);
      expect(b.pushed).toBe(3);
    });

    it("keeps pages in order when it is done twice", () => {
      const b = new LogBuffer();
      b.push([at(9, "nine")]);
      b.prepend([at(5, "five"), at(6, "six")]);
      b.prepend([at(1, "one"), at(2, "two")]);
      expect(texts(b)).toEqual(["one", "two", "five", "six", "nine"]);
    });

    it("drops the lines of the page that are already held at the oldest stamp, by text", () => {
      const b = new LogBuffer();
      b.push([at(5, "tick"), at(5, "tick"), at(6, "six")]);
      const added = b.prepend([
        at(1, "one"),
        at(5, "other"),
        at(5, "tick"),
        at(5, "tick"),
        at(5, "tick"),
      ]);
      // Two of the three ticks are the held ones; the third and "other" share the stamp
      // but are new.
      expect(added).toBe(3);
      expect(texts(b)).toEqual(["one", "other", "tick", "tick", "tick", "six"]);
    });

    it("adds nothing for a page that is only what is held", () => {
      const b = new LogBuffer();
      b.push([at(5, "tick")]);
      expect(b.prepend([at(5, "tick")])).toBe(0);
      expect(b.length).toBe(1);
    });

    it("counts what the cap pushes out of the top", () => {
      const b = new LogBuffer(4);
      b.push([at(8, "a"), at(9, "b")]);
      b.prepend([at(1, "x"), at(2, "y"), at(3, "z")]);
      expect(b.length).toBe(4);
      expect(b.droppedCount).toBe(1);
      expect(texts(b)).toEqual(["y", "z", "a", "b"]);
    });
  });
});
