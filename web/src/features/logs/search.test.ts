// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { describe, expect, it } from "vitest";

import { stripAnsi } from "@/features/logs/ansi";
import { rangesIn, searchLines } from "@/features/logs/search";

// Search over the lines already loaded (plan 04-20, D-17): plain text, regular
// expressions and case, on the text a person sees.

const lines = (...m: string[]) => m.map((text) => ({ m: text }));
const plain = { regex: false, caseSensitive: false };

describe("searchLines", () => {
  it("finds a word in any case by default, with where in the line it is", () => {
    const r = searchLines(lines("all fine", "an Error here", "ERROR error"), "err", plain);
    expect(r.error).toBeUndefined();
    expect(r.matches).toEqual([
      { index: 1, ranges: [[3, 6]] },
      {
        index: 2,
        ranges: [
          [0, 3],
          [6, 9],
        ],
      },
    ]);
  });

  it("matches case when asked to", () => {
    const r = searchLines(lines("Error", "error"), "Error", { regex: false, caseSensitive: true });
    expect(r.matches.map((m) => m.index)).toEqual([0]);
  });

  it("takes the query literally unless it is a regular expression", () => {
    const text = lines("a.c", "abc");
    expect(searchLines(text, "a.c", plain).matches.map((m) => m.index)).toEqual([0]);
    expect(
      searchLines(text, "a.c", { regex: true, caseSensitive: false }).matches.map((m) => m.index),
    ).toEqual([0, 1]);
  });

  it("finds a regular expression, with its ranges", () => {
    const r = searchLines(lines("id=42 id=7", "none"), "id=\\d+", {
      regex: true,
      caseSensitive: true,
    });
    expect(r.matches).toEqual([
      {
        index: 0,
        ranges: [
          [0, 5],
          [6, 10],
        ],
      },
    ]);
  });

  it("reports a pattern that is not valid instead of throwing, and matches nothing", () => {
    const r = searchLines(lines("(x"), "(", { regex: true, caseSensitive: false });
    expect(r.error).toBe("invalid_pattern");
    expect(r.matches).toEqual([]);
    // The same text is fine as plain text.
    expect(searchLines(lines("(x"), "(", plain).matches).toHaveLength(1);
  });

  it("does not loop on a pattern that can match nothing", () => {
    const r = searchLines(lines("abc"), "x*", { regex: true, caseSensitive: false });
    expect(r.error).toBeUndefined();
    expect(r.matches).toEqual([]);
  });

  it("matches nothing for an empty query", () => {
    expect(searchLines(lines("a"), "", plain)).toEqual({ matches: [] });
  });

  it("looks in the text without colour codes, and the ranges are in that text", () => {
    const r = searchLines(
      lines("\u001b[31mred\u001b[0m alert", "\u001b[1;32mok\u001b[0m"),
      "red alert",
      plain,
    );
    expect(r.matches).toEqual([{ index: 0, ranges: [[0, 9]] }]);
    expect(searchLines(lines("\u001b[31mred\u001b[0m"), "31m", plain).matches).toEqual([]);
  });

  it("skips marker rows and reads a buffer-like source", () => {
    const source = {
      length: 2,
      at: (i: number) => (i === 0 ? { m: "", marker: "stopped" } : { m: "stopped for good" }),
    };
    expect(searchLines(source, "stopped", plain).matches.map((m) => m.index)).toEqual([1]);
  });

  it("caps the matches it keeps in one line", () => {
    const r = searchLines(lines("a".repeat(500)), "a", plain);
    expect(r.matches[0]?.ranges).toHaveLength(100);
  });
});

describe("rangesIn", () => {
  it("agrees with the search on what matches in one text", () => {
    expect(rangesIn("Err err", "err", plain)).toEqual([
      [0, 3],
      [4, 7],
    ]);
    expect(rangesIn("Err err", "err", { regex: false, caseSensitive: true })).toEqual([[4, 7]]);
    expect(rangesIn("x", "(", { regex: true, caseSensitive: false })).toEqual([]);
  });
});

describe("stripAnsi", () => {
  it("removes colour codes and links, and leaves everything else alone", () => {
    expect(stripAnsi("\u001b[1;31mbold red\u001b[0m")).toBe("bold red");
    expect(stripAnsi("\u001b]8;;https://evil.test\u0007click\u001b]8;;\u0007")).toBe("click");
    expect(stripAnsi("plain <b>text</b>")).toBe("plain <b>text</b>");
  });
});
