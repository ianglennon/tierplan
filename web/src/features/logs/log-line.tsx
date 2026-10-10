// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { BufferedLine } from "@/features/logs/log-buffer";
import { cn } from "@/lib/utils";

// The height of a row. The virtualiser sizes every row by it, so the line height of
// the text is the same number (UI-SPEC "Typography", log lines).
export const LINE_HEIGHT_PX = 20;

// One line of a log: a stream gutter that is always as wide as the word "stderr" so
// lines never shift, then the text. A stderr line carries the visible "stderr" marker
// in the gutter, a 2px bar and a faint tint; the marker, not the colour, is what says
// it is stderr. The marker is real text for a screen reader and cannot be selected, so
// copying lines does not copy it.
//
// The text is a React text node: it is never read as markup, never linkified and never
// shortened (T-04-67).
export function LogLineRow({
  line,
  stderrLabel,
  markerLabel,
}: {
  line: BufferedLine;
  stderrLabel: string;
  // What a marker row says, already translated; used only for marker rows.
  markerLabel: (line: BufferedLine) => string;
}) {
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
  return (
    <div
      data-stream={stderr ? "stderr" : "stdout"}
      className={cn(
        "flex h-5 w-max min-w-full items-center border-s-2 font-mono text-xs leading-5 whitespace-pre",
        stderr ? "border-status-offline bg-status-offline/8" : "border-transparent",
      )}
    >
      <span className="w-[6ch] shrink-0 select-none text-muted-foreground">
        {stderr ? stderrLabel : ""}
      </span>
      <span className="ps-2">{line.m}</span>
    </div>
  );
}
