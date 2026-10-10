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
});
