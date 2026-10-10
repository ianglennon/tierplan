// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type BufferedLine, stampTime } from "@/features/logs/log-buffer";
import { stripAnsi } from "@/features/logs/search";
import { cn } from "@/lib/utils";

// The height of a row. The virtualiser sizes every row by it, so the line height of
// the text is the same number (UI-SPEC "Typography", log lines).
export const LINE_HEIGHT_PX = 20;

// A run of text in one style.
export interface TextSpan {
  text: string;
  mark?: "match" | "current";
}

// markSpans cuts text at the edges of the ranges, which are [start, end) offsets into
// the text, and flags the pieces inside one. With no ranges the text is one span.
export function markSpans(
  text: string,
  ranges: ReadonlyArray<readonly [number, number]> | undefined,
  current: boolean,
): TextSpan[] {
  if (!ranges || ranges.length === 0) return [{ text }];
  const out: TextSpan[] = [];
  let at = 0;
  for (const [start, end] of ranges) {
    if (start > at) out.push({ text: text.slice(at, start) });
    if (end > start)
      out.push({ text: text.slice(start, end), mark: current ? "current" : "match" });
    at = Math.max(at, end);
  }
  if (at < text.length) out.push({ text: text.slice(at) });
  return out;
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
}: LogLineRowProps) {
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
  const spans = markSpans(stripAnsi(line.m), highlight?.ranges, highlight?.current === true);
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
        {spans.map((s, i) =>
          s.mark ? (
            <mark
              // The spans of one line are drawn once and in order.
              // biome-ignore lint/suspicious/noArrayIndexKey: positions are the identity
              key={i}
              data-match={s.mark}
              className={cn(
                "rounded-xs text-inherit",
                s.mark === "current" ? "bg-foreground text-background" : "bg-foreground/18",
              )}
            >
              {s.text}
            </mark>
          ) : (
            // biome-ignore lint/suspicious/noArrayIndexKey: positions are the identity
            <span key={i}>{s.text}</span>
          ),
        )}
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
