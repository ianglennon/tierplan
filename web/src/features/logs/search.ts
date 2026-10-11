// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { stripAnsi } from "@/features/logs/ansi";

export interface SearchOptions {
  regex: boolean;
  caseSensitive: boolean;
}

// One line that matched: its index in the source and where the matches are in the
// line's text with colour codes removed, as [start, end) offsets.
export interface LineMatch {
  index: number;
  ranges: Array<[number, number]>;
}

export interface SearchResult {
  matches: LineMatch[];
  // Set when the pattern is not a valid regular expression; there are no matches then.
  error?: "invalid_pattern";
}

// What searchLines reads: an array, or the viewer's buffer.
export interface LineSource {
  readonly length: number;
  at(index: number): { m: string; marker?: unknown } | undefined;
}

// Search looks at no more than this much of a line, so one enormous line cannot hold
// the page up. The viewer still shows the whole line.
const MAX_SEARCHED_CHARS = 20_000;
// And finds no more than this many matches in one line.
const MAX_RANGES_PER_LINE = 100;

// A matcher finds the [start, end) ranges of a query in one text.
type Matcher = (text: string) => Array<[number, number]>;

// compile builds the matcher of a query, or says the pattern is not valid.
function compile(query: string, opts: SearchOptions): Matcher | "invalid_pattern" {
  if (opts.regex) {
    let re: RegExp;
    try {
      re = new RegExp(query, opts.caseSensitive ? "g" : "gi");
    } catch {
      return "invalid_pattern";
    }
    return (text) => {
      const out: Array<[number, number]> = [];
      re.lastIndex = 0;
      let m = re.exec(text);
      while (m !== null && out.length < MAX_RANGES_PER_LINE) {
        // An empty match would never advance.
        if (m[0].length === 0) re.lastIndex++;
        else out.push([m.index, m.index + m[0].length]);
        m = re.exec(text);
      }
      return out;
    };
  }
  const needle = opts.caseSensitive ? query : query.toLowerCase();
  return (text) => {
    const hay = opts.caseSensitive ? text : text.toLowerCase();
    const out: Array<[number, number]> = [];
    let from = 0;
    let at = hay.indexOf(needle, from);
    while (at >= 0 && out.length < MAX_RANGES_PER_LINE) {
      out.push([at, at + needle.length]);
      from = at + needle.length;
      at = hay.indexOf(needle, from);
    }
    return out;
  };
}

// searchLines finds the lines of a source that hold the query, in the text a person
// sees (colour codes removed). It only looks at what is loaded: nothing here asks the
// server. An empty query matches nothing. A regular expression that does not compile
// is reported as an error, never thrown. Marker rows (restarts and stops) are not log
// text and never match.
export function searchLines(lines: LineSource, query: string, opts: SearchOptions): SearchResult {
  if (query === "") return { matches: [] };
  const find = compile(query, opts);
  if (find === "invalid_pattern") return { matches: [], error: "invalid_pattern" };
  const matches: LineMatch[] = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines.at(i);
    if (!line || line.marker) continue;
    const text = stripAnsi(
      line.m.length > MAX_SEARCHED_CHARS ? line.m.slice(0, MAX_SEARCHED_CHARS) : line.m,
    );
    const ranges = find(text);
    if (ranges.length > 0) matches.push({ index: i, ranges });
  }
  return { matches };
}

// rangesIn finds the ranges of a query in one text, for highlighting a line that is
// drawn. It agrees with searchLines on what matches.
export function rangesIn(
  text: string,
  query: string,
  opts: SearchOptions,
): Array<[number, number]> {
  if (query === "") return [];
  const find = compile(query, opts);
  return find === "invalid_pattern" ? [] : find(text);
}
