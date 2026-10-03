// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";
import { useId } from "react";

import { CLUSTER_NODE } from "@/api/resources";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
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
  data?: {
    items: T[] | null;
    total: number;
    truncated: boolean;
    // On a swarm: the nodes that reported, and the ones that should have.
    nodes?: string[] | null;
    notReporting?: string[] | null;
  };
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
  // Swarm lists: the Node column goes after the first column, and the Node
  // select filters by it. Both are left out for a standalone host.
  nodeColumn?: Column<T>;
  nodeFilter?: NodeFilter;
  // Shown under the title, above the filter: the "not reporting" banner.
  banner?: ReactNode;
}

export interface NodeFilter {
  // The node in the URL, or undefined for all nodes.
  value?: string;
  onChange: (node: string | undefined) => void;
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

// The Select cannot hold an empty value, and a node name is whatever an agent
// reported, so every node is prefixed and "all nodes" is the one value without
// the prefix: no name can collide with it.
const ALL_NODES = "all";
const NODE_PREFIX = "node:";

interface NodeSelectProps extends NodeFilter {
  // The nodes that reported, from the list answer.
  nodes: readonly string[];
}

// NodeSelect is the Node filter above a swarm list: "All nodes" then each
// hostname. The node in the URL stays selectable even when the server no longer
// lists it (it stopped reporting), so the select never shows a blank.
function NodeSelect({ value, onChange, nodes }: NodeSelectProps) {
  const t = useT();
  const id = useId();
  const names = value && !nodes.includes(value) ? [...nodes, value] : nodes;
  return (
    <div className="flex items-center gap-2">
      <Label htmlFor={id} className="text-sm text-muted-foreground">
        {t("aggregate.filter.label")}
      </Label>
      <Select
        value={value === undefined ? ALL_NODES : `${NODE_PREFIX}${value}`}
        onValueChange={(v) => onChange(v === ALL_NODES ? undefined : v.slice(NODE_PREFIX.length))}
      >
        <SelectTrigger id={id} size="sm" className="min-w-40 max-w-64 max-md:h-11">
          <SelectValue />
        </SelectTrigger>
        <SelectContent position="popper" align="start" className="max-h-72">
          <SelectItem value={ALL_NODES}>{t("aggregate.filter.all")}</SelectItem>
          {names.map((n) => (
            <SelectItem key={n} value={`${NODE_PREFIX}${n}`}>
              {n}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
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
  nodeColumn,
  nodeFilter,
  banner,
}: ResourceTableProps<T>) {
  const t = useT();
  const key = `dashboard.lists.${resource}`;
  const data = query.data;
  const rows = data?.items ?? [];
  const failed = query.isError && !data;
  const loading = query.isPending;
  const empty = !loading && !failed && rows.length === 0;

  // The Node column goes right after the first one (Name).
  const shownColumns = nodeColumn
    ? [...columns.slice(0, 1), nodeColumn, ...columns.slice(1)]
    : columns;
  const nodes = data?.nodes ?? [];
  // The swarm-wide Cluster entry is in the filter but is not a node.
  const nodeCount = nodes.filter((n) => n !== CLUSTER_NODE).length;
  const count = t(`${key}.count`, { count: data?.total ?? 0 });
  let caption = count;
  if (nodeColumn && nodeFilter?.value !== undefined) {
    caption = `${count} ${t("aggregate.caption.on", { node: nodeFilter.value })}`;
  } else if (nodeColumn && nodeCount > 0) {
    caption = `${count} ${t("aggregate.caption.across", { count: nodeCount })}`;
  }
  // "Nothing to show on {name} right now." names the node while one is chosen.
  const emptyName = nodeFilter?.value ?? envName;

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-2xl font-semibold">{t(`${key}.title`)}</h1>
        {data ? (
          <span className="text-sm text-muted-foreground tabular-nums">{caption}</span>
        ) : null}
      </header>

      {banner}

      {nodeFilter ? <NodeSelect {...nodeFilter} nodes={nodes} /> : null}

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
            <EmptyDescription>
              {t("dashboard.lists.emptyBody", { name: emptyName })}
            </EmptyDescription>
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
                {shownColumns.map((c) => (
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
                      {shownColumns.map((c) => (
                        <TableCell key={c.id} className={cn(CELL, c.className)}>
                          <Skeleton className="h-4 w-24" aria-hidden="true" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : rows.map((row) => (
                    <TableRow key={getRowKey(row)} className={ROW}>
                      {shownColumns.map((c) => (
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
