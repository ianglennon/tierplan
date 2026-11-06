// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { CircleCheck, CircleX, TriangleAlert } from "lucide-react";
import { useState } from "react";

import type { FleetNotIncluded, FleetPoolItem } from "@/api/fleet";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { UsageMeter } from "@/features/address-space/usage-meter";
import { FleetCoverageAlert } from "@/features/fleet/fleet-coverage-alert";
import { FLEET_VIRTUALISE_OVER, VirtualRows } from "@/features/fleet/virtual-rows";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A row is 36px (44px under 768px for touch).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";
const COLUMNS = 5;

interface FleetPoolsTableProps {
  // Every pool and overlay network at 70% or more used, fullest first: the server's order.
  items: readonly FleetPoolItem[];
  // The environments the caller may read whose address space could not be read.
  notIncluded: readonly FleetNotIncluded[] | null | undefined;
}

interface PoolRow {
  key: string;
  item: FleetPoolItem;
}

// FleetPoolsTable is the Address pools tab (ENV-10, D-17): the coverage alert and a table of every
// pool or overlay network at 70% or more used, each with a meter, its numbers and its state. The order
// is the server's, so the page and the Overview's tile agree on what is fullest. It says "No pool is
// getting full" only for the environments that reported. Past 100 rows only the rows in view are drawn.
export function FleetPoolsTable({ items, notIncluded }: FleetPoolsTableProps) {
  const t = useT();
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const missing = (notIncluded?.length ?? 0) > 0;
  const rows: PoolRow[] = items.map((item, i) => ({
    key: `${i}|${item.envId}|${item.kind}|${item.name}`,
    item,
  }));
  const virtual = rows.length > FLEET_VIRTUALISE_OVER;

  return (
    <>
      <FleetCoverageAlert notIncluded={notIncluded} />
      {rows.length === 0 ? (
        <Empty className="min-h-[30svh] border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <CircleCheck aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle role="heading" aria-level={2} className="text-base">
              {t(`fleet.pools.empty.${missing ? "headingPartial" : "heading"}`)}
            </EmptyTitle>
            <EmptyDescription>
              {t(`fleet.pools.empty.${missing ? "bodyPartial" : "body"}`)}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <>
          <div
            ref={setScroller}
            className={cn(
              "relative rounded-lg border",
              virtual ? "max-h-[70svh] overflow-auto" : "overflow-x-auto",
            )}
          >
            <table className="w-full caption-bottom text-sm">
              <caption className="sr-only">{t("fleet.pools.caption")}</caption>
              <TableHeader className={virtual ? "sticky top-0 z-10 bg-background" : undefined}>
                <TableRow className="h-12 hover:bg-transparent">
                  <TableHead scope="col" className="w-48">
                    {t("fleet.pools.columns.env")}
                  </TableHead>
                  <TableHead scope="col" className="min-w-56">
                    {t("fleet.pools.columns.name")}
                  </TableHead>
                  <TableHead scope="col" className="w-40">
                    {t("fleet.pools.columns.subnet")}
                  </TableHead>
                  <TableHead scope="col" className="w-56">
                    {t("fleet.pools.columns.used")}
                  </TableHead>
                  <TableHead scope="col" className="w-36">
                    {t("fleet.pools.columns.state")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {virtual ? (
                  <VirtualRows
                    rows={rows}
                    scroller={scroller}
                    columns={COLUMNS}
                    rowKey={(row) => row.key}
                    renderRow={(row) => <PoolTableRow key={row.key} item={row.item} />}
                  />
                ) : (
                  rows.map((row) => <PoolTableRow key={row.key} item={row.item} />)
                )}
              </TableBody>
            </table>
          </div>
          <p className="text-xs text-muted-foreground tabular-nums">
            {t("fleet.pools.count", { count: rows.length })}
          </p>
        </>
      )}
    </>
  );
}

function PoolTableRow({ item }: { item: FleetPoolItem }) {
  const t = useT();
  return (
    <TableRow className={ROW}>
      <TableCell className={cn(CELL, "w-48")}>
        <Link
          to="/env/$envId/address-space"
          params={{ envId: item.envId }}
          title={item.envName}
          className="block max-w-44 truncate rounded-sm hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          {item.envName}
        </Link>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-56")}>
        <TruncatedText mono className="max-w-64">
          {item.name}
        </TruncatedText>
      </TableCell>
      <TableCell className={cn(CELL, "w-40")}>
        <TruncatedText mono className="max-w-36">
          {item.subnet}
        </TruncatedText>
      </TableCell>
      <TableCell className={cn(CELL, "w-56")}>
        <div className="flex items-center gap-2">
          <UsageMeter
            variant="compact"
            used={item.used}
            capacity={item.capacity}
            unit={item.unit}
            label={item.name}
            state={item.state}
            percent={item.percent}
            estimated={item.estimated}
            showState={false}
          />
          <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
            {t("fleet.pools.usedOf", { used: item.used, capacity: item.capacity })}
          </span>
          {item.estimated ? (
            <span className="shrink-0 text-xs text-muted-foreground">
              {t("overview.cell.poolEstimated")}
            </span>
          ) : null}
        </div>
      </TableCell>
      <TableCell className={cn(CELL, "w-36 whitespace-nowrap")}>
        {item.state === "almost_full" ? (
          <span className="inline-flex items-center gap-1 text-xs font-semibold text-status-offline-text">
            <CircleX aria-hidden="true" className="size-4" />
            {t("fleet.pools.state.almost_full")}
          </span>
        ) : (
          <span className="inline-flex items-center gap-1 text-xs font-semibold text-status-warning-text">
            <TriangleAlert aria-hidden="true" className="size-4" />
            {t("fleet.pools.state.getting_full")}
          </span>
        )}
      </TableCell>
    </TableRow>
  );
}
