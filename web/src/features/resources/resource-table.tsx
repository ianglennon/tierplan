// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import type { ReactNode } from "react";
import { Fragment, useId } from "react";

import { CLUSTER_NODE } from "@/api/resources";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
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
import { type GroupCollapse, GroupHeaderRow } from "@/features/resources/group-header-row";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export type ResourceKind = "containers" | "images" | "volumes" | "networks" | "registries";

// One column of a list. The cell renders from the row; the header is the
// translated label.
export interface Column<T> {
  id: string;
  // Text, or a node for a column whose label is only for assistive technology.
  header: ReactNode;
  cell: (row: T) => ReactNode;
  className?: string;
  // Set on a column the list can sort by: its header becomes a button.
  sortKey?: string;
}

// The list's sort, for the columns that carry a sortKey. Choosing the column
// that is sorted already flips the direction; the owner decides what a choice
// means (it lives in the URL).
export interface TableSort {
  key: string;
  dir: "asc" | "desc";
  onSort: (key: string) => void;
}

// One group of rows under a header band (UI-SPEC "Groups"). A group without a
// header is a plain run of rows, for a list that is not grouped.
export interface TableGroup<T> {
  key: string;
  rows: T[];
  header?: {
    name: string;
    summary?: ReactNode;
    badge?: ReactNode;
    checkbox?: ReactNode;
  };
}

// How a row is drawn while the list changes under it (D-05, D-50).
export interface RowState {
  // Its values changed a moment ago: muted for 1.5 s.
  changed?: boolean;
  // It is new: it slides in.
  added?: boolean;
  // It is gone but still listed: dimmed.
  removed?: boolean;
  // ...and on its way out: it fades.
  leaving?: boolean;
}

// The leading checkbox column of a list that can be acted on in bulk (D-05).
// The owner draws the checkboxes (SelectCheckbox) so it can decide which rows
// can be chosen; the table gives them a 44px column of their own and clears the
// selection on Escape.
export interface TableSelect<T> {
  // The header checkbox: "Select all containers".
  header: ReactNode;
  cell: (row: T) => ReactNode;
  // Called on Escape from inside the table, with no menu or dialog taking it.
  onEscape?: () => void;
}

export interface TableGroups<T> {
  items: TableGroup<T>[];
  collapse: GroupCollapse;
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
  // Rows under header bands. Without it the rows of query.data.items are shown
  // in the order the server sent them.
  groups?: TableGroups<T>;
  // The toolbar between the banner and the table. When it is given the table
  // does not draw its own Node select; the toolbar places NodeSelect itself.
  toolbar?: ReactNode;
  // Replaces the count in the header, and adds to it.
  caption?: string;
  captionExtra?: ReactNode;
  // Replaces the empty state, for a list with filters.
  empty?: ReactNode;
  // Replaces the sentence under the table when the server capped the list.
  capNote?: string;
  sort?: TableSort;
  // How each row is drawn, by its row key. A row with no entry is plain.
  rowStates?: ReadonlyMap<string, RowState>;
  // The checkbox column, and the bar that replaces the caption while rows are
  // selected.
  select?: TableSelect<T>;
  bulkBar?: ReactNode;
  // The i18n key whose title and error words the table uses, when it is not one of
  // the dashboard lists (the Registries page keeps its words in its own namespace).
  i18nKey?: string;
  // Replaces the sentence under the error heading.
  errorBody?: string;
  // The primary action of the page, at the right end of the header. Drawn while the
  // list loads, so the header does not move when it arrives.
  actions?: ReactNode;
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

// Body rows. The row component animates colour on hover with no regard to the
// reader's motion setting, so it is turned off here and back on only where
// motion is welcome; the changed-row highlight is a background and shows either
// way (UI-SPEC "Row change highlight").
const BODY_ROW = "transition-none motion-safe:transition-colors motion-safe:duration-150";

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

export interface NodeSelectProps extends NodeFilter {
  // The nodes that reported, from the list answer.
  nodes: readonly string[];
}

// NodeSelect is the Node filter above a swarm list: "All nodes" then each
// hostname. The node in the URL stays selectable even when the server no longer
// lists it (it stopped reporting), so the select never shows a blank.
export function NodeSelect({ value, onChange, nodes }: NodeSelectProps) {
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

interface SelectCheckboxProps {
  checked: boolean | "indeterminate";
  // "Select web-1", "Select all containers", "Select shop".
  label: string;
  // Called with whether Shift was held, for a range.
  onToggle: (shift: boolean) => void;
  // Off for good: the row cannot be selected at all. Used for a row that is
  // gone or waiting on an action.
  disabled?: boolean;
  // Off with a reason: the checkbox stays in the tab order, aria-disabled, and
  // says why (Bayplan's own containers).
  reason?: string;
}

// SelectCheckbox is a row, group or header checkbox. A click, Space or
// Shift+click toggles it; the 44px hit area comes from the column, not the box.
export function SelectCheckbox({
  checked,
  label,
  onToggle,
  disabled,
  reason,
}: SelectCheckboxProps) {
  const box = (
    <Checkbox
      checked={checked}
      aria-label={label}
      disabled={disabled}
      className="max-md:size-5"
      onClick={(e) => onToggle(e.shiftKey)}
    />
  );
  return <NeedsTooltip reason={reason}>{box}</NeedsTooltip>;
}

// SortHeader is a column header that sorts: a button in the th, with aria-sort
// on the th, as the environments overview does.
function SortHeader({ label, active, dir, onClick }: SortHeaderProps) {
  const Icon = !active ? ChevronsUpDown : dir === "asc" ? ArrowUp : ArrowDown;
  return (
    <button
      type="button"
      className="-mx-1 inline-flex items-center gap-1 rounded px-1 text-xs font-semibold outline-none hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 max-md:h-11"
      onClick={onClick}
    >
      {label}
      <Icon aria-hidden="true" className="size-3" />
    </button>
  );
}

interface SortHeaderProps {
  label: ReactNode;
  active: boolean;
  dir: "asc" | "desc";
  onClick: () => void;
}

// ResourceTable is the list behind a dashboard tile and the container list: a
// title with a plural-aware count, a dense table with a sticky header, and every
// state of the UI-SPEC: loading skeletons, empty, an inline error with Retry,
// and the 500-row cap footer. Rows keep the server's order unless the owner hands
// it groups, which it renders under header bands, and a sort, which it only
// reports: the owner orders the rows.
export function ResourceTable<T>({
  resource,
  columns,
  query,
  envName,
  getRowKey,
  nodeColumn,
  nodeFilter,
  banner,
  groups,
  toolbar,
  caption: captionOverride,
  captionExtra,
  empty: emptyState,
  capNote,
  sort,
  rowStates,
  select,
  bulkBar,
  i18nKey,
  errorBody,
  actions,
}: ResourceTableProps<T>) {
  const t = useT();
  const groupsId = useId();
  const key = i18nKey ?? `dashboard.lists.${resource}`;
  const data = query.data;
  const rows = groups ? groups.items.flatMap((g) => g.rows) : (data?.items ?? []);
  const failed = query.isError && !data;
  const loading = query.isPending;
  const empty = !loading && !failed && rows.length === 0;

  // The Node column goes right after the first one (Name).
  const withNode = nodeColumn ? [...columns.slice(0, 1), nodeColumn, ...columns.slice(1)] : columns;
  const shownColumns: Column<T>[] = select
    ? [
        {
          id: "select",
          header: select.header,
          cell: select.cell,
          className: "w-11 min-w-11 px-0 text-center",
        },
        ...withNode,
      ]
    : withNode;
  const nodes = data?.nodes ?? [];
  // The swarm-wide Cluster entry is in the filter but is not a node.
  const nodeCount = nodes.filter((n) => n !== CLUSTER_NODE).length;
  const count = captionOverride ?? t(`${key}.count`, { count: data?.total ?? 0 });
  let caption = count;
  if (nodeColumn && nodeFilter?.value !== undefined) {
    caption = `${count} ${t("aggregate.caption.on", { node: nodeFilter.value })}`;
  } else if (nodeColumn && nodeCount > 0) {
    caption = `${count} ${t("aggregate.caption.across", { count: nodeCount })}`;
  }
  // "Nothing to show on {name} right now." names the node while one is chosen.
  const emptyName = nodeFilter?.value ?? envName;

  function renderRow(row: T) {
    const rowKey = getRowKey(row);
    const state = rowStates?.get(rowKey);
    return (
      <TableRow
        key={rowKey}
        data-changed={state?.changed || undefined}
        data-added={state?.added || undefined}
        data-removed={state?.removed || undefined}
        data-leaving={state?.leaving || undefined}
        className={cn(
          ROW,
          BODY_ROW,
          state?.changed && "bg-muted",
          state?.added &&
            "motion-safe:animate-in motion-safe:fade-in motion-safe:slide-in-from-top-1 motion-safe:duration-150",
          state?.removed && "opacity-60",
          state?.leaving &&
            "motion-safe:animate-out motion-safe:fade-out motion-safe:fill-mode-forwards motion-safe:duration-150",
        )}
      >
        {shownColumns.map((c) => (
          <TableCell key={c.id} className={cn(CELL, c.className)}>
            {c.cell(row)}
          </TableCell>
        ))}
      </TableRow>
    );
  }

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-4">
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-2xl font-semibold">{t(`${key}.title`)}</h1>
        {data && !bulkBar ? (
          <span className="text-sm text-muted-foreground tabular-nums">{caption}</span>
        ) : null}
        {data ? captionExtra : null}
        {actions}
      </header>

      {bulkBar}

      {banner}

      {toolbar ?? (nodeFilter ? <NodeSelect {...nodeFilter} nodes={nodes} /> : null)}

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t(`${key}.error`)}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{errorBody ?? t("dashboard.lists.errorBody")}</span>
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

      {empty
        ? (emptyState ?? (
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
          ))
        : null}

      {!failed && !empty ? (
        // The wrapper scrolls sideways on a phone (D-64) and, past the page
        // height, down, so the sticky header has something to stick to.
        // It is also positioned (relative). Cells hold visually hidden reasons
        // (NeedsTooltip, the credential table's Actions cell), and sr-only is
        // position: absolute: without a positioned wrapper their containing block is
        // the page, so they escape the scroll clip and widen the document. On a phone
        // that grows the layout viewport, and every fixed sheet with it (G-05-7a,
        // G-05-7b).
        // biome-ignore lint/a11y/noStaticElementInteractions: it only hears Escape from the controls inside the table; the wrapper itself is not interactive
        <div
          className="relative max-h-[calc(100dvh-14rem)] overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10"
          onKeyDown={(e) => {
            // Escape from a menu or dialog is theirs: it arrives here already
            // handled, or from a portal outside this table.
            if (e.key !== "Escape" || e.defaultPrevented || !select?.onEscape) return;
            if (!e.currentTarget.contains(e.target as Node)) return;
            select.onEscape();
          }}
        >
          <table
            data-slot="table"
            aria-busy={loading || undefined}
            aria-label={t(`${key}.title`)}
            className="w-full caption-bottom text-sm"
          >
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {shownColumns.map((c) => {
                  const sortable = sort && c.sortKey !== undefined;
                  const active = sortable && sort.key === c.sortKey;
                  return (
                    <TableHead
                      key={c.id}
                      aria-sort={
                        sortable
                          ? active
                            ? sort.dir === "asc"
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
                          active={Boolean(active)}
                          dir={sort.dir}
                          onClick={() => sort.onSort(c.sortKey as string)}
                        />
                      ) : (
                        c.header
                      )}
                    </TableHead>
                  );
                })}
              </TableRow>
            </TableHeader>
            {loading ? (
              <TableBody>
                {Array.from({ length: SKELETON_ROWS }, (_, i) => (
                  // Placeholder rows have no identity of their own.
                  // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                  <TableRow key={i} className={ROW}>
                    {shownColumns.map((c) => (
                      <TableCell key={c.id} className={cn(CELL, c.className)}>
                        <Skeleton className="h-4 w-24" aria-hidden="true" />
                      </TableCell>
                    ))}
                  </TableRow>
                ))}
              </TableBody>
            ) : groups ? (
              groups.items.map((g, i) => {
                const expanded = !g.header || !groups.collapse.collapsed.has(g.key);
                // An index, not the key: a key may hold spaces, and an id list
                // is separated by them.
                const rowsId = `${groupsId}-g${i}`;
                return (
                  <Fragment key={g.key}>
                    {g.header ? (
                      <TableBody>
                        <GroupHeaderRow
                          colSpan={shownColumns.length}
                          name={g.header.name}
                          summary={g.header.summary}
                          badge={g.header.badge}
                          checkbox={g.header.checkbox}
                          expanded={expanded}
                          controls={rowsId}
                          onToggle={() => groups.collapse.toggle(g.key)}
                        />
                      </TableBody>
                    ) : null}
                    <TableBody id={rowsId} hidden={!expanded}>
                      {expanded ? g.rows.map(renderRow) : null}
                    </TableBody>
                  </Fragment>
                );
              })
            ) : (
              <TableBody>{rows.map(renderRow)}</TableBody>
            )}
          </table>
        </div>
      ) : null}

      {data?.truncated ? (
        <p className="text-sm text-muted-foreground">
          {capNote ?? t("dashboard.lists.cap", { total: data.total })}
        </p>
      ) : null}
    </div>
  );
}
