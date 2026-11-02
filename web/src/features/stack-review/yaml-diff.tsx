// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useState } from "react";

import type { StackDiffHunk, StackDiffLine } from "@/api/stacks";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface YamlDiffProps {
  // The hunks the server computed, each with three lines of context. The page does no diffing.
  hunks: StackDiffHunk[] | null | undefined;
  // The lines of the newer text, so the unchanged lines between hunks can be shown on request.
  lines?: string[] | null;
  caption: string;
}

type Kind = StackDiffLine["kind"];

// What a row paints. The glyph is real text, so a change never depends on the tint alone.
const TINT: Record<Kind, string> = {
  added: "bg-status-online/8",
  removed: "bg-status-offline/8",
  context: "",
};
const GLYPH: Record<Kind, string> = { added: "+", removed: "−", context: "" };

interface GapRow {
  gap: true;
  id: string;
  count: number;
  oldStart: number;
  newStart: number;
}
interface LineRow {
  gap: false;
  id: string;
  line: StackDiffLine;
}

// rowsOf lays the hunks out in order with a gap row wherever lines the hunks do not carry sit
// between them (or before the first, or after the last). The numbers say how many: the hunks
// start at the number of the first line they hold, and what lies between two is what is left.
function rowsOf(hunks: StackDiffHunk[], total: number): (GapRow | LineRow)[] {
  const rows: (GapRow | LineRow)[] = [];
  let oldNext = 1;
  let newNext = 1;
  hunks.forEach((hunk, h) => {
    const count = hunk.oldStart - oldNext;
    if (count > 0) {
      rows.push({ gap: true, id: `gap-${h}`, count, oldStart: oldNext, newStart: newNext });
    }
    (hunk.lines ?? []).forEach((line, i) => {
      rows.push({ gap: false, id: `line-${h}-${i}`, line });
    });
    oldNext = hunk.oldStart + hunk.oldLines;
    newNext = hunk.newStart + hunk.newLines;
  });
  const rest = total - (newNext - 1);
  if (rest > 0) {
    rows.push({ gap: true, id: "gap-end", count: rest, oldStart: oldNext, newStart: newNext });
  }
  return rows;
}

// YamlDiff draws the hunks of a text diff the server computed: a native table with a caption,
// old and new line numbers, a glyph column and the line in mono, wrapped. Unchanged runs between
// hunks are a row that expands them (STK-06, D-11, A11). Every line is text (T-07-71).
export function YamlDiff({ hunks, lines, caption }: YamlDiffProps) {
  const t = useT();
  const [open, setOpen] = useState<Set<string>>(() => new Set());
  const list = hunks ?? [];
  const source = lines ?? [];
  if (list.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("stackYamlDiff.none")}</p>;
  }
  const rows = rowsOf(list, source.length);

  function expand(id: string) {
    setOpen((was) => new Set(was).add(id));
  }

  function lineRow(id: string, kind: Kind, old: number, next: number, text: string) {
    const n = kind === "removed" ? old : next;
    return (
      <tr
        key={id}
        aria-label={t(`stackYamlDiff.line.${kind}`, { n })}
        className={cn("h-4.5 align-top text-xs leading-4.5", TINT[kind])}
      >
        <td className="w-12 min-w-12 px-1 text-end text-muted-foreground tabular-nums select-none">
          {old > 0 ? old : ""}
        </td>
        <td className="w-12 min-w-12 px-1 text-end text-muted-foreground tabular-nums select-none">
          {next > 0 ? next : ""}
        </td>
        <td className="w-6 min-w-6 text-center select-none">{GLYPH[kind]}</td>
        <td className="pe-2 break-all whitespace-pre-wrap">{text}</td>
      </tr>
    );
  }

  return (
    <div className="overflow-x-auto rounded-md border bg-card">
      <table className="w-full border-collapse font-mono">
        <caption className="sr-only">{caption}</caption>
        <thead className="sr-only">
          <tr>
            <th scope="col">{t("stackYamlDiff.columns.old")}</th>
            <th scope="col">{t("stackYamlDiff.columns.new")}</th>
            <th scope="col">{t("stackYamlDiff.columns.change")}</th>
            <th scope="col">{t("stackYamlDiff.columns.text")}</th>
          </tr>
        </thead>
        <tbody>
          {rows.flatMap((row) => {
            if (!row.gap) {
              const { line } = row;
              return [lineRow(row.id, line.kind, line.old, line.new, line.text)];
            }
            if (open.has(row.id)) {
              return Array.from({ length: row.count }, (_, i) =>
                lineRow(
                  `${row.id}-${i}`,
                  "context",
                  row.oldStart + i,
                  row.newStart + i,
                  source[row.newStart - 1 + i] ?? "",
                ),
              );
            }
            const label = t("stackYamlDiff.show", { count: row.count });
            return [
              <tr key={row.id} className="h-9 bg-muted/50 font-sans text-xs">
                <td colSpan={4} className="px-2">
                  {source.length > 0 ? (
                    <button
                      type="button"
                      className="rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
                      onClick={() => expand(row.id)}
                    >
                      {label}
                    </button>
                  ) : (
                    <span className="text-muted-foreground">{label}</span>
                  )}
                </td>
              </tr>,
            ];
          })}
        </tbody>
      </table>
    </div>
  );
}
