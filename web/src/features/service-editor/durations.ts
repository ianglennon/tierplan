// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Go durations as the Form's Delay, Window and Monitor fields take them (5s, 30s, 2m, 1h30m),
// and the integer nanoseconds a swarm spec stores them as. Like the other field values, a
// function answers null for text that is not such a value, and the field then says so and
// writes nothing.

const MINUTE = 60e9;
const HOUR = 3600e9;

const UNIT_NS: Readonly<Record<string, number>> = {
  ns: 1,
  us: 1e3,
  µs: 1e3,
  μs: 1e3,
  ms: 1e6,
  s: 1e9,
  m: MINUTE,
  h: HOUR,
};

const PART = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/y;

// parseDuration reads a Go duration: one or more numbers, each with a unit of ns, us, µs, ms, s,
// m or h, such as 5s, 250ms or 1h30m. A number may have a fraction (1.5s). No space is allowed
// and a number must have its unit; the only exception is a bare 0, which Go accepts. A length
// can't be negative here, and one a browser can't hold exactly is refused. Returns the length in
// nanoseconds, undefined for text that is empty (nothing set), and null for text that is not a
// duration.
export function parseDuration(text: string): number | undefined | null {
  const t = text.trim();
  if (t === "") return undefined;
  if (t === "0") return 0;
  let total = 0;
  let at = 0;
  PART.lastIndex = 0;
  while (at < t.length) {
    PART.lastIndex = at;
    const m = PART.exec(t);
    if (m === null) return null;
    total += Number(m[1]) * (UNIT_NS[m[2] as string] as number);
    at = PART.lastIndex;
  }
  const ns = Math.round(total);
  return Number.isSafeInteger(ns) ? ns : null;
}

// seconds writes a count of nanoseconds under a minute as seconds with a fraction when it has
// one: 1500000000 is "1.5".
function seconds(ns: number): string {
  return (ns / 1e9).toFixed(9).replace(/\.?0+$/, "");
}

// formatDuration writes nanoseconds as the shortest Go duration that reads back as the same
// length: 90000000000 is "1m30s", 250000000 is "250ms". A value that is not a whole number of
// nanoseconds of 0 or more is "".
export function formatDuration(ns: unknown): string {
  if (typeof ns !== "number" || !Number.isSafeInteger(ns) || ns < 0) return "";
  if (ns === 0) return "0s";
  const hours = Math.floor(ns / HOUR);
  const minutes = Math.floor((ns % HOUR) / MINUTE);
  const rest = ns % MINUTE;
  if (hours === 0 && minutes === 0 && rest < 1e9) {
    if (rest % 1e6 === 0) return `${rest / 1e6}ms`;
    if (rest % 1e3 === 0) return `${rest / 1e3}µs`;
    return `${rest}ns`;
  }
  return `${hours > 0 ? `${hours}h` : ""}${minutes > 0 ? `${minutes}m` : ""}${rest > 0 ? `${seconds(rest)}s` : ""}`;
}
