// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Ellipsis, Info, Pin, Trash2 } from "lucide-react";
import { Fragment, useCallback, useMemo, useState } from "react";

import {
  type KeepTarget,
  OrphanRequestError,
  type OrphanRow,
  type OrphansWithheld,
  useKeepNetworks,
} from "@/api/address-space";
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
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { type BulkAction, BulkBar } from "@/features/resources/bulk-bar";
import { SelectCheckbox, TruncatedText } from "@/features/resources/resource-table";
import { useSelection } from "@/features/resources/selection";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
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
// The grace an environment has until it sets its own (the janitor's default, and the orphan check's).
export const DEFAULT_GRACE_HOURS = 24;

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
  // How many hours a network must have been idle before it is listed: the environment's janitor
  // grace, as the address space answers it. 24 until the page has read it.
  graceHours?: number;
}

// Who a Can't tell yet row is waiting for: the nodes that did not report.
function waitingFor(withheld: OrphansWithheld | undefined): string[] {
  return withheld?.nodes ?? [];
}

// IdleSinceCell is how long a network has been idle, with the words that say what idle means.
function IdleSinceCell({ row, graceHours }: { row: OrphanRow; graceHours: number }) {
  const t = useT();
  const words = t(`addressSpace.orphans.idle.${row.idleSinceSource}`, { count: graceHours });
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

// keepFailure is the sentence for a keep the server refused or never answered.
function keepFailure(
  t: (key: string, options?: Record<string, unknown>) => string,
  name: string,
  err: unknown,
): string {
  const e = err instanceof OrphanRequestError ? err : new OrphanRequestError(0);
  switch (e.type) {
    case "network_builtin":
      return t("addressSpace.orphans.refused.keepBuiltin", { name });
    case "network_not_found":
      return t("addressSpace.orphans.refused.gone", { name });
    case "node_required":
      return t("addressSpace.orphans.refused.keepNode", { name });
    default:
      return e.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
  }
}

const keepTargetOf = (r: OrphanRow): KeepTarget => ({
  name: r.name,
  // A local network is kept on its node; a swarm-wide one is kept for the cluster.
  ...(r.scope === "local" ? { nodeId: r.nodeId } : {}),
});

// OrphansCard is "Orphaned and unused networks": what the server found that nothing has used for the
// environment's grace (24 hours unless the janitor policy says otherwise), in three groups, with Keep and Delete… per row and in bulk (NET-04). It only renders: the
// server decided every class, and decides again before it deletes.
export function OrphansCard({
  envId,
  envName,
  orphans,
  withheld,
  swarm,
  graceHours = DEFAULT_GRACE_HOURS,
}: OrphansCardProps) {
  const t = useT();
  const manage = useNeeds("networks.manage", envId);
  const keepNeeds = useNeeds("networks.keep", envId);
  const keep = useKeepNetworks(envId);
  // The rows a delete is about stay while the dialog closes, so its title never reads "Delete ?"
  // on the way out.
  const [deleting, setDeleting] = useState<{ rows: OrphanRow[]; open: boolean }>({
    rows: [],
    open: false,
  });
  // The results a person asked to see from a toast's Details.
  const [report, setReport] = useState<BulkReport | null>(null);
  const loading = orphans === undefined;
  const groups = useMemo(() => groupOrphans(orphans ?? []), [orphans]);
  const waiting = waitingFor(withheld);
  // The columns: select, Network, Node (swarm), Subnet, Former stack, Idle since, actions.
  const columnCount = swarm ? 7 : 6;

  // Only Orphaned and Unused rows can be chosen: TierPlan never acts on a network it cannot be
  // sure of.
  const selectable = useMemo(() => [...groups.orphaned, ...groups.unused].map(orphanKey), [groups]);
  const isEligible = useCallback((key: string) => selectable.includes(key), [selectable]);
  const selection = useSelection<string>({ orderedKeys: selectable, isEligible });
  const byKey = useMemo(() => new Map((orphans ?? []).map((r) => [orphanKey(r), r])), [orphans]);
  // In the order the table shows them, whatever order they were chosen in.
  const chosen = selectable
    .filter((k) => selection.selected.has(k))
    .map((k) => byKey.get(k))
    .filter((r): r is OrphanRow => r !== undefined);

  const needsOn = (reason: string | undefined) =>
    t("addressSpace.orphans.needsOn", {
      need: reason ?? t("needs.role.admin"),
      name: envName,
    });
  const waitingReason =
    waiting.length > 0
      ? t("addressSpace.orphans.waiting", { nodes: waiting.join(", ") })
      : t("addressSpace.orphans.waitingUnknown");
  const keepReason = keepNeeds.allowed ? undefined : needsOn(keepNeeds.reason);
  const deleteReason = (row?: OrphanRow): string | undefined => {
    if (!manage.allowed) return needsOn(manage.reason);
    return row?.class === "cant_tell" ? waitingReason : undefined;
  };

  async function keepRows(rows: OrphanRow[]) {
    if (rows.length === 0 || keep.isPending) return;
    try {
      await keep.mutateAsync(rows.map(keepTargetOf));
      toastSuccess(
        t("addressSpace.orphans.keepDone", { count: rows.length, name: rows[0]?.name ?? "" }),
      );
      selection.clear();
    } catch (e) {
      toastError(keepFailure(t, rows[0]?.name ?? "", e));
    }
  }

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

  const bulkDeleteReason = deleteReason();
  const bulkActions: BulkAction[] = [
    {
      id: "keep",
      label: t("addressSpace.orphans.actions.keep"),
      onClick: () => {
        if (!keepReason) void keepRows(chosen);
      },
      ...(keepReason ? { reason: keepReason } : {}),
    },
    {
      id: "delete",
      label: t("addressSpace.orphans.actions.delete"),
      destructive: true,
      onClick: () => {
        if (!bulkDeleteReason) setDeleting({ rows: chosen, open: true });
      },
      ...(bulkDeleteReason ? { reason: bulkDeleteReason } : {}),
    },
  ];

  const headerCell =
    "sticky top-0 z-10 h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11";

  return (
    <Card id="orphans" className="scroll-mt-4" aria-busy={loading || undefined}>
      <CardHeader>
        <CardTitle>{t("addressSpace.orphans.title")}</CardTitle>
        {selection.selected.size > 0 ? (
          <BulkBar
            count={selection.selected.size}
            actions={bulkActions}
            onClear={selection.clear}
          />
        ) : (
          <CardDescription>
            {t("addressSpace.orphans.caption", { count: graceHours })}
          </CardDescription>
        )}
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <p className="text-sm text-muted-foreground">
          {t("addressSpace.orphans.janitor.note")}{" "}
          <Link
            to="/env/$envId/janitor"
            params={{ envId }}
            className="rounded-sm text-accent-ink underline underline-offset-3"
          >
            {t("addressSpace.orphans.janitor.link")}
          </Link>
        </p>
        {empty ? (
          <div className="flex flex-col gap-1 rounded-xl border p-6 text-center">
            <h3 className="text-base font-semibold">{t("addressSpace.orphans.empty.title")}</h3>
            <p className="text-sm text-muted-foreground">
              {t("addressSpace.orphans.empty.body", { count: graceHours })}
            </p>
          </div>
        ) : (
          <div className="relative max-h-[70svh] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
            <table data-slot="table" className="w-full caption-bottom text-sm">
              <caption className="sr-only">{t("addressSpace.orphans.tableLabel")}</caption>
              <TableHeader>
                <TableRow className={cn(ROW, "hover:bg-transparent")}>
                  <TableHead scope="col" className={cn(headerCell, "w-12 min-w-12 ps-3 pe-0")}>
                    <span className="sr-only">{t("addressSpace.orphans.columns.select")}</span>
                  </TableHead>
                  {[
                    t("addressSpace.orphans.columns.network"),
                    ...(swarm ? [t("addressSpace.orphans.columns.node")] : []),
                    t("addressSpace.orphans.columns.subnet"),
                    formerHeader,
                    t("addressSpace.orphans.columns.idleSince"),
                  ].map((header) => (
                    <TableHead key={header} scope="col" className={headerCell}>
                      {header}
                    </TableHead>
                  ))}
                  <TableHead scope="col" className={cn(headerCell, "w-12")}>
                    <span className="sr-only">{t("addressSpace.orphans.columns.actions")}</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              {loading ? (
                <TableBody>
                  {Array.from({ length: SKELETON_ROWS }, (_, i) => (
                    // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                    <TableRow key={i} className={ROW} data-slot="skeleton-row">
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
                  const groupLabel = t(`addressSpace.orphans.groups.${key}.label`);
                  const keys = rows.map(orphanKey);
                  const state = selection.groupState(keys);
                  return (
                    <Fragment key={cls}>
                      <TableBody>
                        <TableRow className="h-9 bg-muted hover:bg-muted max-md:h-11">
                          <TableHead
                            scope="rowgroup"
                            colSpan={columnCount}
                            className="px-3 py-1 text-start whitespace-normal"
                          >
                            <span className="flex items-center gap-3">
                              {cls === "cant_tell" ? null : (
                                <SelectCheckbox
                                  checked={
                                    state === "checked"
                                      ? true
                                      : state === "mixed"
                                        ? "indeterminate"
                                        : false
                                  }
                                  label={t("addressSpace.orphans.select.group", {
                                    group: groupLabel,
                                  })}
                                  onToggle={() => selection.selectGroup(keys)}
                                />
                              )}
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
                            graceHours={graceHours}
                            checked={selection.isSelected(orphanKey(row))}
                            onToggle={(shift) => selection.toggle(orphanKey(row), { shift })}
                            waitingReason={waitingReason}
                            keepReason={keepReason}
                            deleteReason={deleteReason(row)}
                            onKeep={() => void keepRows([row])}
                            onDelete={() => setDeleting({ rows: [row], open: true })}
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
        rows={deleting.rows}
        open={deleting.open}
        swarm={swarm}
        notReporting={waiting}
        onClose={() => setDeleting((d) => ({ ...d, open: false }))}
        onDone={selection.clear}
        onDetails={setReport}
      />
      <BulkResultDialog
        envId={envId}
        report={report}
        labels={swarm ? undefined : { node: null }}
        onClose={() => setReport(null)}
      />
    </Card>
  );
}

interface OrphanTableRowProps {
  row: OrphanRow;
  swarm: boolean;
  graceHours: number;
  checked: boolean;
  onToggle: (shift: boolean) => void;
  // Why a Can't tell yet row cannot be chosen.
  waitingReason: string;
  // Why Keep or Delete… is unavailable, or undefined when it is available.
  keepReason: string | undefined;
  deleteReason: string | undefined;
  onKeep: () => void;
  onDelete: () => void;
}

function OrphanTableRow({
  row,
  swarm,
  graceHours,
  checked,
  onToggle,
  waitingReason,
  keepReason,
  deleteReason,
  onKeep,
  onDelete,
}: OrphanTableRowProps) {
  const t = useT();
  const cantTell = row.class === "cant_tell";
  const former = row.formerOwner ? (
    <TruncatedText mono>{row.formerOwner}</TruncatedText>
  ) : (
    <span className="text-muted-foreground">{t("addressSpace.states.missing")}</span>
  );
  return (
    <TableRow className={cn(ROW)} data-state={checked ? "selected" : undefined}>
      <TableCell className={cn(CELL, "w-12 min-w-12 ps-3 pe-0")}>
        <SelectCheckbox
          checked={checked}
          label={t("addressSpace.orphans.select.row", { name: row.name })}
          onToggle={onToggle}
          {...(cantTell ? { reason: waitingReason } : {})}
        />
      </TableCell>
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
        <IdleSinceCell row={row} graceHours={graceHours} />
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
            <NeedsTooltip reason={keepReason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                onSelect={() => {
                  if (!keepReason) onKeep();
                }}
              >
                <Pin aria-hidden="true" />
                {t("addressSpace.orphans.actions.keep")}
              </DropdownMenuItem>
            </NeedsTooltip>
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
