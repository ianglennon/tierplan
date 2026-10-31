// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";
import { useMemo } from "react";

import type { AddressSpace } from "@/api/address-space";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SearchBox } from "@/features/containers/container-list";
import {
  type Column,
  NodeSelect,
  SortHeader,
  TruncatedText,
} from "@/features/resources/resource-table";
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

// How many rows the table draws. The server holds up to 5000; past this many the table says so and
// the search finds the rest (UI-SPEC "Shared state and overflow rules").
export const MAP_RENDER_CAP = 500;

// The overlays are the swarm's own networks that cross hosts.
export function isOverlay(row: Pick<AddressMapRow, "scope" | "driver">): boolean {
  return row.scope === "swarm" && row.driver === "overlay";
}

// filterMapRows keeps the rows that match the search text (name, any subnet or owner, whatever the
// case), the chip and the node. The overlaps and the usage state were decided on the server; this
// only reads them.
export function filterMapRows(
  rows: readonly AddressMapRow[],
  { q, show, node }: Pick<MapSearch, "q" | "show" | "node">,
): AddressMapRow[] {
  const needle = q?.trim().toLowerCase();
  return rows.filter((row) => {
    if (node !== undefined && row.node !== node) return false;
    if (show === "overlay" && !isOverlay(row)) return false;
    if (show === "local" && row.scope !== "local") return false;
    if (
      show === "filling" &&
      row.usage?.state !== "getting_full" &&
      row.usage?.state !== "almost_full"
    )
      return false;
    if (show === "overlap" && (row.overlaps ?? []).length === 0) return false;
    if (!needle) return true;
    return (
      row.name.toLowerCase().includes(needle) ||
      row.owner.toLowerCase().includes(needle) ||
      (row.subnets ?? []).some((c) => c.toLowerCase().includes(needle))
    );
  });
}

const byOrder = (a: AddressMapRow, b: AddressMapRow) => a.order - b.order;

// sortMapRows orders the rows by a column. Subnet is the server's order, which compares addresses
// numerically ("10.0.9.0" before "10.0.10.0"); a row with nothing to compare (no count, no age)
// stays after the others in either direction.
export function sortMapRows(
  rows: readonly AddressMapRow[],
  key: MapSortKey,
  dir: MapSortDir,
): AddressMapRow[] {
  const sign = dir === "asc" ? 1 : -1;
  if (key === "subnet") return [...rows].sort((a, b) => sign * (a.order - b.order));
  if (key === "name") {
    return [...rows].sort(
      (a, b) =>
        sign * (a.name.localeCompare(b.name) || a.node.localeCompare(b.node)) || byOrder(a, b),
    );
  }
  const measured: AddressMapRow[] = [];
  const rest: AddressMapRow[] = [];
  for (const row of rows) {
    const has = key === "used" ? row.usage !== undefined : ageOf(row) !== undefined;
    (has ? measured : rest).push(row);
  }
  measured.sort((a, b) => {
    if (key === "used" && a.usage && b.usage) {
      // used / capacity compared as a cross product, so no rounding decides the order.
      const diff = a.usage.used * b.usage.capacity - b.usage.used * a.usage.capacity;
      return sign * Math.sign(diff) || byOrder(a, b);
    }
    return sign * ((ageOf(a) ?? 0) - (ageOf(b) ?? 0)) || byOrder(a, b);
  });
  return [...measured, ...rest.sort(byOrder)];
}

function ageOf(row: AddressMapRow): number | undefined {
  if (!row.created) return undefined;
  const ms = Date.parse(row.created);
  return Number.isNaN(ms) ? undefined : ms;
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

// How many partners the flag's tooltip lists before it counts the rest.
const OVERLAPS_LISTED = 5;

// OverlapFlag marks a network whose subnet overlaps another's. The words are real text for assistive
// technology; the tooltip names each subnet and, for a local partner, its node.
function OverlapFlag({ overlaps }: { overlaps: NonNullable<AddressMapRow["overlaps"]> }) {
  const t = useT();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
          tabIndex={0}
          className="shrink-0 rounded-sm text-status-warning-text outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        >
          <TriangleAlert aria-hidden="true" className="size-4" />
          <span className="sr-only">
            {t("addressSpace.map.overlap.hidden", { name: overlaps.map((o) => o.name).join(", ") })}
          </span>
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 break-all">
        {overlaps.slice(0, OVERLAPS_LISTED).map((o) => (
          <div key={`${o.node}|${o.name}|${o.subnet}`}>
            {o.node
              ? t("addressSpace.map.overlap.tipNode", {
                  name: o.name,
                  subnet: o.subnet,
                  node: o.node,
                })
              : t("addressSpace.map.overlap.tip", { name: o.name, subnet: o.subnet })}
          </div>
        ))}
        {overlaps.length > OVERLAPS_LISTED ? (
          <div>
            {t("addressSpace.map.overlap.more", { count: overlaps.length - OVERLAPS_LISTED })}
          </div>
        ) : null}
      </TooltipContent>
    </Tooltip>
  );
}

// SubnetCell is the first subnet in mono with "+n" for the rest, as the Networks list shows it.
function SubnetCell({ row }: { row: AddressMapRow }) {
  const t = useT();
  const [first, ...rest] = row.subnets ?? [];
  const overlaps = row.overlaps ?? [];
  if (first === undefined) {
    return <span className="text-muted-foreground">{t("addressSpace.states.missing")}</span>;
  }
  return (
    <span className="flex items-center gap-1">
      {overlaps.length > 0 ? <OverlapFlag overlaps={overlaps} /> : null}
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
  // How many networks the environment has, and whether the server sent fewer rows than that.
  total: number;
  truncated: boolean;
  swarm: boolean;
  search: MapSearch;
  // Writes the given keys to the URL; a key set to undefined is removed from it.
  onSearchChange: (patch: Partial<MapSearch>) => void;
}

// The chip row: one choice of five, All being none. Pressing the chosen chip is All.
function ShowChips({
  value,
  onChange,
}: {
  value: MapShow | undefined;
  onChange: (show: MapShow | undefined) => void;
}) {
  const t = useT();
  return (
    <ToggleGroup
      type="single"
      // The chips are toggle buttons in a labelled group, not a toolbar.
      role="group"
      variant="outline"
      size="sm"
      spacing={2}
      aria-label={t("addressSpace.map.show.label")}
      value={value ?? "all"}
      onValueChange={(v) =>
        onChange(
          v === "overlay" || v === "local" || v === "filling" || v === "overlap" ? v : undefined,
        )
      }
      className="max-md:flex-wrap"
    >
      {MAP_SHOW_VALUES.map((v) => (
        <ToggleGroupItem
          key={v}
          value={v}
          className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
        >
          {t(`addressSpace.map.show.${v}`)}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

// SubnetMap is the Subnet map card: every network of the environment in the server's address order,
// with who owns it, what is attached and where it overlaps another (NET-01). The search, the chip,
// the node and the sort come from the URL; at most MAP_RENDER_CAP rows are drawn.
export function SubnetMap({
  envId,
  rows,
  total,
  truncated,
  swarm,
  search,
  onSearchChange,
}: SubnetMapProps) {
  const t = useT();
  const nodeColumn = useNodeColumn<AddressMapRow>();
  const loading = rows === undefined;
  const { q, show, node } = search;
  const sortKey = search.sort ?? "subnet";
  const sortDir = search.dir ?? "asc";

  const all = rows;
  const matched = useMemo(
    () =>
      sortMapRows(
        filterMapRows(all ?? [], { q, show, node: swarm ? node : undefined }),
        sortKey,
        sortDir,
      ),
    [all, q, show, node, swarm, sortKey, sortDir],
  );
  const nodes = useMemo(
    () => [...new Set((all ?? []).map((r) => r.node).filter(Boolean))].sort(),
    [all],
  );
  const drawn = matched.slice(0, MAP_RENDER_CAP);
  const filtered =
    Boolean(q) || (show !== undefined && show !== "all") || (swarm && node !== undefined);

  function onSort(key: string) {
    const k = key as MapSortKey;
    // The column that is sorted flips; another starts ascending, except use, whose fullest come first.
    if (k === sortKey) onSearchChange({ sort: k, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: k, dir: k === "used" ? "desc" : "asc" });
  }

  const columns: Column<AddressMapRow>[] = [
    {
      id: "name",
      header: t("addressSpace.map.columns.name"),
      className: "min-w-48",
      sortKey: "name",
      cell: (row) => <NetworkCell envId={envId} row={row} />,
    },
    {
      id: "subnet",
      header: t("addressSpace.map.columns.subnet"),
      className: "w-44 min-w-44",
      sortKey: "subnet",
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
      sortKey: "age",
      cell: (row) => <RelativeTime value={row.created} />,
    },
  ];

  const empty =
    !loading && matched.length === 0 ? (
      rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("addressSpace.map.none")}</p>
      ) : (
        <Empty className="min-h-[20svh] border">
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={3} className="text-base break-words">
              {q ? t("addressSpace.map.empty.query", { q }) : t("addressSpace.map.empty.filter")}
            </EmptyTitle>
          </EmptyHeader>
          <EmptyContent>
            <Button
              variant="outline"
              className="max-md:h-11"
              onClick={() => onSearchChange({ q: undefined, show: undefined, node: undefined })}
            >
              {t("addressSpace.map.empty.clear")}
            </Button>
          </EmptyContent>
        </Empty>
      )
    ) : null;

  return (
    <Card id="map" className="scroll-mt-4" aria-busy={loading || undefined}>
      <CardHeader>
        <CardTitle>{t("addressSpace.map.title")}</CardTitle>
        <CardDescription>{t("addressSpace.map.caption")}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
          <SearchBox
            value={q}
            onCommit={(next) => onSearchChange({ q: next })}
            label={t("addressSpace.map.search.label")}
            placeholder={t("addressSpace.map.search.placeholder")}
            clearLabel={t("addressSpace.map.search.clear")}
          />
          <ShowChips value={show} onChange={(next) => onSearchChange({ show: next })} />
          {swarm ? (
            <NodeSelect
              value={node}
              onChange={(next) => onSearchChange({ node: next })}
              nodes={nodes}
            />
          ) : null}
        </div>
        {empty ?? (
          <div className="relative max-h-[70svh] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
            <table data-slot="table" className="w-full caption-bottom text-sm">
              <caption className="sr-only">{t("addressSpace.map.tableLabel")}</caption>
              <TableHeader>
                <TableRow className={cn(ROW, "hover:bg-transparent")}>
                  {columns.map((c) => {
                    const sortable = c.sortKey !== undefined;
                    const active = sortable && c.sortKey === sortKey;
                    return (
                      <TableHead
                        key={c.id}
                        scope="col"
                        aria-sort={
                          sortable
                            ? active
                              ? sortDir === "asc"
                                ? "ascending"
                                : "descending"
                              : "none"
                            : undefined
                        }
                        className={cn(
                          "sticky top-0 z-10 h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11",
                          c.className,
                        )}
                      >
                        {sortable && c.sortKey !== undefined ? (
                          <SortHeader
                            label={c.header}
                            active={active}
                            dir={sortDir}
                            onClick={() => onSort(c.sortKey as string)}
                          />
                        ) : (
                          c.header
                        )}
                      </TableHead>
                    );
                  })}
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
                  : drawn.map((row) => (
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
        )}
        {loading ? (
          <span className="sr-only">{t("addressSpace.map.loading")}</span>
        ) : matched.length > 0 ? (
          <div className="flex flex-col gap-1 text-sm text-muted-foreground">
            <p className="tabular-nums">{t("addressSpace.map.count", { count: matched.length })}</p>
            {matched.length > MAP_RENDER_CAP ? (
              <p>{t("addressSpace.map.cap", { cap: MAP_RENDER_CAP, total: matched.length })}</p>
            ) : null}
            {truncated && !filtered ? (
              <p>{t("addressSpace.map.truncated", { rows: rows.length, total })}</p>
            ) : null}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}
