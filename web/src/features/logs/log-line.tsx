// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { CSSProperties, ReactNode } from "react";

import { type AnsiSpan, ansiToSpans, type Theme } from "@/features/logs/ansi";
import { type BufferedLine, stampTime } from "@/features/logs/log-buffer";
import { ReplacementMarker } from "@/features/logs/replacement-marker";
import { cn } from "@/lib/utils";

// The height of a row. The virtualiser sizes every row by it, so the line height of
// the text is the same number (UI-SPEC "Typography", log lines).
export const LINE_HEIGHT_PX = 20;

// A run of text in one style, and whether a search match covers it.
export interface StyledSpan extends AnsiSpan {
  mark?: "match" | "current";
}

// markSpans cuts the spans at the edges of the ranges, which are [start, end) offsets
// into the text of all the spans together, and flags the pieces inside one. With no
// ranges the spans come back as they were.
export function markSpans(
  spans: readonly AnsiSpan[],
  ranges: ReadonlyArray<readonly [number, number]> | undefined,
  current: boolean,
): StyledSpan[] {
  if (!ranges || ranges.length === 0) return spans.slice();
  const out: StyledSpan[] = [];
  let at = 0;
  for (const span of spans) {
    const end = at + span.text.length;
    let from = at;
    for (const [rs, re] of ranges) {
      const a = Math.max(rs, from);
      const b = Math.min(re, end);
      if (b <= a) continue;
      if (a > from) out.push({ ...span, text: span.text.slice(from - at, a - at) });
      out.push({
        ...span,
        text: span.text.slice(a - at, b - at),
        mark: current ? "current" : "match",
      });
      from = b;
    }
    if (from < end) out.push({ ...span, text: span.text.slice(from - at) });
    at = end;
  }
  return out;
}

// The CSS value of one of our colours: a palette variable, the page's own colour, or a
// #rrggbb that ansi.ts has already checked.
function cssColour(c: string): string {
  const idx = /^ansi-(\d+)$/.exec(c);
  if (idx) return `var(--ansi-${idx[1]})`;
  if (c === "foreground" || c === "background") return `var(--${c})`;
  return c;
}

// Colour reaches the page only as custom properties, so what a program printed is never
// a style string of its own: the value is one of ours (T-04-68).
function spanStyle(s: StyledSpan): CSSProperties | undefined {
  const style: Record<string, string> = {};
  if (s.fg && s.mark !== "current") {
    style["--log-fg"] = cssColour(s.fg);
    style.color = "var(--log-fg)";
  }
  if (s.bg && !s.mark) {
    style["--log-bg"] = cssColour(s.bg);
    style.backgroundColor = "var(--log-bg)";
  }
  return Object.keys(style).length > 0 ? (style as CSSProperties) : undefined;
}

export interface LogLineHighlight {
  // Where the search matched, in the text without colour codes.
  ranges: ReadonlyArray<readonly [number, number]>;
  // True for the match the counter is on.
  current: boolean;
}

export interface LogLineRowProps {
  line: BufferedLine;
  stderrLabel: string;
  // What a marker row says, already translated; used only for marker rows.
  markerLabel: (line: BufferedLine) => string;
  // Show the time column, in UTC or in the browser's zone. label names the column
  // for a screen reader and says UTC when it is.
  time?: { utc: boolean; label: string };
  wrap?: boolean;
  highlight?: LogLineHighlight;
  // Which palette the colour in the line is checked against.
  theme?: Theme;
  // The task that wrote the line, for a service's log: drawn between the stream gutter and
  // the time. It cannot be selected, so copied lines carry none (plan 06-29, D-32).
  prefix?: ReactNode;
}

// One line of a log: a stream gutter that is always as wide as the word "stderr" so
// lines never shift, an optional time column, then the text. A stderr line carries the
// visible "stderr" marker in the gutter, a 2px bar and a faint tint; the marker, not
// the colour, is what says it is stderr. The marker is real text for a screen reader
// and cannot be selected, so copying lines does not copy it.
//
// The text is made of React text nodes: it is never read as markup, never linkified
// and never shortened (T-04-67, T-04-68). A search match is a <mark> around a piece of
// that text; the current one is also inverted, so it is never colour alone.
export function LogLineRow({
  line,
  stderrLabel,
  markerLabel,
  time,
  wrap = false,
  highlight,
  theme = "light",
  prefix,
}: LogLineRowProps) {
  if (line.marker === "swarm") {
    // A task of the service was replaced, or a node dropped out: a separator of its own.
    return <ReplacementMarker text={markerLabel(line)} kind={line.swarm?.kind} />;
  }
  if (line.marker) {
    // The container started again or stopped: not a line of the log, so it is set
    // apart by hairlines and drawn in the muted colour.
    return (
      <div
        data-marker={line.marker}
        className="flex h-5 w-full min-w-full items-center gap-3 px-3 text-xs text-muted-foreground"
      >
        <span aria-hidden="true" className="h-px flex-1 bg-border" />
        <span className="shrink-0">{markerLabel(line)}</span>
        <span aria-hidden="true" className="h-px flex-1 bg-border" />
      </div>
    );
  }
  const stderr = line.s === "e";
  const spans = markSpans(
    ansiToSpans(line.m, theme),
    highlight?.ranges,
    highlight?.current === true,
  );
  return (
    <div
      data-stream={stderr ? "stderr" : "stdout"}
      className={cn(
        "flex min-w-full border-s-2 font-mono text-xs leading-5",
        wrap
          ? "min-h-5 w-full items-start whitespace-pre-wrap [overflow-wrap:anywhere]"
          : "h-5 w-max items-center whitespace-pre",
        stderr ? "border-status-offline bg-status-offline/8" : "border-transparent",
      )}
    >
      <span className="w-[6ch] shrink-0 select-none text-muted-foreground">
        {stderr ? stderrLabel : ""}
      </span>
      {prefix ? (
        <span className="flex shrink-0 select-none items-center ps-2">{prefix}</span>
      ) : null}
      {time ? (
        <time
          dateTime={line.t}
          title={time.label}
          className="ps-2 w-[calc(12ch+0.5rem)] shrink-0 text-muted-foreground tabular-nums"
        >
          {stampTime(line.t, time.utc)}
        </time>
      ) : null}
      <span className="ps-2">
        {spans.map((s, i) => {
          const classes = cn(
            s.bold && "font-semibold",
            s.dim && !s.fg && "text-muted-foreground",
            s.underline && "underline",
          );
          // The spans of one line are drawn once and in order, so the position is
          // the identity.
          return s.mark ? (
            <mark
              // biome-ignore lint/suspicious/noArrayIndexKey: positions are the identity
              key={i}
              data-match={s.mark}
              style={spanStyle(s)}
              className={cn(
                "rounded-xs text-inherit",
                classes,
                s.mark === "current" ? "bg-foreground text-background" : "bg-foreground/18",
              )}
            >
              {s.text}
            </mark>
          ) : (
            // biome-ignore lint/suspicious/noArrayIndexKey: positions are the identity
            <span key={i} style={spanStyle(s)} className={classes || undefined}>
              {s.text}
            </span>
          );
        })}
      </span>
    </div>
  );
}

// The row between two days, shown while timestamps are on: the date in the viewer's
// own format, set apart like the restart divider.
export function DayDividerRow({ label }: { label: string }) {
  return (
    <div
      data-day-divider=""
      className="flex h-5 w-full min-w-full items-center gap-3 px-3 text-xs text-muted-foreground"
    >
      <span aria-hidden="true" className="h-px flex-1 bg-border" />
      <span className="shrink-0">{label}</span>
      <span aria-hidden="true" className="h-px flex-1 bg-border" />
    </div>
  );
}
