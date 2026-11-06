// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import {
  ArrowRight,
  ChevronDown,
  ChevronRight,
  Container,
  Ellipsis,
  Layers,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";
import { useCallback, useMemo, useRef, useState } from "react";
import { Trans } from "react-i18next";

import type { FleetShowValue, FleetUpdateGroup, FleetUpdateItem } from "@/api/fleet";
import { useCheckingUpdates, useCheckUpdates } from "@/api/updates";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { TypeBadge } from "@/components/type-badge";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { FLEET_VIRTUALISE_OVER, VirtualRows } from "@/features/fleet/virtual-rows";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import { SelectCheckbox, TruncatedText } from "@/features/resources/resource-table";
import {
  type GroupSelection,
  type Selection,
  type SelectionState,
  useSelection,
} from "@/features/resources/selection";
import { DigestChip } from "@/features/stacks/digest-chip";
import { IgnoreUpdatesDialog } from "@/features/updates/ignore-updates-dialog";
import { UpdateBadge } from "@/features/updates/update-badge";
import { UpdateConfirmDialog } from "@/features/updates/update-confirm-dialog";
import { UpdateMenuItems } from "@/features/updates/update-menu-items";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A row is 36px (44px under 768px for touch).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";
// The tick, Workload, Image, Change, Status, Checked and the row menu.
const COLUMNS = 7;
const PLATFORMS_SHOWN = 3;

type TableRowKind =
  | { kind: "group"; key: string; group: FleetUpdateGroup }
  | { kind: "item"; key: string; group: FleetUpdateGroup; item: FleetUpdateItem };

// itemKey is what a tick is keyed by: a workload's ID is only unique in its environment and kind.
export const itemKey = (envId: string, item: FleetUpdateItem) => `${envId}|${item.kind}|${item.id}`;

// checkedOf is a checkbox's state from a selection's. Radix draws an indeterminate box with
// aria-checked="mixed", which is how a group that is partly ticked is announced.
function checkedOf(state: GroupSelection): boolean | "indeterminate" {
  return state === "checked" ? true : state === "mixed" ? "indeterminate" : false;
}

interface UpdatesTableProps {
  groups: readonly FleetUpdateGroup[];
  // The Show value the groups were asked for: it words the count in a group's caption.
  show: FleetShowValue;
  // Which rows are ticked. The tab holds it, so the bulk bar can read it; the table works out which
  // rows may be ticked (the ones the server says can be updated now) and keeps only those.
  chosen: SelectionState<string>;
}

// UpdatesTable is the Updates tab's table (D-17): one group of rows for each environment that has
// something to show, a group header with the environment, its type, how many it lists and when it
// was checked, and a row for each workload with its image, the change on offer, its update state, when it
// was last checked successfully and its menu. A group folds away and starts open. Past 100 rows only
// the rows in view are drawn. Every name, image and digest is placed as text.
export function UpdatesTable({ groups, show, chosen }: UpdatesTableProps) {
  const t = useT();
  const collapse = useGroupCollapse();
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);

  // Every row that can be ticked, found or not on screen: a tick is a key in a set, so the 2,000th row
  // is ticked as easily as the first and a virtualised list draws only the rows in view.
  const selectable = useMemo(() => {
    const out = new Set<string>();
    for (const group of groups) {
      for (const item of group.items ?? [])
        if (item.selectable) out.add(itemKey(group.envId, item));
    }
    return out;
  }, [groups]);
  // The order the rows show in, folded groups left out: what a Shift range and Select all follow.
  const orderedKeys = useMemo(() => {
    const out: string[] = [];
    for (const group of groups) {
      if (collapse.collapsed.has(group.envId)) continue;
      for (const item of group.items ?? []) out.push(itemKey(group.envId, item));
    }
    return out;
  }, [groups, collapse.collapsed]);
  const isEligible = useCallback((key: string) => selectable.has(key), [selectable]);
  const selection = useSelection({ orderedKeys, isEligible }, chosen);

  const total = useMemo(() => groups.reduce((n, g) => n + (g.items?.length ?? 0), 0), [groups]);
  const rows = useMemo<TableRowKind[]>(() => {
    const out: TableRowKind[] = [];
    for (const group of groups) {
      out.push({ kind: "group", key: `group|${group.envId}`, group });
      if (collapse.collapsed.has(group.envId)) continue;
      for (const item of group.items ?? []) {
        out.push({ kind: "item", key: itemKey(group.envId, item), group, item });
      }
    }
    return out;
  }, [groups, collapse.collapsed]);

  const virtual = total > FLEET_VIRTUALISE_OVER;
  const renderRow = useCallback(
    (row: TableRowKind) =>
      row.kind === "group" ? (
        <GroupHeader
          key={row.key}
          group={row.group}
          show={show}
          expanded={!collapse.collapsed.has(row.group.envId)}
          onToggle={() => collapse.toggle(row.group.envId)}
          selection={selection}
        />
      ) : (
        <UpdateRow
          key={row.key}
          group={row.group}
          item={row.item}
          selection={selection}
          selectable={selectable.has(row.key)}
        />
      ),
    [show, collapse, selection, selectable],
  );

  return (
    <div
      ref={setScroller}
      className={cn(
        "relative rounded-lg border",
        virtual ? "max-h-[70svh] overflow-auto" : "overflow-x-auto",
      )}
    >
      <table className="w-full caption-bottom text-sm">
        <caption className="sr-only">{t("fleet.updates.caption")}</caption>
        <TableHeader className={virtual ? "sticky top-0 z-10 bg-background" : undefined}>
          <TableRow className="h-12 hover:bg-transparent">
            <TableHead scope="col" className="w-11 p-0 ps-3">
              <span className="sr-only">{t("fleet.updates.columns.select")}</span>
              <SelectCheckbox
                checked={checkedOf(selection.allState)}
                label={t("fleet.updates.select.all")}
                disabled={selection.eligibleCount === 0}
                onToggle={() => selection.selectAll()}
              />
            </TableHead>
            <TableHead scope="col" className="min-w-56">
              {t("fleet.updates.columns.workload")}
            </TableHead>
            <TableHead scope="col" className="min-w-56">
              {t("fleet.updates.columns.image")}
            </TableHead>
            <TableHead scope="col" className="min-w-64">
              {t("fleet.updates.columns.change")}
            </TableHead>
            <TableHead scope="col" className="w-48">
              {t("fleet.updates.columns.status")}
            </TableHead>
            <TableHead scope="col" className="w-32">
              {t("fleet.updates.columns.checked")}
            </TableHead>
            <TableHead scope="col" className="w-11 p-0">
              <span className="sr-only">{t("fleet.updates.columns.actions")}</span>
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
              renderRow={renderRow}
            />
          ) : (
            rows.map(renderRow)
          )}
        </TableBody>
      </table>
    </div>
  );
}

interface GroupHeaderProps {
  group: FleetUpdateGroup;
  show: FleetShowValue;
  expanded: boolean;
  onToggle: () => void;
  selection: Selection<string>;
}

// GroupHeader is the band that opens an environment's rows: a fold button, the environment (a link),
// its type, "{n} updates, checked {time}", the reason when its checks failed and a button that checks
// that environment again.
function GroupHeader({ group, show, expanded, onToggle, selection }: GroupHeaderProps) {
  const t = useT();
  const keys = useMemo(
    () => (group.items ?? []).map((item) => itemKey(group.envId, item)),
    [group],
  );
  // A group with nothing that can be updated has no tick to offer.
  const ticks = (group.items ?? []).some((item) => item.selectable);
  const Chevron = expanded ? ChevronDown : ChevronRight;
  const count = group.items?.length ?? 0;
  const what = t(show === "updates" ? "fleet.updates.group.updates" : "fleet.updates.group.items", {
    count,
  });
  return (
    <TableRow data-slot="group-header-row" className="h-9 bg-muted hover:bg-muted max-md:h-11">
      <TableHead
        scope="rowgroup"
        colSpan={COLUMNS}
        className="h-9 px-1 py-0 text-start whitespace-normal max-md:h-11"
      >
        <div className="flex min-w-0 items-center gap-2">
          {ticks ? (
            <span className="ps-2">
              <SelectCheckbox
                checked={checkedOf(selection.groupState(keys))}
                label={t("fleet.updates.select.group", { environment: group.envName })}
                onToggle={() => selection.selectGroup(keys)}
              />
            </span>
          ) : null}
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-8 max-md:size-11"
            aria-expanded={expanded}
            aria-label={t(
              expanded ? "containers.list.group.collapse" : "containers.list.group.expand",
              { name: group.envName },
            )}
            onClick={onToggle}
          >
            <Chevron aria-hidden="true" />
          </Button>
          <Link
            to="/env/$envId"
            params={{ envId: group.envId }}
            title={group.envName}
            className="min-w-0 truncate rounded-sm text-sm font-semibold hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {group.envName}
          </Link>
          <TypeBadge envType={group.type} />
          <span className="shrink-0 text-xs font-normal text-muted-foreground tabular-nums">
            {group.lastCheckedAt ? (
              <Trans
                i18nKey="fleet.updates.group.checked"
                ns="fleet"
                values={{ what }}
                components={{ time: <RelativeTime value={group.lastCheckedAt} /> }}
              />
            ) : (
              t("fleet.updates.group.neverChecked", { what })
            )}
          </span>
          {group.checkFailed ? (
            <span className="inline-flex min-w-0 items-center gap-1 text-xs font-normal text-status-warning-text">
              <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
              <span className="truncate">
                {t("fleet.updates.group.failedLabel", {
                  reason: t(`fleet.updates.group.failed.${group.checkFailed}`),
                })}
              </span>
            </span>
          ) : null}
          <GroupCheckButton envId={group.envId} envName={group.envName} />
        </div>
      </TableHead>
    </TableRow>
  );
}

// GroupCheckButton checks one environment again. It needs updates.check on that environment: below
// it the button is unavailable with the reason, and the server decides either way.
function GroupCheckButton({ envId, envName }: { envId: string; envName: string }) {
  const t = useT();
  const needs = useNeeds("updates.check", envId);
  const check = useCheckUpdates(envId);
  const checking = useCheckingUpdates(envId);
  const reason = needs.reason
    ? t("updates.check.needs", { needs: needs.reason, environment: envName })
    : undefined;
  return (
    <NeedsTooltip reason={reason}>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="ms-auto size-8 shrink-0 max-md:size-11"
        aria-label={t("fleet.updates.group.checkNow", { environment: envName })}
        aria-busy={checking || undefined}
        disabled={!needs.allowed && !needs.reason}
        onClick={() => {
          if (!needs.allowed || checking) return;
          check.mutate();
        }}
      >
        {checking ? <Spinner aria-hidden="true" /> : <RefreshCw aria-hidden="true" />}
      </Button>
    </NeedsTooltip>
  );
}

interface UpdateRowProps {
  group: FleetUpdateGroup;
  item: FleetUpdateItem;
  selection: Selection<string>;
  selectable: boolean;
}

function UpdateRow({ group, item, selection, selectable }: UpdateRowProps) {
  const t = useT();
  const key = itemKey(group.envId, item);
  const className =
    "block max-w-60 truncate rounded-sm font-mono text-xs font-semibold hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none";
  const KindIcon = item.kind === "service" ? Layers : Container;
  return (
    <TableRow className={ROW} data-state={selection.isSelected(key) ? "selected" : undefined}>
      <TableCell className={cn(CELL, "w-11 ps-3")}>
        {selectable ? (
          <SelectCheckbox
            checked={selection.isSelected(key)}
            label={t("fleet.updates.select.row", { name: item.name })}
            onToggle={(shift) => selection.toggle(key, { shift })}
          />
        ) : null}
      </TableCell>
      <TableCell className={cn(CELL, "min-w-56")}>
        <div className="flex min-w-0 items-center gap-2">
          <KindIcon
            role="img"
            aria-label={t(`fleet.updates.kind.${item.kind}`)}
            className="size-4 shrink-0 text-muted-foreground"
          />
          <div className="flex min-w-0 flex-col">
            {item.kind === "service" ? (
              <Link
                to="/env/$envId/services/$serviceId"
                params={{ envId: group.envId, serviceId: item.id }}
                title={item.name}
                className={className}
              >
                {item.name}
              </Link>
            ) : (
              <Link
                to="/env/$envId/containers/$containerId"
                params={{ envId: group.envId, containerId: item.id }}
                title={item.name}
                className={className}
              >
                {item.name}
              </Link>
            )}
            {item.stack ? (
              <span className="max-w-60 truncate text-xs text-muted-foreground" title={item.stack}>
                {t("fleet.updates.in", { stack: item.stack })}
              </span>
            ) : null}
          </div>
        </div>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-56")}>
        <TruncatedText mono className="max-w-64">
          {item.image}
        </TruncatedText>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-64")}>
        <Change item={item} />
      </TableCell>
      <TableCell className={cn(CELL, "w-48 whitespace-nowrap")}>
        <UpdateBadge
          info={item.update}
          name={item.name}
          envId={group.envId}
          stackId={item.tierplanStackId || undefined}
        />
      </TableCell>
      <TableCell className={cn(CELL, "w-32 whitespace-nowrap")}>
        {item.update.successAt ? (
          <RelativeTime value={item.update.successAt} />
        ) : (
          <span className="text-muted-foreground">{t("fleet.updates.never")}</span>
        )}
      </TableCell>
      <TableCell className={cn(CELL, "w-11 text-end")}>
        <UpdateRowActions group={group} item={item} />
      </TableCell>
    </TableRow>
  );
}

// Change is the old digest, an arrow and the new one, with the platforms that changed when only some
// did. A workload with no new version on record shows a dash.
function Change({ item }: { item: FleetUpdateItem }) {
  const t = useT();
  if (!item.toDigest) {
    return <span className="text-muted-foreground">{t("fleet.updates.noChange")}</span>;
  }
  const platforms = item.changedPlatforms ?? [];
  const shown = platforms.slice(0, PLATFORMS_SHOWN);
  const rest = platforms.slice(PLATFORMS_SHOWN);
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {item.fromDigest ? (
        <DigestChip digest={item.fromDigest} variant="pinned" />
      ) : (
        <span className="text-muted-foreground">{t("fleet.updates.noChange")}</span>
      )}
      <ArrowRight aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />
      <DigestChip digest={item.toDigest} variant="new" />
      {shown.map((p) => (
        <Badge key={p} variant="outline">
          {p}
        </Badge>
      ))}
      {rest.length > 0 ? (
        <Badge variant="outline" title={rest.join(", ")}>
          {t("fleet.updates.morePlatforms", { count: rest.length })}
        </Badge>
      ) : null}
    </div>
  );
}

// UpdateRowActions is the row menu "Actions for {name}": the update items every list offers (Update…,
// Skip this version, Ignore updates…, Check now), with the dialogs they open. The dialogs belong to the
// row, since a menu's content is gone as soon as an item is chosen, and focus goes back to the
// menu's button when they close.
function UpdateRowActions({ group, item }: { group: FleetUpdateGroup; item: FleetUpdateItem }) {
  const t = useT();
  const [ignoring, setIgnoring] = useState(false);
  const [updating, setUpdating] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  // Set when an item was chosen, so the menu does not hand focus back to its button before the dialog
  // has opened.
  const chose = useRef(false);
  const workload = { kind: item.kind, name: item.name } as const;
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            ref={button}
            type="button"
            variant="ghost"
            size="icon"
            className="size-8 max-md:size-11"
            aria-label={t("actions.menu.label", { name: item.name })}
          >
            <Ellipsis aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          onCloseAutoFocus={(event) => {
            if (!chose.current) return;
            chose.current = false;
            event.preventDefault();
          }}
        >
          <UpdateMenuItems
            envId={group.envId}
            workload={workload}
            update={item.update}
            onChosen={() => {
              chose.current = true;
            }}
            onIgnore={() => setIgnoring(true)}
            onUpdate={() => setUpdating(true)}
            project={item.kind === "container" ? item.stack || undefined : undefined}
          />
        </DropdownMenuContent>
      </DropdownMenu>
      {ignoring ? (
        <IgnoreUpdatesDialog
          envId={group.envId}
          workload={workload}
          update={item.update}
          open
          onOpenChange={(isOpen) => {
            if (!isOpen) setIgnoring(false);
          }}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
      {updating ? (
        <UpdateConfirmDialog
          envId={group.envId}
          envName={group.envName}
          workload={workload}
          managedStack={item.tierplanStackId ? item.stack : undefined}
          onClose={() => setUpdating(false)}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
    </>
  );
}
