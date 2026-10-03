// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link, type LinkProps } from "@tanstack/react-router";

import { Skeleton } from "@/components/ui/skeleton";
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
  // A skeleton replaces the value while the count loads.
  loading?: boolean;
  // The value shows "—" when the count could not be read.
  error?: boolean;
  // A swarm's per-node share of the value, for a tooltip on desktop.
  breakdown?: { node: string; value: number }[];
}

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
  loading,
  error,
  breakdown: _breakdown,
}: CountTileProps) {
  const t = useT();
  const shown =
    error || value === undefined
      ? t("dashboard.engine.missing")
      : new Intl.NumberFormat().format(value);
  const body = (
    <>
      <span className="text-xs font-semibold text-muted-foreground">{label}</span>
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

  if (to) {
    return (
      <Link
        to={to}
        params={params}
        aria-busy={loading || undefined}
        className={cn(
          FRAME,
          "transition-colors hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
        )}
      >
        {body}
      </Link>
    );
  }
  return (
    <div aria-busy={loading || undefined} className={FRAME}>
      {body}
    </div>
  );
}
