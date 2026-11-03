// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronDown, ChevronRight, PinOff } from "lucide-react";
import { useRef, useState } from "react";

import {
  type JanitorKept,
  JanitorRequestError,
  useJanitorKeeps,
  useUnkeepJanitorItem,
} from "@/api/janitor";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { TruncatedText } from "@/features/resources/resource-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// A keep follows the object: its kind, the node it is on and its engine ID (a network's name).
export const keepKey = (k: Pick<JanitorKept, "kind" | "nodeId" | "ref">) =>
  `${k.kind}|${k.nodeId}|${k.ref}`;

interface KeptItemsListProps {
  envId: string;
  envName: string;
  // The name of the excluded stack that owns an item, when one does. The kept answer does not say, so
  // the page does not pass it today; an item with a stack is shown as held by the stack, not by a
  // person, and cannot be un-kept here.
  heldByStack?: (item: JanitorKept) => string | undefined;
}

// KeptItemsList is "Kept items (n)" (D-12, anchor #kept): everything the janitor never removes in the
// environment because a person chose Keep on it or the object carries the tierplan.keep label. It is
// closed until opened and left out when nothing is kept. A keep a person made can be taken back; one
// made by the label can only be removed from the object.
export function KeptItemsList({ envId, envName, heldByStack }: KeptItemsListProps) {
  const t = useT();
  const needs = useNeeds("janitor.run", envId);
  const keeps = useJanitorKeeps(envId);
  const unkeep = useUnkeepJanitorItem(envId);
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const rows = keeps.data ?? [];
  if (rows.length === 0) return null;

  async function stop(row: JanitorKept, index: number) {
    if (unkeep.isPending) return;
    // The button that was pressed leaves with its row, so focus moves first: to the next row's
    // button, or to the trigger.
    const next = rows[index + 1];
    const target = (next ? buttons.current.get(keepKey(next)) : undefined) ?? trigger.current;
    target?.focus();
    try {
      await unkeep.mutateAsync(row);
      toastSuccess(t("janitorKeep.list.done", { name: row.name }));
    } catch (e) {
      const err = e instanceof JanitorRequestError ? e : new JanitorRequestError("DELETE", 0);
      if (err.type === "keep_not_found") {
        toastInfo(t("janitorKeep.list.notKept", { name: row.name }));
      } else if (err.type === "kept_by_label") {
        toastError(t("janitorKeep.list.labelReason"));
      } else {
        toastError(t("janitorKeep.list.failed", { name: row.name }));
      }
    }
  }

  const headerCell = "h-9 bg-muted px-3 text-xs font-semibold text-muted-foreground max-md:h-11";
  const permission = needs.allowed
    ? undefined
    : t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName });
  // Why Stop keeping is unavailable for one row, when it is.
  const reasonFor = (row: JanitorKept): string | undefined => {
    if (permission) return permission;
    const stack = heldByStack?.(row);
    if (stack) return t("janitorKeep.list.stackReason", { stack });
    return row.keptBy === "label" ? t("janitorKeep.list.labelReason") : undefined;
  };

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
          {t("janitorKeep.list.trigger", { count: rows.length })}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="flex flex-col gap-3 px-4 pb-4">
        <p className="text-sm text-muted-foreground">{t("janitorKeep.list.caption")}</p>
        <div className="relative overflow-x-auto rounded-xl ring-1 ring-foreground/10">
          <table data-slot="table" className="w-full caption-bottom text-sm">
            <caption className="sr-only">{t("janitorKeep.list.tableLabel")}</caption>
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {(["type", "name", "node", "keptBy", "keptSince"] as const).map((column) => (
                  <TableHead key={column} scope="col" className={headerCell}>
                    {t(`janitorKeep.list.columns.${column}`)}
                  </TableHead>
                ))}
                <TableHead scope="col" className={cn(headerCell, "w-40")}>
                  <span className="sr-only">{t("janitorKeep.list.columns.actions")}</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((row, index) => (
                <TableRow key={keepKey(row)} className={ROW}>
                  <TableCell className={cn(CELL, "w-28")}>
                    {t(`janitor.type.${row.kind}`)}
                  </TableCell>
                  <TableCell className={cn(CELL, "min-w-48")}>
                    <span className="flex min-w-0 flex-col">
                      <TruncatedText mono>{row.name}</TruncatedText>
                      {row.present ? null : (
                        <span className="text-xs text-muted-foreground">
                          {t("janitorKeep.list.absent")}
                        </span>
                      )}
                    </span>
                  </TableCell>
                  <TableCell className={cn(CELL, "w-36 min-w-36")}>
                    {row.node ? (
                      <TruncatedText mono>{row.node}</TruncatedText>
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell className={cn(CELL, "min-w-40")}>
                    {row.keptBy === "label"
                      ? t("janitorKeep.list.by.label")
                      : row.keptByName
                        ? t("janitorKeep.list.by.user", { user: row.keptByName })
                        : t("janitorKeep.list.by.unknown")}
                  </TableCell>
                  <TableCell className={cn(CELL, "w-36 min-w-36")}>
                    {row.keptAt ? (
                      <RelativeTime value={row.keptAt} />
                    ) : (
                      <span className="text-muted-foreground">—</span>
                    )}
                  </TableCell>
                  <TableCell className={cn(CELL, "w-40 text-end")}>
                    <NeedsTooltip reason={reasonFor(row)}>
                      <Button
                        ref={(el) => {
                          if (el) buttons.current.set(keepKey(row), el);
                          else buttons.current.delete(keepKey(row));
                        }}
                        type="button"
                        variant="outline"
                        size="sm"
                        className="max-md:h-11"
                        aria-label={t("janitorKeep.list.stopLabel", { name: row.name })}
                        onClick={() => void stop(row, index)}
                      >
                        <PinOff aria-hidden="true" />
                        {t("janitorKeep.list.stop")}
                      </Button>
                    </NeedsTooltip>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </table>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
