// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Ellipsis, Info, Trash2 } from "lucide-react";
import { Fragment, useState } from "react";

import type { OrphanRow, OrphansWithheld } from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { DeleteOrphansDialog } from "@/features/address-space/delete-orphans-dialog";
import { SubnetCell } from "@/features/address-space/subnet-map";
import { TruncatedText } from "@/features/resources/resource-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// Skeleton rows while the first read is out (UI-SPEC: 36px rows).
const SKELETON_ROWS = 8;
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

export type OrphanClass = OrphanRow["class"];

export interface OrphanGroups {
  orphaned: OrphanRow[];
  unused: OrphanRow[];
  cant_tell: OrphanRow[];
}

// groupOrphans only sorts the server's answer into its three groups. Which network is orphaned,
// unused or not yet judged is the server's call, never the browser's (D-10); the order inside a
// group is the server's.
export function groupOrphans(rows: readonly OrphanRow[]): OrphanGroups {
  const out: OrphanGroups = { orphaned: [], unused: [], cant_tell: [] };
  for (const row of rows) out[row.class].push(row);
  return out;
}

// The key of one row: a local network is judged per node, so the node is part of it.
export const orphanKey = (r: Pick<OrphanRow, "networkId" | "nodeId">) =>
  `${r.networkId}|${r.nodeId}`;

const GROUP_ORDER = ["orphaned", "unused", "cant_tell"] as const;

interface OrphansCardProps {
  envId: string;
  envName: string;
  // Undefined while the address space loads.
  orphans: readonly OrphanRow[] | undefined;
  // Present when the check could not judge every network.
  withheld?: OrphansWithheld | undefined;
  swarm: boolean;
}

// Who a Can't tell yet row is waiting for: the nodes that did not report.
function waitingFor(withheld: OrphansWithheld | undefined): string[] {
  return withheld?.nodes ?? [];
}

// IdleSinceCell is how long a network has been idle, with the words that say what idle means.
function IdleSinceCell({ row }: { row: OrphanRow }) {
  const t = useT();
  const words = t(`addressSpace.orphans.idle.${row.idleSinceSource}`);
  return (
    <span className="flex items-center gap-1">
      <RelativeTime value={row.idleSince} />
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
            tabIndex={0}
            role="img"
            aria-label={`${t("addressSpace.orphans.idle.about")}: ${words}`}
            className="rounded text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
          >
            <Info className="size-3.5" aria-hidden="true" />
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-80">{words}</TooltipContent>
      </Tooltip>
    </span>
  );
}

// OrphansCard is "Orphaned and unused networks": what the server found that nothing has used for 24
// hours, in three groups, with Delete… per row (NET-04). It only renders: the server decided
// every class, and decides again before it deletes.
export function OrphansCard({ envId, envName, orphans, withheld, swarm }: OrphansCardProps) {
  const t = useT();
  const manage = useNeeds("networks.manage", envId);
  const [deleting, setDeleting] = useState<OrphanRow | null>(null);
  const loading = orphans === undefined;
  const groups = groupOrphans(orphans ?? []);
  const waiting = waitingFor(withheld);
  const columnCount = swarm ? 6 : 5;

  const needsOn = (reason: string | undefined) =>
    t("addressSpace.orphans.needsOn", {
      need: reason ?? t("needs.role.admin"),
      name: envName,
    });
  const waitingReason =
    waiting.length > 0
      ? t("addressSpace.orphans.waiting", { nodes: waiting.join(", ") })
      : t("addressSpace.orphans.waitingUnknown");
  const deleteReason = (row: OrphanRow): string | undefined => {
    if (!manage.allowed) return needsOn(manage.reason);
    return row.class === "cant_tell" ? waitingReason : undefined;
  };

  const cantTellCaption =
    waiting.length > 0
      ? t("addressSpace.orphans.groups.cantTell.caption", { nodes: waiting.join(", ") })
      : withheld?.managerMissing
        ? t("addressSpace.orphans.groups.cantTell.captionManager")
        : t("addressSpace.orphans.groups.cantTell.captionOther");

  const empty = !loading && (orphans?.length ?? 0) === 0;
  const formerHeader = swarm
    ? t("addressSpace.orphans.columns.formerStack")
    : t("addressSpace.orphans.columns.formerProject");

  return (
    <Card id="orphans" className="scroll-mt-4" aria-busy={loading || undefined}>
      <CardHeader>
        <CardTitle>{t("addressSpace.orphans.title")}</CardTitle>
        <CardDescription>{t("addressSpace.orphans.caption")}</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {empty ? (
          <div className="flex flex-col gap-1 rounded-xl border p-6 text-center">
            <h3 className="text-base font-semibold">{t("addressSpace.orphans.empty.title")}</h3>
            <p className="text-sm text-muted-foreground">{t("addressSpace.orphans.empty.body")}</p>
          </div>
        ) : (
          <div className="relative max-h-[70svh] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
            <table data-slot="table" className="w-full caption-bottom text-sm">
              <caption className="sr-only">{t("addressSpace.orphans.tableLabel")}</caption>
              <TableHeader>
                <TableRow className={cn(ROW, "hover:bg-transparent")}>
                  {[
                    t("addressSpace.orphans.columns.network"),
                    ...(swarm ? [t("addressSpace.orphans.columns.node")] : []),
                    t("addressSpace.orphans.columns.subnet"),
                    formerHeader,
                    t("addressSpace.orphans.columns.idleSince"),
                  ].map((header) => (
                    <TableHead
                      key={header}
                      scope="col"
                      className="sticky top-0 z-10 h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11"
                    >
                      {header}
                    </TableHead>
                  ))}
                  <TableHead
                    scope="col"
                    className="sticky top-0 z-10 h-9 w-12 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11"
                  >
                    <span className="sr-only">{t("addressSpace.orphans.columns.actions")}</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              {loading ? (
                <TableBody>
                  {Array.from({ length: SKELETON_ROWS }, (_, i) => (
                    // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                    <TableRow key={i} className={ROW}>
                      {Array.from({ length: columnCount }, (_, c) => (
                        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton cells
                        <TableCell key={c} className={CELL}>
                          <Skeleton className="h-4 w-24" aria-hidden="true" />
                        </TableCell>
                      ))}
                    </TableRow>
                  ))}
                </TableBody>
              ) : (
                GROUP_ORDER.map((cls) => {
                  const rows = groups[cls];
                  if (rows.length === 0) return null;
                  const key = cls === "cant_tell" ? "cantTell" : cls;
                  return (
                    <Fragment key={cls}>
                      <TableBody>
                        <TableRow className="h-9 bg-muted hover:bg-muted max-md:h-11">
                          <TableHead
                            scope="rowgroup"
                            colSpan={columnCount}
                            className="px-3 py-1 text-start whitespace-normal"
                          >
                            <span className="flex flex-col gap-0.5">
                              <span className="text-sm font-semibold">
                                {t(`addressSpace.orphans.groups.${key}.name`, {
                                  count: rows.length,
                                })}
                              </span>
                              <span className="text-xs font-normal text-muted-foreground">
                                {cls === "cant_tell"
                                  ? cantTellCaption
                                  : t(`addressSpace.orphans.groups.${key}.caption`)}
                              </span>
                            </span>
                          </TableHead>
                        </TableRow>
                      </TableBody>
                      <TableBody>
                        {rows.map((row) => (
                          <OrphanTableRow
                            key={orphanKey(row)}
                            row={row}
                            swarm={swarm}
                            deleteReason={deleteReason(row)}
                            onDelete={() => setDeleting(row)}
                          />
                        ))}
                      </TableBody>
                    </Fragment>
                  );
                })
              )}
            </table>
          </div>
        )}
        {loading ? <span className="sr-only">{t("addressSpace.orphans.loading")}</span> : null}
      </CardContent>
      <DeleteOrphansDialog
        envId={envId}
        row={deleting}
        notReporting={waiting}
        onClose={() => setDeleting(null)}
      />
    </Card>
  );
}

interface OrphanTableRowProps {
  row: OrphanRow;
  swarm: boolean;
  // Why Delete… is unavailable, or undefined when it is available.
  deleteReason: string | undefined;
  onDelete: () => void;
}

function OrphanTableRow({ row, swarm, deleteReason, onDelete }: OrphanTableRowProps) {
  const t = useT();
  const former = row.formerOwner ? (
    <TruncatedText mono>{row.formerOwner}</TruncatedText>
  ) : (
    <span className="text-muted-foreground">{t("addressSpace.states.missing")}</span>
  );
  return (
    <TableRow className={cn(ROW)}>
      <TableCell className={cn(CELL, "min-w-44")}>
        <span className="flex min-w-0 flex-col">
          <TruncatedText>{row.name}</TruncatedText>
          {swarm && row.scope === "local" ? (
            <span className="text-xs text-muted-foreground">
              {t("addressSpace.orphans.onNode", { node: row.node })}
            </span>
          ) : null}
        </span>
      </TableCell>
      {swarm ? (
        <TableCell className={cn(CELL, "w-36 min-w-36")}>
          <TruncatedText>{row.node}</TruncatedText>
        </TableCell>
      ) : null}
      <TableCell className={cn(CELL, "w-44 min-w-44")}>
        <SubnetCell row={{ subnets: row.subnets ?? [] }} />
      </TableCell>
      <TableCell className={cn(CELL, "min-w-36")}>{former}</TableCell>
      <TableCell className={cn(CELL, "w-40 min-w-40")}>
        <IdleSinceCell row={row} />
      </TableCell>
      <TableCell className={cn(CELL, "w-12 text-end")}>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="max-md:size-11"
              aria-label={t("addressSpace.orphans.actions.menu", { name: row.name })}
            >
              <Ellipsis aria-hidden="true" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <NeedsTooltip reason={deleteReason}>
              <DropdownMenuItem
                variant="destructive"
                className="max-md:min-h-11"
                onSelect={() => {
                  if (!deleteReason) onDelete();
                }}
              >
                <Trash2 aria-hidden="true" />
                {t("addressSpace.orphans.actions.delete")}
              </DropdownMenuItem>
            </NeedsTooltip>
          </DropdownMenuContent>
        </DropdownMenu>
      </TableCell>
    </TableRow>
  );
}
