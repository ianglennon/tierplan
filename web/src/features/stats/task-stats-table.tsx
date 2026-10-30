// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useId } from "react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { formatCpu, formatMemory, formatRate } from "@/features/stats/format";
import { Sparkline } from "@/features/stats/sparkline";
import { useT } from "@/i18n/use-t";

// How many characters of a task's ID are shown beside its name.
const SHORT_ID = 8;
const NO_VALUE = "—";

// One running task of a service, with its latest sample and the CPU it has used over the last
// minutes. A value is null while it could not be told; noData is set for a task on a node
// that is not reporting.
export interface TaskStatsRow {
  id: string;
  slot: number;
  node: string;
  cpu: number | null;
  mem: number | null;
  netRx: number | null;
  netTx: number | null;
  blkRead: number | null;
  blkWrite: number | null;
  noData: boolean;
  trend: ReadonlyArray<number | null>;
}

// A task is called by its service and its slot, web.2; a global service's task by the service
// alone, since the node has its own column.
const taskName = (serviceName: string, slot: number): string =>
  slot > 0 ? `${serviceName}.${slot}` : serviceName;

function NodeName({ node }: { node: string }) {
  const t = useT();
  const name = node || t("stats.thisNode");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="block max-w-48 truncate">{name}</span>
      </TooltipTrigger>
      <TooltipContent className="break-all">{name}</TooltipContent>
    </Tooltip>
  );
}

// TaskStatsTable is the per-task breakdown under the totals of a service's Stats tab (D-29,
// UI-SPEC "Stats"). A task whose node is not reporting says No data and why. The table
// scrolls inside its card, so a service of a hundred tasks does not lengthen the page.
export function TaskStatsTable({
  serviceName,
  rows,
}: {
  serviceName: string;
  rows: readonly TaskStatsRow[];
}) {
  const t = useT();
  const headingId = useId();
  return (
    <section
      aria-labelledby={headingId}
      className="flex flex-col gap-2 rounded-xl bg-card p-4 text-card-foreground ring-1 ring-foreground/10"
    >
      <h2 id={headingId} className="text-base leading-6 font-semibold">
        {t("stats.tasks.title")}
      </h2>
      <div data-slot="task-stats-scroll" className="max-h-96 overflow-x-auto overflow-y-auto">
        <Table aria-labelledby={headingId}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("stats.tasks.columns.task")}</TableHead>
              <TableHead>{t("stats.tasks.columns.node")}</TableHead>
              <TableHead className="text-end">{t("stats.tiles.cpu")}</TableHead>
              <TableHead className="text-end">{t("stats.tiles.memory")}</TableHead>
              <TableHead className="text-end">{t("stats.tiles.network")}</TableHead>
              <TableHead className="text-end">{t("stats.tiles.block")}</TableHead>
              <TableHead>{t("stats.tasks.columns.trend")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((row) => (
              <TableRow key={row.id} className="h-9">
                <TableCell>
                  <div className="flex flex-col">
                    <span className="font-mono text-xs break-all">
                      {taskName(serviceName, row.slot)}
                    </span>
                    <span className="font-mono text-xs text-muted-foreground">
                      {row.id.slice(0, SHORT_ID)}
                    </span>
                  </div>
                </TableCell>
                <TableCell>
                  <NodeName node={row.node} />
                </TableCell>
                {row.noData ? (
                  <TableCell colSpan={4} className="text-end text-muted-foreground">
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <button
                          type="button"
                          className="cursor-help rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                        >
                          {t("stats.tasks.noData")}
                        </button>
                      </TooltipTrigger>
                      <TooltipContent>
                        {t("stats.tasks.noDataTip", { node: row.node || t("stats.thisNode") })}
                      </TooltipContent>
                    </Tooltip>
                  </TableCell>
                ) : (
                  <>
                    <TableCell className="text-end tabular-nums">{formatCpu(row.cpu)}</TableCell>
                    <TableCell className="text-end tabular-nums">{formatMemory(row.mem)}</TableCell>
                    <TableCell className="text-end tabular-nums">
                      <Pair
                        a={row.netRx}
                        b={row.netTx}
                        glyphs={["↓", "↑"]}
                        sr={(a, b) => t("stats.list.networkSr", { inRate: a, outRate: b })}
                      />
                    </TableCell>
                    <TableCell className="text-end tabular-nums">
                      <Pair
                        a={row.blkRead}
                        b={row.blkWrite}
                        glyphs={["R", "W"]}
                        sr={(a, b) => t("stats.tasks.blockSr", { read: a, write: b })}
                      />
                    </TableCell>
                  </>
                )}
                <TableCell>
                  <Sparkline values={row.trend} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <p className="text-xs text-muted-foreground">{t("stats.tasks.stoppedNote")}</p>
    </section>
  );
}

// Pair is two rates side by side, with glyphs for the eye and words for assistive technology.
function Pair({
  a,
  b,
  glyphs,
  sr,
}: {
  a: number | null;
  b: number | null;
  glyphs: [string, string];
  sr: (a: string, b: string) => string;
}) {
  if (a === null && b === null) return <span>{NO_VALUE}</span>;
  const first = formatRate(a);
  const second = formatRate(b);
  return (
    <>
      <span aria-hidden="true">{`${glyphs[0]} ${first} ${glyphs[1]} ${second}`}</span>
      <span className="sr-only">{sr(first, second)}</span>
    </>
  );
}
