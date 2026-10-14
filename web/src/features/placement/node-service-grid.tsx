// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import {
  Popover,
  PopoverContent,
  PopoverHeader,
  PopoverTitle,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { HEAT_CLASS, heatStep } from "@/features/placement/heat";
import {
  type ColumnKind,
  filterTasks,
  type PlacementData,
  type PlacementFilters,
  type PlacementNode,
  type PlacementTask,
  passes,
} from "@/features/placement/project";
import { taskState } from "@/features/tasks/task-state";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export interface GridColumn {
  key: string;
  kind: ColumnKind;
  node?: PlacementNode;
  // The tasks the filters leave in the column.
  count: number;
}

export interface GridRow {
  serviceId: string;
  name: string;
  // The tasks in each column, in the order of the columns.
  cells: PlacementTask[][];
  total: number;
}

export interface GridBand {
  // The stack's name; empty for the services that belong to no stack.
  stack: string;
  rows: GridRow[];
}

export interface GridModel {
  columns: GridColumn[];
  bands: GridBand[];
  // Tasks per column, and in all.
  totals: number[];
  total: number;
}

const names = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });

const byService = (a: PlacementTask, b: PlacementTask) =>
  a.slot - b.slot || a.id.localeCompare(b.id);

// projectGrid turns the answer into the matrix of the Grid layout: a column per node in node-name
// order whether or not it holds tasks, then Not placed and Removed node when they hold some; a row
// per service, under bands of stacks A to Z with the services in no stack last. The filters
// narrow the rows and never the nodes. keepServices keeps a row for every service the answer
// lists, so a service with no task still shows as a row of zeros.
export function projectGrid(
  data: PlacementData,
  filters: PlacementFilters,
  keepServices = false,
): GridModel {
  const kept = filterTasks(data, filters);
  const nodes = [...(data.nodes ?? [])].sort(
    (a, b) => names.compare(a.hostname, b.hostname) || a.id.localeCompare(b.id),
  );
  const known = new Set(nodes.map((n) => n.id));
  const columnOf = (t: PlacementTask) => {
    if (t.nodeId === "") return "not-placed";
    if (!t.nodeKnown || !known.has(t.nodeId)) return "removed";
    return `node:${t.nodeId}`;
  };

  const columns: GridColumn[] = nodes.map((n) => ({
    key: `node:${n.id}`,
    kind: "node",
    node: n,
    count: 0,
  }));
  if (kept.some((t) => columnOf(t) === "not-placed")) {
    columns.push({ key: "not-placed", kind: "notPlaced", count: 0 });
  }
  if (kept.some((t) => columnOf(t) === "removed")) {
    columns.push({ key: "removed", kind: "removed", count: 0 });
  }
  const at = new Map(columns.map((c, i) => [c.key, i]));

  const services = new Map<string, { name: string; stack: string; tasks: PlacementTask[] }>();
  for (const t of kept) {
    const found = services.get(t.serviceId);
    if (found) found.tasks.push(t);
    else services.set(t.serviceId, { name: t.service, stack: t.stack, tasks: [t] });
  }
  if (keepServices) {
    for (const s of data.services ?? []) {
      if (!services.has(s.id) && passes(filters, s.stack, s.name)) {
        services.set(s.id, { name: s.name, stack: s.stack, tasks: [] });
      }
    }
  }

  const totals = columns.map(() => 0);
  const byStack = new Map<string, GridRow[]>();
  for (const [serviceId, s] of services) {
    const cells: PlacementTask[][] = columns.map(() => []);
    for (const t of s.tasks) {
      const i = at.get(columnOf(t));
      if (i === undefined) continue;
      (cells[i] as PlacementTask[]).push(t);
      totals[i] = (totals[i] ?? 0) + 1;
    }
    for (const list of cells) list.sort(byService);
    const row: GridRow = { serviceId, name: s.name, cells, total: s.tasks.length };
    const list = byStack.get(s.stack);
    if (list) list.push(row);
    else byStack.set(s.stack, [row]);
  }
  for (const [i, c] of columns.entries()) c.count = totals[i] ?? 0;

  const bands = [...byStack.entries()]
    .sort(([a], [b]) => (a === "" ? 1 : b === "" ? -1 : names.compare(a, b)))
    .map(([stack, rows]) => ({
      stack,
      rows: rows.sort(
        (a, b) => names.compare(a.name, b.name) || a.serviceId.localeCompare(b.serviceId),
      ),
    }));
  return { columns, bands, totals, total: totals.reduce((n, x) => n + x, 0) };
}

export interface NodeServiceGridProps {
  envId: string;
  data: PlacementData;
  filters: PlacementFilters;
  // Keep a row for every service the answer lists, also one with no task.
  keepServices?: boolean;
  // Told when a cell's popover opens and closes, so the page can hold the grid still while one
  // is open (UI-SPEC "Live updates").
  onPopoverOpenChange?: (open: boolean) => void;
}

// A count: zero in muted text, anything else plain. The shade never replaces it.
function Count({ n }: { n: number }) {
  return n === 0 ? <span className="text-muted-foreground">0</span> : <span>{n}</span>;
}

// The dot in a cell's corner when a task in it is not running: the offline colour when any of
// them failed, the warning colour otherwise. Its words are in the tooltip, and the popover names
// each task's state.
function CellDot({ tasks }: { tasks: PlacementTask[] }) {
  const t = useT();
  const notRunning = tasks.filter((x) => x.state !== "running");
  if (notRunning.length === 0) return null;
  const failed = notRunning.some((x) => taskState(x.state).tone === "offline");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          data-slot="cell-dot"
          aria-hidden="true"
          className={cn(
            "absolute top-1 right-1 size-2 rounded-full",
            failed ? "bg-status-offline" : "bg-status-warning",
          )}
        />
      </TooltipTrigger>
      <TooltipContent>
        {t("placement.grid.notRunning", { count: notRunning.length })}
      </TooltipContent>
    </Tooltip>
  );
}

interface CellProps {
  envId: string;
  row: GridRow;
  column: GridColumn;
  tasks: PlacementTask[];
  rowMax: number;
  onPopoverOpenChange?: (open: boolean) => void;
}

// Cell is one service on one node: its count, shaded by the row's maximum, as a button that
// opens a popover listing the tasks with their states, each a link to the service. A cell with
// no task is the muted 0 and nothing to open.
function Cell({ envId, row, column, tasks, rowMax, onPopoverOpenChange }: CellProps) {
  const t = useT();
  const n = tasks.length;
  const step = heatStep(n, rowMax);
  const title =
    column.kind === "notPlaced"
      ? t("placement.grid.cellNotPlaced", { service: row.name })
      : column.kind === "removed"
        ? t("placement.grid.cellRemoved", { service: row.name })
        : t("placement.grid.cellNode", { service: row.name, node: column.node?.hostname ?? "" });
  return (
    <td
      data-heat={step}
      className={cn(
        "h-10 border-r border-b p-0 text-center tabular-nums max-md:h-11",
        HEAT_CLASS[step],
      )}
    >
      {n === 0 ? (
        <Count n={0} />
      ) : (
        <Popover onOpenChange={onPopoverOpenChange}>
          <PopoverTrigger asChild>
            <button
              type="button"
              aria-label={t("placement.grid.cell", { title, count: n })}
              className="relative flex h-10 w-full min-w-16 items-center justify-center outline-none hover:bg-accent/60 focus-visible:ring-3 focus-visible:ring-ring/50 max-md:h-11"
            >
              <Count n={n} />
              <CellDot tasks={tasks} />
            </button>
          </PopoverTrigger>
          <PopoverContent align="start" className="max-h-80 w-72 overflow-y-auto">
            <PopoverHeader>
              <PopoverTitle className="break-all">{title}</PopoverTitle>
            </PopoverHeader>
            <ul className="flex flex-col gap-1">
              {tasks.map((task) => {
                const state = t(taskState(task.state).labelKey);
                return (
                  <li key={task.id} className="flex min-w-0 flex-col">
                    <Link
                      to="/env/$envId/services/$serviceId"
                      params={{ envId, serviceId: task.serviceId }}
                      className="rounded-sm text-accent-ink hover:underline"
                    >
                      {task.slot > 0
                        ? t("placement.grid.task", { slot: task.slot, state })
                        : t("placement.grid.taskGlobal", { state })}
                    </Link>
                    {task.error ? (
                      <span className="text-xs break-words text-status-offline-text">
                        {task.error}
                      </span>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          </PopoverContent>
        </Popover>
      )}
    </td>
  );
}

// The name of a service or node as a line of a header, cut at the cell's width with the whole
// name in a tooltip.
function Name({ children, className }: { children: string; className?: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className={cn("block min-w-0 truncate", className)}>{children}</span>
      </TooltipTrigger>
      <TooltipContent className="break-all">{children}</TooltipContent>
    </Tooltip>
  );
}

const STICKY_FIRST = "sticky left-0 z-10 w-60 min-w-60 max-w-60 bg-card border-r border-b";
const STICKY_HEAD = "sticky top-0 z-20 bg-card border-b";

// NodeServiceGrid is the Grid layout (D-18): services as rows under stack bands, nodes as
// columns, the number of tasks in each cell, shaded by the row's maximum. A native table, so
// the headers name the cells to a screen reader; the first column and the header row stick while
// the table scrolls both ways. Every Docker string is a text node.
export function NodeServiceGrid({
  envId,
  data,
  filters,
  keepServices,
  onPopoverOpenChange,
}: NodeServiceGridProps) {
  const t = useT();
  const grid = projectGrid(data, filters, keepServices);
  const span = grid.columns.length + 2;
  const columnName = (c: GridColumn) =>
    c.kind === "notPlaced"
      ? t("placement.grid.notPlaced")
      : c.kind === "removed"
        ? t("placement.grid.removed")
        : (c.node?.hostname ?? "");
  return (
    <div className="flex flex-col gap-2">
      <div
        data-slot="node-service-grid"
        className="max-h-[max(20rem,calc(100svh-18rem))] overflow-auto rounded-lg border bg-card"
      >
        <table
          aria-label={t("placement.grid.label")}
          className="w-full border-separate border-spacing-0 text-sm"
        >
          <thead>
            <tr>
              <th
                scope="col"
                className={cn(STICKY_HEAD, STICKY_FIRST, "z-30 h-12 px-3 text-left font-semibold")}
              >
                {t("placement.grid.corner")}
              </th>
              {grid.columns.map((c) => (
                <th
                  key={c.key}
                  scope="col"
                  className={cn(
                    STICKY_HEAD,
                    "h-12 min-w-24 max-w-48 border-r px-2 text-center font-semibold",
                    c.kind !== "node" && "text-muted-foreground",
                  )}
                >
                  <Name>{columnName(c)}</Name>
                  <span className="block text-xs font-normal text-muted-foreground tabular-nums">
                    {t("placement.column.tasks", { count: c.count })}
                  </span>
                </th>
              ))}
              <th
                scope="col"
                className={cn(STICKY_HEAD, "h-12 min-w-20 px-2 text-center font-semibold")}
              >
                {t("placement.grid.total")}
              </th>
            </tr>
          </thead>
          {grid.bands.map((band) => (
            <tbody key={band.stack}>
              <tr>
                <th
                  scope="rowgroup"
                  colSpan={span}
                  className="sticky left-0 h-8 border-b bg-muted/50 px-3 text-left text-sm font-semibold"
                >
                  {band.stack || t("placement.column.noStack")}
                </th>
              </tr>
              {band.rows.map((row) => {
                const rowMax = Math.max(0, ...row.cells.map((c) => c.length));
                return (
                  <tr key={row.serviceId}>
                    <th
                      scope="row"
                      className={cn(STICKY_FIRST, "h-10 px-3 text-left font-normal max-md:h-11")}
                    >
                      <Link
                        to="/env/$envId/services/$serviceId"
                        params={{ envId, serviceId: row.serviceId }}
                        className="block min-w-0 truncate rounded-sm text-accent-ink hover:underline"
                        title={row.name}
                      >
                        {row.name}
                      </Link>
                    </th>
                    {grid.columns.map((c, i) => (
                      <Cell
                        key={c.key}
                        envId={envId}
                        row={row}
                        column={c}
                        tasks={row.cells[i] ?? []}
                        rowMax={rowMax}
                        onPopoverOpenChange={onPopoverOpenChange}
                      />
                    ))}
                    <td className="h-10 border-b px-2 text-center font-semibold tabular-nums max-md:h-11">
                      <Count n={row.total} />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          ))}
          <tfoot>
            <tr>
              <th scope="row" className={cn(STICKY_FIRST, "h-10 px-3 text-left font-semibold")}>
                {t("placement.grid.total")}
              </th>
              {grid.columns.map((c, i) => (
                <td
                  key={c.key}
                  className="h-10 border-r border-b px-2 text-center font-semibold tabular-nums"
                >
                  <Count n={grid.totals[i] ?? 0} />
                </td>
              ))}
              <td className="h-10 border-b px-2 text-center font-semibold tabular-nums">
                <Count n={grid.total} />
              </td>
            </tr>
          </tfoot>
        </table>
      </div>
      <p className="text-xs text-muted-foreground">{t("placement.grid.note")}</p>
    </div>
  );
}
