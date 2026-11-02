// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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

    it("tells identical lines of two tasks at the oldest stamp apart, so the other task's line is kept", () => {
      const b = new LogBuffer();
      b.push([{ ...at(5, "ping"), task: "t1" }, at(6, "six")]);
      // The page ends with the same text written by t2 at that stamp and the held line of t1.
      const added = b.prepend([
        at(1, "one"),
        { ...at(5, "ping"), task: "t2" },
        { ...at(5, "ping"), task: "t1" },
      ]);
      expect(added).toBe(2);
      expect(
        Array.from({ length: b.length }, (_, i) => `${b.at(i).task ?? ""}:${b.at(i).m}`),
      ).toEqual([":one", "t2:ping", "t1:ping", ":six"]);
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

// The marks a service's log shows between its lines (plan 06-30, OBS-02, D-34, D-37).
describe("swarm markers", () => {
  const stamp = (n: number) => `2026-10-14T10:00:${String(n).padStart(2, "0")}.000000000Z`;
  const line = (n: number, task = "t1"): LogLineData => ({
    t: stamp(n),
    s: "o",
    m: `line ${n}`,
    task,
    slot: 1,
    node: "alpha",
  });
  const replaced = (old: string, next: string, extra = {}) => ({
    kind: "replaced" as const,
    slot: 1,
    old,
    oldState: "failed",
    oldNode: "alpha",
    new: next,
    node: "beta",
    ...extra,
  });
  const rows = (b: LogBuffer) =>
    Array.from({ length: b.length }, (_, i) => {
      const l = b.at(i);
      return l.marker ? `[${l.swarm?.kind}]` : l.m;
    });

  it("goes where the old task's last line was: after the last line that is not newer than it", () => {
    const b = new LogBuffer();
    b.push([line(1), line(2), line(5, "t2"), line(6, "t2")]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(2));
    expect(rows(b)).toEqual(["line 1", "line 2", "[replaced]", "line 5", "line 6"]);
    expect(b.at(2).marker).toBe("swarm");
    expect(b.at(2).t).toBe(stamp(2));
  });

  it("goes at the end when every line is older", () => {
    const b = new LogBuffer();
    b.push([line(1), line(2)]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(9));
    expect(rows(b)).toEqual(["line 1", "line 2", "[replaced]"]);
  });

  it("goes after the lines at its own time", () => {
    const b = new LogBuffer();
    b.push([line(2), line(2, "t3"), line(3)]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(2));
    expect(rows(b)).toEqual(["line 2", "line 2", "[replaced]", "line 3"]);
  });

  it("is not kept when it is older than every line held, but goes into an empty buffer", () => {
    const b = new LogBuffer();
    b.insertSwarmMarker(replaced("t0", "t1"), stamp(1));
    expect(b.length).toBe(1);
    const held = new LogBuffer();
    held.push([line(5), line(6)]);
    held.insertSwarmMarker(replaced("t1", "t2"), stamp(1));
    expect(rows(held)).toEqual(["line 5", "line 6"]);
  });

  it("counts as a line for the jump pill, and gets a number of its own", () => {
    const b = new LogBuffer();
    b.push([line(1), line(5)]);
    const before = b.pushed;
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(2));
    expect(b.pushed).toBe(before + 1);
    expect(new Set([0, 1, 2].map((i) => b.at(i).seq)).size).toBe(3);
  });

  it("tells the listeners once", () => {
    const b = new LogBuffer();
    b.push([line(1)]);
    const seen = vi.fn();
    b.subscribe(seen);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(2));
    expect(seen).toHaveBeenCalledTimes(1);
  });

  it("turns the marker of a slot that was waiting for a task into the replacement, in place", () => {
    const b = new LogBuffer();
    b.push([line(1), line(2)]);
    b.insertSwarmMarker(
      { kind: "stopped", slot: 1, old: "t1", oldState: "failed", oldNode: "alpha" },
      stamp(2),
    );
    b.push([line(8, "t2")]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(2));
    expect(rows(b)).toEqual(["line 1", "line 2", "[replaced]", "line 8"]);
    expect(b.at(2).swarm).toMatchObject({ kind: "replaced", new: "t2" });
  });

  it("keeps one marker when the same one is told again", () => {
    const b = new LogBuffer();
    b.push([line(1)]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(1));
    const seen = vi.fn();
    b.subscribe(seen);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(1));
    expect(rows(b)).toEqual(["line 1", "[replaced]"]);
    expect(seen).not.toHaveBeenCalled();
  });

  it("does not repeat a node notice that says what the last one for that node said", () => {
    const b = new LogBuffer();
    b.push([line(1)]);
    b.insertSwarmMarker({ kind: "down", node: "beta" }, stamp(3));
    b.insertSwarmMarker({ kind: "down", node: "beta" }, stamp(4));
    b.insertSwarmMarker({ kind: "down", node: "gamma" }, stamp(4));
    b.insertSwarmMarker({ kind: "back", node: "beta" }, stamp(5));
    b.insertSwarmMarker({ kind: "down", node: "beta" }, stamp(6));
    expect(b.length).toBe(5);
    expect(Array.from({ length: b.length }, (_, i) => b.at(i).swarm?.kind ?? "line")).toEqual([
      "line",
      "down",
      "down",
      "back",
      "down",
    ]);
  });

  it("is dropped from the top with the lines past the cap, and is not a search or prepend anchor", () => {
    const b = new LogBuffer(4);
    b.push([line(1), line(2), line(3)]);
    b.insertSwarmMarker(replaced("t1", "t2"), stamp(3));
    b.push([line(4), line(5)]);
    expect(b.length).toBe(4);
    expect(b.droppedCount).toBe(2);
    expect(rows(b)).toEqual(["line 3", "[replaced]", "line 4", "line 5"]);
  });
});
