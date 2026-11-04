// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ArrowDown, ArrowUp, ChevronsUpDown, CircleCheck } from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { type FleetFailingItem, useFleetFailing } from "@/api/fleet";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { EnvFilter } from "@/features/fleet/env-filter";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A row is 36px (44px under 768px for touch). Past 100 rows the table draws only the rows in view,
// inside max-h-[70svh] (UI-SPEC "Fleet tables").
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";
const SKELETON_ROWS = 6;
export const VIRTUALISE_OVER = 100;
const ROW_PX = 36;
const OVERSCAN = 10;
const COLUMNS = 6;

type SortKey = "name" | "env" | "since";
type SortDir = "asc" | "desc";

const MISSING = "—";

function time(since: string): number {
  const ms = Date.parse(since);
  return Number.isNaN(ms) ? Number.NaN : ms;
}

// sortItems orders the rows. A row whose time is not known goes last whichever way the time is
// sorted, and ties break on environment then name so the order never shuffles between reads.
export function sortItems(
  items: readonly FleetFailingItem[],
  key: SortKey,
  dir: SortDir,
): FleetFailingItem[] {
  const text = (a: string, b: string) => a.localeCompare(b, undefined, { sensitivity: "accent" });
  const sign = dir === "asc" ? 1 : -1;
  return [...items].sort((a, b) => {
    if (key === "since") {
      const x = time(a.since);
      const y = time(b.since);
      if (Number.isNaN(x) !== Number.isNaN(y)) return Number.isNaN(x) ? 1 : -1;
      if (!Number.isNaN(x) && x !== y) return sign * (x - y);
    } else if (key === "name") {
      const c = text(a.name, b.name);
      if (c !== 0) return sign * c;
    } else {
      const c = text(a.envName, b.envName);
      if (c !== 0) return sign * c;
    }
    return text(a.envName, b.envName) || text(a.name, b.name) || (a.id < b.id ? -1 : 1);
  });
}

interface FleetFailingTableProps {
  // The environments the table is narrowed to (empty: all the caller may read).
  env: readonly string[];
  onEnvChange: (ids: string[]) => void;
}

// FleetFailingTable is the Failing services tab (ENV-10, D-17): the coverage alert, the
// environment filter and the table of every failing service and container the caller may read.
// It says what it could not see and never claims "nothing is failing" over an environment that did
// not report.
export function FleetFailingTable({ env, onEnvChange }: FleetFailingTableProps) {
  const t = useT();
  const query = useFleetFailing(env);
  const environments = useQuery(environmentsQuery);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({ key: "since", dir: "desc" });

  const data = query.data;
  const items = useMemo(
    () => sortItems(data?.items ?? [], sort.key, sort.dir),
    [data?.items, sort.key, sort.dir],
  );
  const missing = (data?.notIncluded?.length ?? 0) > 0;
  const failed = query.isError && !data;
  const loading = !data && !failed;

  const onSort = (key: SortKey) =>
    setSort((old) =>
      old.key === key
        ? { key, dir: old.dir === "asc" ? "desc" : "asc" }
        : { key, dir: key === "since" ? "desc" : "asc" },
    );

  return (
    <>
      <FleetCoverageAlert notIncluded={data?.notIncluded} />
      <div className="flex flex-wrap items-center gap-2">
        <EnvFilter
          value={env}
          onChange={onEnvChange}
          environments={(environments.data?.items ?? []).map((e) => ({ id: e.id, name: e.name }))}
        />
      </div>
      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("fleet.failing.error.heading")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("fleet.failing.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("fleet.failing.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {!failed && data && items.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <CircleCheck aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>
              {missing ? t("fleet.failing.empty.headingPartial") : t("fleet.failing.empty.heading")}
            </EmptyTitle>
            <EmptyDescription>
              {missing ? t("fleet.failing.empty.bodyPartial") : t("fleet.failing.empty.body")}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}
      {!failed && (loading || items.length > 0) ? (
        <FailingRows items={items} loading={loading} sort={sort} onSort={onSort} />
      ) : null}
      {!failed && items.length > 0 ? (
        <p className="text-xs text-muted-foreground tabular-nums">
          {t("fleet.failing.count", { count: items.length })}
        </p>
      ) : null}
    </>
  );
}

interface FailingRowsProps {
  items: readonly FleetFailingItem[];
  loading: boolean;
  sort: { key: SortKey; dir: SortDir };
  onSort: (key: SortKey) => void;
}

function FailingRows({ items, loading, sort, onSort }: FailingRowsProps) {
  const t = useT();
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const virtual = items.length > VIRTUALISE_OVER;

  const renderRow = (item: FleetFailingItem) => <FailingRow key={rowKey(item)} item={item} />;

  return (
    <div
      ref={setScroller}
      className={cn(
        "rounded-lg border",
        virtual ? "max-h-[70svh] overflow-auto" : "overflow-x-auto",
      )}
    >
      <table
        className="w-full caption-bottom text-sm"
        aria-busy={loading || undefined}
        aria-label={loading ? t("fleet.failing.loading") : undefined}
      >
        <caption className="sr-only">{t("fleet.failing.caption")}</caption>
        <TableHeader className={virtual ? "sticky top-0 z-10 bg-background" : undefined}>
          <TableRow className="h-12 hover:bg-transparent">
            <SortHeader column="name" sort={sort} onSort={onSort} className="min-w-56" />
            <SortHeader column="env" sort={sort} onSort={onSort} className="w-48" />
            <TableHead scope="col" className="w-40">
              {t("fleet.failing.columns.stack")}
            </TableHead>
            <TableHead scope="col" className="w-40">
              {t("fleet.failing.columns.state")}
            </TableHead>
            <SortHeader column="since" sort={sort} onSort={onSort} className="w-32" />
            <TableHead scope="col" className="min-w-64">
              {t("fleet.failing.columns.error")}
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading ? (
            Array.from({ length: SKELETON_ROWS }, (_, i) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
              <TableRow key={i} className={ROW} aria-hidden="true">
                {Array.from({ length: COLUMNS }, (_, c) => (
                  // biome-ignore lint/suspicious/noArrayIndexKey: placeholder cells have no identity
                  <TableCell key={c} className={CELL}>
                    <Skeleton className="h-4 w-4/5" />
                  </TableCell>
                ))}
              </TableRow>
            ))
          ) : virtual ? (
            <VirtualRows rows={items} scroller={scroller} renderRow={renderRow} />
          ) : (
            items.map(renderRow)
          )}
        </TableBody>
      </table>
    </div>
  );
}

const rowKey = (item: FleetFailingItem) => `${item.envId}|${item.kind}|${item.nodeId}|${item.id}`;

function SortHeader({
  column,
  sort,
  onSort,
  className,
}: {
  column: SortKey;
  sort: { key: SortKey; dir: SortDir };
  onSort: (key: SortKey) => void;
  className: string;
}) {
  const t = useT();
  const active = sort.key === column ? sort.dir : undefined;
  const label = t(`fleet.failing.columns.${column}`);
  const Icon = active === "asc" ? ArrowUp : active === "desc" ? ArrowDown : ChevronsUpDown;
  return (
    <TableHead
      scope="col"
      className={className}
      aria-sort={active === "asc" ? "ascending" : active === "desc" ? "descending" : "none"}
    >
      <button
        type="button"
        className="-ms-1 inline-flex items-center gap-1 rounded-sm px-1 py-0.5 font-medium outline-none hover:underline focus-visible:ring-3 focus-visible:ring-ring/50 max-md:min-h-11"
        aria-label={t("fleet.failing.sortBy", { column: label })}
        onClick={() => onSort(column)}
      >
        {label}
        <Icon aria-hidden="true" className="size-3.5 text-muted-foreground" />
      </button>
    </TableHead>
  );
}

// The words of the state: a swarm service says how many of its tasks run, a standalone container
// says why it is listed. The server's own text is the fallback for a state this build does not
// know.
function useStateText() {
  const t = useT();
  return (item: FleetFailingItem): string => {
    if (item.kind === "service" && item.running !== null && item.desired !== null) {
      return t("fleet.failing.running", { running: item.running, desired: item.desired });
    }
    if (item.kind === "container") {
      if (item.state === "restarting" || item.state === "exited" || item.state === "unhealthy") {
        return t(`fleet.failing.containerState.${item.state}`);
      }
    }
    return item.state;
  };
}

function FailingRow({ item }: { item: FleetFailingItem }) {
  const stateText = useStateText()(item);
  return (
    <TableRow className={ROW}>
      <TableCell className={cn(CELL, "min-w-56")}>
        <ItemLink item={item} />
      </TableCell>
      <TableCell className={cn(CELL, "w-48")}>
        <Link
          to="/env/$envId"
          params={{ envId: item.envId }}
          title={item.envName}
          className="block max-w-44 truncate rounded-sm hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {item.envName}
        </Link>
      </TableCell>
      <TableCell className={cn(CELL, "w-40")}>
        {item.stack ? <TruncatedText className="max-w-36">{item.stack}</TruncatedText> : MISSING}
      </TableCell>
      <TableCell className={cn(CELL, "w-40 whitespace-nowrap")}>
        <StatusDot state="offline" label={stateText} />
      </TableCell>
      <TableCell className={cn(CELL, "w-32 whitespace-nowrap")}>
        <RelativeTime value={item.since || undefined} />
      </TableCell>
      <TableCell className={cn(CELL, "min-w-64")}>
        {item.lastError ? (
          <TruncatedText className="max-w-80">{item.lastError}</TruncatedText>
        ) : (
          MISSING
        )}
      </TableCell>
    </TableRow>
  );
}

// The name opens the service or the container in its environment. A container is found by its
// node as well as its ID.
function ItemLink({ item }: { item: FleetFailingItem }) {
  const className =
    "block max-w-60 truncate rounded-sm font-semibold hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";
  if (item.kind === "service") {
    return (
      <Link
        to="/env/$envId/services/$serviceId"
        params={{ envId: item.envId, serviceId: item.id }}
        title={item.name}
        className={className}
      >
        {item.name}
      </Link>
    );
  }
  return (
    <Link
      to="/env/$envId/containers/$containerId"
      params={{ envId: item.envId, containerId: item.id }}
      search={{ node: item.nodeId || undefined }}
      title={item.name}
      className={className}
    >
      {item.name}
    </Link>
  );
}

interface VirtualRowsProps {
  rows: readonly FleetFailingItem[];
  scroller: HTMLDivElement | null;
  renderRow: (row: FleetFailingItem) => ReactNode;
}

// VirtualRows draws the rows in view between two spacer rows that hold the height of the rest, so
// the table stays a native table and a long list stays responsive.
function VirtualRows({ rows, scroller, renderRow }: VirtualRowsProps) {
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller,
    estimateSize: () => ROW_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => {
      const row = rows[index];
      return row ? rowKey(row) : index;
    },
  });
  const inView = virtualizer.getVirtualItems();
  const first = inView[0];
  const last = inView[inView.length - 1];
  const before = first?.start ?? 0;
  const after = last ? virtualizer.getTotalSize() - last.end : 0;
  return (
    <>
      {before > 0 ? (
        <tr style={{ height: before }}>
          <td colSpan={COLUMNS} />
        </tr>
      ) : null}
      {inView.map((v) => {
        const row = rows[v.index];
        return row ? renderRow(row) : null;
      })}
      {after > 0 ? (
        <tr style={{ height: after }}>
          <td colSpan={COLUMNS} />
        </tr>
      ) : null}
    </>
  );
}
