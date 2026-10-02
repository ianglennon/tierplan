// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export type ResourceKind = "containers" | "images" | "volumes" | "networks";

// One column of a list. The cell renders from the row; the header is the
// translated label.
export interface Column<T> {
  id: string;
  header: string;
  cell: (row: T) => ReactNode;
  className?: string;
}

// The part of a React Query result the table reads, so it is not tied to one
// query hook and a test can hand it plain values.
export interface ListQuery<T> {
  data?: { items: T[] | null; total: number; truncated: boolean };
  isPending: boolean;
  isError: boolean;
  isFetching: boolean;
  refetch: () => unknown;
}

interface ResourceTableProps<T> {
  resource: ResourceKind;
  columns: Column<T>[];
  query: ListQuery<T>;
  // The environment's name, for "Nothing to show on {name} right now."
  envName: string;
  getRowKey: (row: T) => string;
}

// How many skeleton rows keep the layout stable while loading (UI-SPEC).
const SKELETON_ROWS = 8;

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

interface TruncatedTextProps {
  children: string;
  mono?: boolean;
  className?: string;
}

// TruncatedText cuts a long value with an ellipsis and keeps the whole value
// in a tooltip. The full text stays in the DOM, so screen readers read it.
export function TruncatedText({ children, mono, className }: TruncatedTextProps) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className={cn("block max-w-64 truncate", mono && "font-mono text-xs", className)}>
          {children}
        </span>
      </TooltipTrigger>
      <TooltipContent>{children}</TooltipContent>
    </Tooltip>
  );
}

// ResourceTable is the read-only list behind a dashboard tile (D-56): a title
// with a plural-aware count, a dense table with a sticky header, and every
// state of the UI-SPEC: loading skeletons, empty, an inline error with Retry,
// and the 500-row cap footer. There are no checkboxes, row actions or row
// links in Phase 1, and rows keep the server's order, so it never sorts.
export function ResourceTable<T>({
  resource,
  columns,
  query,
  envName,
  getRowKey,
}: ResourceTableProps<T>) {
  const t = useT();
  const key = `dashboard.lists.${resource}`;
  const data = query.data;
  const rows = data?.items ?? [];
  const failed = query.isError && !data;
  const loading = query.isPending;
  const empty = !loading && !failed && rows.length === 0;

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-2xl font-semibold">{t(`${key}.title`)}</h1>
        {data ? (
          <span className="text-sm text-muted-foreground tabular-nums">
            {t(`${key}.count`, { count: data.total })}
          </span>
        ) : null}
      </header>

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t(`${key}.error`)}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("dashboard.lists.errorBody")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("dashboard.lists.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      {empty ? (
        <Empty className="min-h-[30svh] border">
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={2} className="text-base">
              {t(`${key}.empty`)}
            </EmptyTitle>
            <EmptyDescription>{t("dashboard.lists.emptyBody", { name: envName })}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}

      {!failed && !empty ? (
        // The wrapper scrolls sideways on a phone (D-64) and, past the page
        // height, down, so the sticky header has something to stick to.
        <div className="max-h-[calc(100dvh-14rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table
            data-slot="table"
            aria-busy={loading || undefined}
            aria-label={t(`${key}.title`)}
            className="w-full caption-bottom text-sm"
          >
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {columns.map((c) => (
                  <TableHead
                    key={c.id}
                    className={cn(
                      "sticky top-0 z-10 h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11",
                      c.className,
                    )}
                  >
                    {c.header}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {loading
                ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                    // Placeholder rows have no identity of their own.
                    // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                    <TableRow key={i} className={ROW}>
                      {columns.map((c) => (
                        <TableCell key={c.id} className={cn(CELL, c.className)}>
                          <Skeleton className="h-4 w-24" aria-hidden="true" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : rows.map((row) => (
                    <TableRow key={getRowKey(row)} className={ROW}>
                      {columns.map((c) => (
                        <TableCell key={c.id} className={cn(CELL, c.className)}>
                          {c.cell(row)}
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
            </TableBody>
          </table>
        </div>
      ) : null}

      {data?.truncated ? (
        <p className="text-sm text-muted-foreground">
          {t("dashboard.lists.cap", { total: data.total })}
        </p>
      ) : null}
    </div>
  );
}
