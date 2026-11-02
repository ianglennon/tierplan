// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { Sparkline } from "@/features/stats/sparkline";

interface StatTileProps {
  label: string;
  // Words on the label's Tooltip, for a label that needs one.
  hint?: string;
  // The lines under the label: a value, or an In and Out pair. Numbers use tabular
  // figures and never truncate.
  lines: ReactNode[];
  // A muted line under the value, such as "60% of a 2 GiB limit".
  subline?: string;
  // The history beside the value; left out for a tile without a trend.
  trend?: ReadonlyArray<number | null>;
  // A skeleton replaces the value until the first sample arrives.
  loading?: boolean;
}

// StatTile is one current number of the stats panel, with its trend. Tile numerals are
// plain foreground text: no accent and no status colour (UI-SPEC Color).
export function StatTile({ label, hint, lines, subline, trend, loading }: StatTileProps) {
  const heading = <span className="text-xs font-semibold text-muted-foreground">{label}</span>;
  return (
    <div className="flex flex-col gap-1 rounded-xl bg-card p-4 text-card-foreground ring-1 ring-foreground/10">
      {hint ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="w-fit cursor-help rounded-sm text-start focus-visible:ring-2 focus-visible:ring-accent-ink focus-visible:outline-none"
            >
              {heading}
            </button>
          </TooltipTrigger>
          <TooltipContent>{hint}</TooltipContent>
        </Tooltip>
      ) : (
        heading
      )}
      {loading ? (
        <Skeleton className="h-8 w-24" aria-hidden="true" />
      ) : (
        <div className="flex items-end justify-between gap-2">
          <div className="flex min-w-0 flex-col">
            {lines.map((line, i) => (
              <span
                // The lines are fixed by the tile, never reordered.
                // biome-ignore lint/suspicious/noArrayIndexKey: fixed positions
                key={i}
                className={
                  lines.length === 1
                    ? "text-2xl font-semibold tabular-nums"
                    : "text-sm font-semibold tabular-nums"
                }
              >
                {line}
              </span>
            ))}
          </div>
          {trend ? <Sparkline values={trend} /> : null}
        </div>
      )}
      {subline && !loading ? (
        <span className="text-xs text-muted-foreground">{subline}</span>
      ) : null}
    </div>
  );
}
