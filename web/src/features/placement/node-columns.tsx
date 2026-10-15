// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Crown } from "lucide-react";
import { useId } from "react";

import type { HealthState } from "@/api/environments";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { PlacementColumn, PlacementNode } from "@/features/placement/project";
import { TaskCard } from "@/features/placement/task-card";
import type { NodeStat } from "@/features/stats/list-stats";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";

// A column with more tasks than this keeps its stack sub-headers in view while it scrolls.
const STICKY_AFTER = 40;

// The status dot of a node the swarm cannot count on: down and disconnected are the offline
// red, anything it cannot name is neutral. A ready node has none.
const NODE_TONE: Record<string, HealthState> = {
  down: "offline",
  disconnected: "offline",
  unknown: "unknown",
};

// The latest round of the node stream, when the Live stats switch is on; left out when off.
export interface NodeStatsView {
  nodes: ReadonlyMap<string, NodeStat>;
  partial: boolean;
}

export interface NodeColumnsProps {
  envId: string;
  columns: readonly PlacementColumn[];
  // What the tasks use of each node. Left out, the headers show capacity alone.
  stats?: NodeStatsView;
  // What a column with no tasks says in place of cards.
  emptyLabel?: string;
}

// NodeColumns is the By node layout (D-18): one column per node, the whole row scrolling
// sideways with snap. Each column is a section named by its h2, its tasks cards in lists under
// stack sub-headers. Down and disconnected nodes stay, muted, so their orphaned tasks are seen.
export function NodeColumns({ envId, columns, stats, emptyLabel }: NodeColumnsProps) {
  return (
    <div
      data-slot="node-columns"
      className="flex snap-x snap-proximity items-start gap-4 overflow-x-auto pb-2"
    >
      {columns.map((column) => (
        <NodeColumn
          key={column.key}
          envId={envId}
          column={column}
          stats={stats}
          emptyLabel={emptyLabel}
        />
      ))}
    </div>
  );
}

// Three columns of skeleton, drawn while the first answer is on its way.
export function ColumnsSkeleton() {
  return (
    <div data-slot="node-columns-skeleton" className="flex gap-4 overflow-x-hidden pb-2">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex w-72 shrink-0 flex-col rounded-lg border bg-card">
          <div className="flex h-24 flex-col gap-2 border-b p-3">
            <Skeleton className="h-5 w-32" />
            <Skeleton className="h-4 w-24" />
            <Skeleton className="h-4 w-40" />
          </div>
          <div className="flex min-h-80 flex-col gap-2 p-2">
            {[0, 1, 2].map((j) => (
              <Skeleton key={j} className="h-10 w-full" />
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

// ColumnName is the column's h2. A node's name is a link to the node's page; the columns that are
// not nodes (Not placed, Removed node) have no page to go to.
function ColumnName({
  name,
  id,
  link,
}: {
  name: string;
  id: string;
  link?: { envId: string; nodeId: string };
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <h2 id={id} className="min-w-0 truncate text-base leading-6 font-semibold">
          {link ? (
            <Link
              to="/env/$envId/nodes/$nodeId"
              params={link}
              className="rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              {name}
            </Link>
          ) : (
            name
          )}
        </h2>
      </TooltipTrigger>
      <TooltipContent className="break-all">{name}</TooltipContent>
    </Tooltip>
  );
}

function availabilityText(node: PlacementNode, tasks: number, t: ReturnType<typeof useT>) {
  switch (node.availability) {
    case "pause":
      return t("placement.node.availability.pause");
    case "drain":
      return tasks > 0
        ? t("placement.node.availability.drain", { count: tasks })
        : t("placement.node.availability.drained");
    default:
      return t("placement.node.availability.active");
  }
}

const clampPercent = (n: number): number => Math.max(0, Math.min(100, Math.round(n)));

// NodeUsage is what the tasks on a node use of it (D-31): a line in words and two 8px bars,
// CPU against the node's CPUs and memory against its memory. A node that is not reporting
// says so in place of the bars.
function NodeUsage({
  node,
  stat,
  partial,
}: {
  node: PlacementNode;
  stat: NodeStat | undefined;
  partial: boolean;
}) {
  const t = useT();
  if (!stat) {
    return <p className="text-xs text-muted-foreground">{t("stats.placement.waiting")}</p>;
  }
  if (stat.notReporting) {
    return <p className="text-xs text-muted-foreground">{t("stats.placement.notReporting")}</p>;
  }
  const cpus = stat.cpus > 0 ? stat.cpus : node.cpus;
  const total = stat.memoryBytes > 0 ? stat.memoryBytes : node.memoryBytes;
  const cpu = cpus > 0 && stat.cpu !== null ? clampPercent((stat.cpu / (cpus * 100)) * 100) : null;
  const mem = total > 0 ? clampPercent((stat.mem / total) * 100) : null;
  return (
    <div className="flex flex-col gap-1">
      <p className="truncate text-xs text-muted-foreground tabular-nums">
        {t("stats.placement.usage", {
          cpu: cpu ?? "—",
          used: formatBytes(stat.mem),
          total: total > 0 ? formatBytes(total) : "—",
        })}
      </p>
      <Progress
        value={cpu}
        aria-label={t("stats.placement.cpuBar", { node: node.hostname })}
        className="h-2"
      />
      <Progress
        value={mem}
        aria-label={t("stats.placement.memoryBar", { node: node.hostname })}
        className="h-2"
      />
      {partial ? (
        <p className="text-xs text-muted-foreground">{t("stats.placement.partial")}</p>
      ) : null}
    </div>
  );
}

function NodeHeaderFacts({
  node,
  tasks,
  stats,
}: {
  node: PlacementNode;
  tasks: number;
  stats?: NodeStatsView;
}) {
  const t = useT();
  const tone = NODE_TONE[node.state];
  const capacity =
    node.cpus > 0 || node.memoryBytes > 0
      ? t("placement.column.capacity", {
          cpus: t("placement.column.cpus", { count: node.cpus }),
          memory: formatBytes(node.memoryBytes),
        })
      : t("placement.column.noCapacity");
  return (
    <>
      <div className="flex flex-wrap items-center gap-1">
        <Badge variant="outline">
          {node.role === "manager" ? t("placement.column.manager") : t("placement.column.worker")}
        </Badge>
        {node.leader ? (
          <Badge variant="outline">
            <Crown aria-hidden="true" />
            {t("placement.column.leader")}
          </Badge>
        ) : null}
        {tone ? (
          <StatusDot
            state={tone}
            label={t(
              `placement.node.state.${node.state === "down" || node.state === "disconnected" ? node.state : "unknown"}`,
            )}
          />
        ) : null}
        <span className="text-xs text-muted-foreground">{availabilityText(node, tasks, t)}</span>
      </div>
      <p className="truncate text-xs text-muted-foreground">{capacity}</p>
      {stats ? (
        <NodeUsage node={node} stat={stats.nodes.get(node.id)} partial={stats.partial} />
      ) : null}
    </>
  );
}

function NodeColumn({
  envId,
  column,
  stats,
  emptyLabel,
}: {
  envId: string;
  column: PlacementColumn;
  stats?: NodeStatsView;
  emptyLabel?: string;
}) {
  const t = useT();
  const headingId = useId();
  const { node } = column;
  const down = node?.state === "down" || node?.state === "disconnected";
  const muted = down || column.kind === "notPlaced";
  const sticky = column.count > STICKY_AFTER;
  const name =
    column.kind === "notPlaced"
      ? t("placement.column.notPlaced")
      : column.kind === "removed"
        ? t("placement.column.removed")
        : (node?.hostname ?? "");
  const note =
    column.kind === "notPlaced"
      ? t("placement.column.notPlacedTooltip")
      : column.kind === "removed"
        ? t("placement.column.removedTooltip")
        : undefined;
  return (
    <section
      aria-labelledby={headingId}
      data-slot="node-column"
      data-muted={muted ? "true" : undefined}
      className={cn(
        "flex w-72 shrink-0 snap-start flex-col rounded-lg border bg-card",
        muted && "bg-muted/40",
      )}
    >
      <header
        className={cn(
          "flex min-h-24 flex-col justify-center gap-1 border-b px-3 py-2",
          muted && "text-muted-foreground",
        )}
      >
        <ColumnName
          name={name}
          id={headingId}
          link={node && column.kind === "node" ? { envId, nodeId: node.id } : undefined}
        />
        {node ? (
          <NodeHeaderFacts node={node} tasks={column.count} stats={stats} />
        ) : (
          <p className="text-xs text-muted-foreground">{note}</p>
        )}
        <p className="text-xs text-muted-foreground tabular-nums">
          {t("placement.column.tasks", { count: column.count })}
        </p>
      </header>
      <div className="flex max-h-[max(20rem,calc(100svh-22rem))] min-h-80 flex-col gap-2 overflow-y-auto p-2">
        {column.groups.length === 0 ? (
          <p className="px-1 text-sm text-muted-foreground">
            {emptyLabel ?? t("placement.column.none")}
          </p>
        ) : (
          column.groups.map((group) => (
            <div key={group.stack} className="flex flex-col gap-2">
              <h3
                className={cn(
                  "flex h-8 shrink-0 items-center justify-between gap-2 px-1 text-sm font-semibold",
                  sticky && "sticky top-0 z-10 bg-card",
                )}
              >
                <span className="min-w-0 truncate">
                  {group.stack || t("placement.column.noStack")}
                </span>
                <span className="text-xs font-normal text-muted-foreground tabular-nums">
                  {group.tasks.length}
                </span>
              </h3>
              <ul className="flex flex-col gap-2">
                {group.tasks.map((task) => (
                  <li key={task.id} className="min-w-0">
                    <TaskCard envId={envId} task={task} />
                  </li>
                ))}
              </ul>
            </div>
          ))
        )}
      </div>
    </section>
  );
}
