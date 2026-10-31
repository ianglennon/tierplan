// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronDown, ChevronRight, PinOff } from "lucide-react";
import { useRef, useState } from "react";

import { type KeptRow, OrphanRequestError, useUnkeepNetwork } from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { SubnetCell } from "@/features/address-space/subnet-map";
import { TruncatedText } from "@/features/resources/resource-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// A keep follows the name, and for a local network its node.
export const keptKey = (k: Pick<KeptRow, "name" | "nodeId">) => `${k.name}|${k.nodeId}`;

interface KeptListProps {
  envId: string;
  envName: string;
  // Undefined while the address space loads.
  kept: readonly KeptRow[] | undefined;
  swarm: boolean;
}

// KeptList is the "Kept networks (n)" collapsible: the networks Coaming never lists as orphaned or
// unused (D-11). It is closed until opened and left out when nothing is kept. A keep a person made
// can be taken back; one made by the coaming.keep label can only be removed from the network.
export function KeptList({ envId, envName, kept, swarm }: KeptListProps) {
  const t = useT();
  const needs = useNeeds("networks.keep", envId);
  const unkeep = useUnkeepNetwork(envId);
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const rows = kept ?? [];
  if (rows.length === 0) return null;

  async function stop(row: KeptRow, index: number) {
    if (unkeep.isPending) return;
    // The button that was pressed leaves with its row, so focus moves first: to the next row's
    // button, or to the trigger.
    const next = rows[index + 1];
    const target = (next ? buttons.current.get(keptKey(next)) : undefined) ?? trigger.current;
    target?.focus();
    try {
      await unkeep.mutateAsync({ name: row.name, ...(row.nodeId ? { nodeId: row.nodeId } : {}) });
      toastSuccess(t("addressSpace.kept.done", { name: row.name }));
    } catch (e) {
      const err = e instanceof OrphanRequestError ? e : new OrphanRequestError(0);
      if (err.type === "keep_not_found") {
        toastInfo(t("addressSpace.kept.notKept", { name: row.name }));
      } else if (err.type === "kept_by_label") {
        toastError(t("addressSpace.kept.byLabel", { name: row.name }));
      } else {
        toastError(
          err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic"),
        );
      }
    }
  }

  const headerCell = "h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11";
  const userName = (row: KeptRow) =>
    row.keptByName
      ? t("addressSpace.kept.by.user", { user: row.keptByName })
      : t("addressSpace.kept.by.unknown");

  return (
    <Collapsible
      id="kept"
      open={open}
      onOpenChange={setOpen}
      className="scroll-mt-4 rounded-xl ring-1 ring-foreground/10"
    >
      <CollapsibleTrigger asChild>
        <Button
          ref={trigger}
          type="button"
          variant="ghost"
          className="h-11 w-full justify-start gap-2 px-4 text-base font-semibold md:h-10"
        >
          {open ? <ChevronDown aria-hidden="true" /> : <ChevronRight aria-hidden="true" />}
          {t("addressSpace.kept.trigger", { count: rows.length })}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="flex flex-col gap-3 px-4 pb-4">
        <p className="text-sm text-muted-foreground">{t("addressSpace.kept.caption")}</p>
        <div className="relative overflow-x-auto rounded-xl ring-1 ring-foreground/10">
          <table data-slot="table" className="w-full caption-bottom text-sm">
            <caption className="sr-only">{t("addressSpace.kept.tableLabel")}</caption>
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {[
                  t("addressSpace.kept.columns.network"),
                  ...(swarm ? [t("addressSpace.kept.columns.node")] : []),
                  t("addressSpace.kept.columns.subnet"),
                  t("addressSpace.kept.columns.keptBy"),
                  t("addressSpace.kept.columns.keptSince"),
                ].map((header) => (
                  <TableHead key={header} scope="col" className={headerCell}>
                    {header}
                  </TableHead>
                ))}
                <TableHead scope="col" className={cn(headerCell, "w-36")}>
                  <span className="sr-only">{t("addressSpace.kept.columns.actions")}</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row, index) => {
                const byLabel = row.keptBy === "label";
                const reason = !needs.allowed
                  ? t("addressSpace.orphans.needsOn", {
                      need: needs.reason ?? t("needs.role.admin"),
                      name: envName,
                    })
                  : byLabel
                    ? t("addressSpace.kept.labelReason")
                    : undefined;
                return (
                  <TableRow key={keptKey(row)} className={ROW}>
                    <TableCell className={cn(CELL, "min-w-44")}>
                      <TruncatedText>{row.name}</TruncatedText>
                    </TableCell>
                    {swarm ? (
                      <TableCell className={cn(CELL, "w-36 min-w-36")}>
                        {row.node ? (
                          <TruncatedText>{row.node}</TruncatedText>
                        ) : (
                          <span className="text-muted-foreground">
                            {t("addressSpace.states.missing")}
                          </span>
                        )}
                      </TableCell>
                    ) : null}
                    <TableCell className={cn(CELL, "w-44 min-w-44")}>
                      <SubnetCell row={{ subnets: row.subnets ?? [] }} />
                    </TableCell>
                    <TableCell className={cn(CELL, "min-w-40")}>
                      {byLabel ? t("addressSpace.kept.by.label") : userName(row)}
                    </TableCell>
                    <TableCell className={cn(CELL, "w-36 min-w-36")}>
                      <RelativeTime {...(row.keptAt ? { value: row.keptAt } : {})} />
                    </TableCell>
                    <TableCell className={cn(CELL, "w-36 text-end")}>
                      <NeedsTooltip reason={reason}>
                        <Button
                          ref={(el) => {
                            if (el) buttons.current.set(keptKey(row), el);
                            else buttons.current.delete(keptKey(row));
                          }}
                          type="button"
                          variant="outline"
                          size="sm"
                          className="max-md:h-11"
                          aria-label={t("addressSpace.kept.stopLabel", { name: row.name })}
                          onClick={() => void stop(row, index)}
                        >
                          <PinOff aria-hidden="true" />
                          {t("addressSpace.kept.stop")}
                        </Button>
                      </NeedsTooltip>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </table>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
