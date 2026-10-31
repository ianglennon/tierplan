// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";

import type { AddressSpace } from "@/api/address-space";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type Column, TruncatedText } from "@/features/resources/resource-table";
import { nodeRowKey, useNodeColumn } from "@/features/resources/swarm-lists";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export type AddressMapRow = NonNullable<AddressSpace["networks"]>[number];

// The chips of the map, in the order they show, and the columns it sorts by. All of it lives in the
// URL, so a filtered map is a link.
export const MAP_SHOW_VALUES = ["all", "overlay", "local", "filling", "overlap"] as const;
export type MapShow = (typeof MAP_SHOW_VALUES)[number];
export const MAP_SORT_VALUES = ["name", "subnet", "used", "age"] as const;
export type MapSortKey = (typeof MAP_SORT_VALUES)[number];
export type MapSortDir = "asc" | "desc";

// What the route holds for the map. A key that is absent is the default.
export interface MapSearch {
  q?: string;
  show?: MapShow;
  node?: string;
  sort?: MapSortKey;
  dir?: MapSortDir;
}

// The skeleton rows that keep the layout steady while the map loads (UI-SPEC "Loading").
const SKELETON_ROWS = 8;

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// Cells: the whole text of an attached count, as the Networks list words it.
function AttachedCell({ row, swarm }: { row: AddressMapRow; swarm: boolean }) {
  const t = useT();
  if (row.attachedContainers === 0 && row.attachedServices === 0) {
    return <span className="text-muted-foreground">{t("networks.list.none")}</span>;
  }
  const containers = t("networks.list.attached.containers", { count: row.attachedContainers });
  // Services exist only on a swarm.
  return swarm
    ? t("networks.list.attached.both", {
        containers,
        services: t("networks.list.attached.services", { count: row.attachedServices }),
      })
    : containers;
}

// OwnerCell words who made the network. A stack Coaming manages is a link to it; a stack or compose
// project Coaming does not manage is its name in mono; a service-made network says which service.
// Every value is text, and the link is built from the stack's ID, never from the name.
function OwnerCell({ envId, row }: { envId: string; row: AddressMapRow }) {
  const t = useT();
  const missing = t("addressSpace.states.missing");
  if (row.ownerKind === "service") {
    return (
      <TruncatedText>{t("addressSpace.map.owner.service", { name: row.owner })}</TruncatedText>
    );
  }
  if ((row.ownerKind === "stack" || row.ownerKind === "project") && row.owner !== "") {
    if (row.ownerStackId !== "") {
      return (
        <Link
          to="/env/$envId/stacks/$stackId"
          params={{ envId, stackId: row.ownerStackId }}
          className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <TruncatedText>{row.owner}</TruncatedText>
        </Link>
      );
    }
    return <TruncatedText mono>{row.owner}</TruncatedText>;
  }
  return <span className="text-muted-foreground">{missing}</span>;
}

// SubnetCell is the first subnet in mono with "+n" for the rest, as the Networks list shows it.
function SubnetCell({ row }: { row: AddressMapRow }) {
  const t = useT();
  const [first, ...rest] = row.subnets ?? [];
  if (first === undefined) {
    return <span className="text-muted-foreground">{t("addressSpace.states.missing")}</span>;
  }
  return (
    <span className="flex items-center gap-1">
      <TruncatedText mono>{first}</TruncatedText>
      {rest.length > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
              tabIndex={0}
              className="shrink-0 rounded text-xs text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
            >
              +{rest.length}
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-80 break-all font-mono">
            {rest.join(", ")}
          </TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

// NetworkCell is the network's name, the one link of its row to the network's page, with the
// Ingress badge on the swarm's routing-mesh network.
function NetworkCell({ envId, row }: { envId: string; row: AddressMapRow }) {
  const t = useT();
  return (
    <span className="flex min-w-0 items-center gap-2">
      <Link
        to="/env/$envId/networks/$networkId"
        params={{ envId, networkId: row.id }}
        search={{ node: row.nodeId }}
        className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <TruncatedText>{row.name}</TruncatedText>
      </Link>
      {row.ingress ? <Badge variant="outline">{t("addressSpace.map.ingress")}</Badge> : null}
    </span>
  );
}

interface SubnetMapProps {
  envId: string;
  // Undefined while the address space loads.
  rows: readonly AddressMapRow[] | undefined;
  swarm: boolean;
  search: MapSearch;
  // Writes the given keys to the URL; a key set to undefined is removed from it.
  onSearchChange: (patch: Partial<MapSearch>) => void;
}

// SubnetMap is the Subnet map card: every network of the environment in the server's address order,
// with who owns it and what is attached (NET-01).
export function SubnetMap({ envId, rows, swarm }: SubnetMapProps) {
  const t = useT();
  const nodeColumn = useNodeColumn<AddressMapRow>();
  const loading = rows === undefined;
  const shown = rows ?? [];

  const columns: Column<AddressMapRow>[] = [
    {
      id: "name",
      header: t("addressSpace.map.columns.name"),
      className: "min-w-48",
      cell: (row) => <NetworkCell envId={envId} row={row} />,
    },
    {
      id: "subnet",
      header: t("addressSpace.map.columns.subnet"),
      className: "w-44 min-w-44",
      cell: (row) => <SubnetCell row={row} />,
    },
    {
      id: "scope",
      header: t("addressSpace.map.columns.scope"),
      className: "w-24 min-w-24",
      cell: (row) => t(`networks.list.scope.${row.scope}`),
    },
    {
      id: "driver",
      header: t("addressSpace.map.columns.driver"),
      className: "w-28 min-w-28",
      cell: (row) => row.driver || t("addressSpace.states.missing"),
    },
    ...(swarm ? [{ ...nodeColumn, className: "w-36 min-w-36" }] : []),
    {
      id: "owner",
      header: t("addressSpace.map.columns.owner"),
      className: "min-w-44",
      cell: (row) => <OwnerCell envId={envId} row={row} />,
    },
    {
      id: "attached",
      header: t("addressSpace.map.columns.attached"),
      className: "w-28 min-w-28",
      cell: (row) => <AttachedCell row={row} swarm={swarm} />,
    },
    {
      id: "age",
      header: t("addressSpace.map.columns.age"),
      className: "w-28 min-w-28",
      cell: (row) => <RelativeTime value={row.created} />,
    },
  ];

  return (
    <Card id="map" className="scroll-mt-4" aria-busy={loading || undefined}>
      <CardHeader>
        <CardTitle>{t("addressSpace.map.title")}</CardTitle>
        <CardDescription>{t("addressSpace.map.caption")}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="relative max-h-[70svh] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table data-slot="table" className="w-full caption-bottom text-sm">
            <caption className="sr-only">{t("addressSpace.map.tableLabel")}</caption>
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {columns.map((c) => (
                  <TableHead
                    key={c.id}
                    scope="col"
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
                    // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                    <TableRow key={i} className={ROW}>
                      {columns.map((c) => (
                        <TableCell key={c.id} className={cn(CELL, c.className)}>
                          <Skeleton className="h-4 w-24" aria-hidden="true" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))
                : shown.map((row) => (
                    <TableRow key={nodeRowKey(row.node, row.id)} className={ROW}>
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
        {loading ? (
          <span className="sr-only">{t("addressSpace.map.loading")}</span>
        ) : (
          <p className="text-sm text-muted-foreground tabular-nums">
            {t("addressSpace.map.count", { count: shown.length })}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
