// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useVirtualizer } from "@tanstack/react-virtual";
import {
  ArrowDown,
  ArrowUp,
  ChevronsUpDown,
  CircleCheck,
  CircleX,
  Container,
  Info,
  Layers,
  Network,
  ShieldCheck,
  TriangleAlert,
} from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";

import type { JanitorRunItem } from "@/api/janitor";
import { Button } from "@/components/ui/button";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { MAX_QUERY } from "@/features/containers/list-search";
import { reasonText, type Translate } from "@/features/janitor/preview-summary";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";
import { z } from "@/lib/zod-config";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// A list with more rows than this draws only the rows in view (UI-SPEC "Run items table"). The
// footer and the filters always cover every row, drawn or not.
export const VIRTUALISE_OVER = 100;
const ROW_PX = 36;
const OVERSCAN = 10;

// The choices of the Show filter, in the order they are drawn.
export const SHOW_VALUES = ["all", "removed", "kept", "problems"] as const;
export type Show = (typeof SHOW_VALUES)[number];

// The run page's search (UI-SPEC "Screens"): which items to list, a search by name, and the item
// whose crash evidence is open. A value that does not fit is dropped, so a stale or hand-edited link
// opens the default and not an error.
export const runSearchSchema = z.object({
  show: z.enum(SHOW_VALUES).optional().catch(undefined),
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  item: z.coerce.number().int().min(1).optional().catch(undefined),
});

export type RunSearch = z.output<typeof runSearchSchema>;

// A change to the search: a key set to undefined is taken out of the address.
export type RunSearchPatch = { [K in keyof RunSearch]?: RunSearch[K] | undefined };

// Kept is an item the run left alone because something uses it or it is no longer stale. It is a
// safety success and never a problem.
const KEPT: ReadonlySet<JanitorRunItem["outcome"]> = new Set([
  "refused_in_use",
  "skipped_protected",
]);
const PROBLEMS: ReadonlySet<JanitorRunItem["outcome"]> = new Set(["error", "skipped_node_offline"]);

// matchesShow says whether an item belongs to a choice of the Show filter. Problems are the items
// that failed and the ones whose node was not reporting.
export function matchesShow(outcome: JanitorRunItem["outcome"], show: Show): boolean {
  switch (show) {
    case "removed":
      return outcome === "removed";
    case "kept":
      return KEPT.has(outcome);
    case "problems":
      return PROBLEMS.has(outcome);
    default:
      return true;
  }
}

// itemReason is why an item ended as it did. A code names a refusal or a failure of the attempt, and is
// worded with the item's own name and node; with none, the reason the run selected it for is shown.
// The run item's name is whatever Docker called it, and is placed as text.
export function itemReason(item: JanitorRunItem, t: Translate): string {
  if (item.code) {
    const key = `janitorRun.itemReason.${item.code}`;
    const words = t(key, { name: item.name, node: item.node });
    if (words !== key) return words;
  }
  return reasonText(item.reason, t);
}

function TypeIcon({ type }: { type: JanitorRunItem["type"] }) {
  const Icon = type === "container" ? Container : type === "image" ? Layers : Network;
  return <Icon aria-hidden="true" className="size-4 text-muted-foreground" />;
}

function OutcomeIcon({ outcome }: { outcome: JanitorRunItem["outcome"] }) {
  switch (outcome) {
    case "removed":
      return <CircleCheck aria-hidden="true" className="size-4 text-status-online-text" />;
    case "refused_in_use":
      return <Info aria-hidden="true" className="size-4 text-muted-foreground" />;
    case "skipped_protected":
      return <ShieldCheck aria-hidden="true" className="size-4 text-muted-foreground" />;
    case "skipped_node_offline":
      return <TriangleAlert aria-hidden="true" className="size-4 text-status-warning-text" />;
    default:
      return <CircleX aria-hidden="true" className="size-4 text-status-offline-text" />;
  }
}

// OutcomeCell is an icon and the word for how the attempt ended. The icon is decorative: the word
// carries the meaning.
export function OutcomeCell({ outcome }: { outcome: JanitorRunItem["outcome"] }) {
  const t = useT();
  return (
    <span className="inline-flex items-center gap-1.5">
      <OutcomeIcon outcome={outcome} />
      {t(`janitorRun.outcome.${outcome}`)}
    </span>
  );
}

type SortKey = "type" | "name" | "node" | "reason" | "outcome";
interface Sort {
  key: SortKey;
  dir: "asc" | "desc";
}

export interface RunItemsTableProps {
  envId: string;
  runId: string;
  items: readonly JanitorRunItem[];
  show: Show;
  q?: string;
  // Clears the filters, for the line that says nothing matches.
  onClearFilters?: () => void;
}

// RunItemsTable lists the items a run attempted, narrowed by the Show filter and the search by name
// and sortable by its headers. It is a native table; over 100 rows only the rows in view are drawn
// between two spacer rows, and the count under it is of every row that matches.
export function RunItemsTable({ items, show, q, onClearFilters }: RunItemsTableProps) {
  const t = useT();
  const [sort, setSort] = useState<Sort | undefined>();
  // The scrolling element is state and not a ref: the rows ask for it before React has set a ref on
  // their parent, and a ref would still be empty then.
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const showNode = items.some((i) => i.node !== "");

  const rows = useMemo(() => {
    const needle = q?.trim().toLowerCase();
    const wanted = items.filter(
      (i) => matchesShow(i.outcome, show) && (!needle || i.name.toLowerCase().includes(needle)),
    );
    if (!sort) return wanted;
    const word = (i: JanitorRunItem): string => {
      switch (sort.key) {
        case "type":
          return t(`janitor.type.${i.type}`);
        case "name":
          return i.name;
        case "node":
          return i.node;
        case "reason":
          return itemReason(i, t);
        default:
          return t(`janitorRun.outcome.${i.outcome}`);
      }
    };
    const sign = sort.dir === "asc" ? 1 : -1;
    return [...wanted].sort(
      (a, b) =>
        sign * word(a).localeCompare(word(b), undefined, { numeric: true }) || a.seq - b.seq,
    );
  }, [items, show, q, sort, t]);

  if (rows.length === 0) {
    return (
      <div className="flex flex-col items-start gap-2 rounded-lg border p-4">
        <p className="text-sm font-semibold">{t("janitorRun.noMatch.title")}</p>
        {onClearFilters ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={onClearFilters}
          >
            {t("janitorRun.noMatch.clear")}
          </Button>
        ) : null}
      </div>
    );
  }

  const virtual = rows.length > VIRTUALISE_OVER;
  const columns = showNode ? 5 : 4;
  const header = (key: SortKey, className: string) => (
    <SortHeader
      sort={sort}
      column={key}
      className={className}
      onSort={() =>
        setSort((s) =>
          s?.key !== key ? { key, dir: "asc" } : s.dir === "asc" ? { key, dir: "desc" } : undefined,
        )
      }
    />
  );
  const renderRow = (item: JanitorRunItem) => (
    <TableRow key={item.seq} className={ROW}>
      <TableCell className={cn(CELL, "w-32")}>
        <span className="inline-flex items-center gap-1.5">
          <TypeIcon type={item.type} />
          {t(`janitor.type.${item.type}`)}
        </span>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-48")}>
        <TruncatedText mono>{item.name}</TruncatedText>
      </TableCell>
      {showNode ? (
        <TableCell className={cn(CELL, "w-36")}>
          <TruncatedText mono>{item.node || "—"}</TruncatedText>
        </TableCell>
      ) : null}
      <TableCell className={cn(CELL, "min-w-64 whitespace-normal")}>
        {itemReason(item, t) || "—"}
      </TableCell>
      <TableCell className={cn(CELL, "w-48")}>
        <OutcomeCell outcome={item.outcome} />
      </TableCell>
    </TableRow>
  );

  return (
    <>
      <div
        ref={setScroller}
        className={cn(
          "rounded-lg border",
          virtual ? "max-h-[70svh] overflow-auto" : "overflow-x-auto",
        )}
      >
        <table className="w-full caption-bottom text-sm">
          <caption className="sr-only">{t("janitorRun.caption")}</caption>
          <TableHeader className={virtual ? "sticky top-0 z-10 bg-background" : undefined}>
            <TableRow>
              {header("type", "w-32")}
              {header("name", "min-w-48")}
              {showNode ? header("node", "w-36") : null}
              {header("reason", "min-w-64")}
              {header("outcome", "w-48")}
            </TableRow>
          </TableHeader>
          <TableBody>
            {virtual ? (
              <VirtualRows
                rows={rows}
                scroller={scroller}
                columns={columns}
                renderRow={renderRow}
              />
            ) : (
              rows.map(renderRow)
            )}
          </TableBody>
        </table>
      </div>
      <p className="text-xs text-muted-foreground">
        {rows.length === items.length
          ? t("janitorRun.items", { count: items.length })
          : t("janitorRun.itemsFiltered", { shown: rows.length, count: items.length })}
      </p>
    </>
  );
}

// SortHeader is a column title that is a button: a first press sorts ascending, a second descending,
// a third returns to the order the run attempted the items in. The header carries aria-sort.
function SortHeader({
  sort,
  column,
  className,
  onSort,
}: {
  sort: Sort | undefined;
  column: SortKey;
  className: string;
  onSort: () => void;
}) {
  const t = useT();
  const active = sort?.key === column ? sort.dir : undefined;
  const label = t(`janitorRun.columns.${column}`);
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
        title={t("janitorRun.sortBy", { column: label })}
        onClick={onSort}
      >
        {label}
        <Icon aria-hidden="true" className="size-3.5 text-muted-foreground" />
      </button>
    </TableHead>
  );
}

interface VirtualRowsProps {
  rows: readonly JanitorRunItem[];
  scroller: HTMLDivElement | null;
  columns: number;
  renderRow: (row: JanitorRunItem) => ReactNode;
}

// VirtualRows draws the rows in view between two spacer rows that hold the height of the rest, so
// the table stays a native table and a long list stays responsive.
function VirtualRows({ rows, scroller, columns, renderRow }: VirtualRowsProps) {
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller,
    estimateSize: () => ROW_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => rows[index]?.seq ?? index,
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
          <td colSpan={columns} />
        </tr>
      ) : null}
      {inView.map((v) => {
        const row = rows[v.index];
        return row ? renderRow(row) : null;
      })}
      {after > 0 ? (
        <tr style={{ height: after }}>
          <td colSpan={columns} />
        </tr>
      ) : null}
    </>
  );
}
