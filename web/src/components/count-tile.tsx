// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link, type LinkProps } from "@tanstack/react-router";
import { useState } from "react";

import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface CountTileProps {
  label: string;
  // The count. Zero shows "0", never blank.
  value?: number;
  // A muted line under the value, such as "5 running · 2 stopped".
  subline?: string;
  // With a destination the tile is a focusable link with a hover background.
  // Without one it is static: no hover state, not focusable (UI-SPEC).
  to?: LinkProps["to"];
  params?: LinkProps["params"];
  // The destination's search, such as the services list filtered to restart loops.
  search?: LinkProps["search"];
  // A skeleton replaces the value while the count loads.
  loading?: boolean;
  // The value shows "—" when the count could not be read.
  error?: boolean;
  // An offline dot before the label while the value is above zero: the restart-loop tile, which
  // counts a problem. The numerals stay the foreground colour (UI-SPEC "CountTile").
  dot?: "offline";
  // A swarm's per-node share of the value, for a tooltip on desktop. Radix
  // tooltips ignore touch, and the trigger is the tile itself, so this adds no
  // tap target and no second tab stop (UI-SPEC: per-node breakdown on touch).
  breakdown?: { node: string; value: number }[];
}

// How many nodes the tooltip lists before it counts the rest.
const BREAKDOWN_LINES = 8;

const FRAME =
  "flex flex-col gap-1 rounded-xl bg-card p-4 text-card-foreground ring-1 ring-foreground/10";

// CountTile is one number on the dashboard: label, value and an optional
// sub-line.
export function CountTile({
  label,
  value,
  subline,
  to,
  params,
  search,
  loading,
  error,
  dot,
  breakdown,
}: CountTileProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const shown =
    error || value === undefined
      ? t("dashboard.engine.missing")
      : new Intl.NumberFormat().format(value);
  const showDot = dot === "offline" && !loading && !error && value !== undefined && value > 0;
  const body = (
    <>
      {showDot ? (
        <span className="flex items-center gap-1.5">
          <span
            aria-hidden="true"
            className="inline-block size-2 shrink-0 rounded-full bg-status-offline"
          />
          <span className="text-xs font-semibold text-muted-foreground">{label}</span>
        </span>
      ) : (
        <span className="text-xs font-semibold text-muted-foreground">{label}</span>
      )}
      {loading ? (
        <Skeleton className="h-8 w-16" aria-hidden="true" />
      ) : (
        <span className="text-2xl font-semibold tabular-nums">{shown}</span>
      )}
      {subline && !loading && !error ? (
        <span className="text-xs text-muted-foreground">{subline}</span>
      ) : null}
    </>
  );

  const tile = to ? (
    <Link
      to={to}
      params={params}
      search={search}
      aria-busy={loading || undefined}
      className={cn(
        FRAME,
        "transition-colors hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
      )}
    >
      {body}
    </Link>
  ) : (
    <div aria-busy={loading || undefined} className={FRAME}>
      {body}
    </div>
  );

  // The tile is always inside the Tooltip, open only when there is something to
  // list, so it keeps its place in the tree (and the focus a keyboard user gave
  // it) when the counts arrive after the first render.
  const lines = loading || error ? [] : (breakdown ?? []);
  const listed = lines.slice(0, BREAKDOWN_LINES);
  const rest = lines.length - listed.length;
  // Node names come from agents and are placed as text (T-02-75).
  return (
    <Tooltip open={lines.length > 0 && open} onOpenChange={setOpen}>
      <TooltipTrigger asChild>{tile}</TooltipTrigger>
      <TooltipContent className="max-w-80 flex-col items-start gap-0.5 break-words">
        {listed.map((b) => (
          <span key={b.node}>
            {t("aggregate.tiles.breakdownLine", { node: b.node, count: b.value })}
          </span>
        ))}
        {rest > 0 ? <span>{t("aggregate.tiles.breakdownMore", { count: rest })}</span> : null}
      </TooltipContent>
    </Tooltip>
  );
}
