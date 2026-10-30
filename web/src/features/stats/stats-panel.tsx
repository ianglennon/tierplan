// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Activity } from "lucide-react";
import { lazy, type ReactNode, Suspense, useEffect, useMemo, useState } from "react";

import { LiveChip } from "@/components/live-chip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import type { EventSourceLike } from "@/features/dashboard/events";
import { formatCpu, formatMemory, formatRate, percentOf } from "@/features/stats/format";
import { useServiceStats } from "@/features/stats/service-stats";
import { StatTile } from "@/features/stats/stat-tile";
import {
  createStatsStream,
  type StatsCloseReason,
  type StatsReady,
  type StatsStatus,
} from "@/features/stats/stats-stream";
import { type TaskStatsRow, TaskStatsTable } from "@/features/stats/task-stats-table";
import { isGap, type StatsPoint, useStatsHistory } from "@/features/stats/use-stats-history";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

// The chart pulls in recharts, so it loads with the Stats tab and never with a list.
const StatsChart = lazy(() =>
  import("@/features/stats/stats-chart").then((m) => ({ default: m.StatsChart })),
);

interface StatsPanelProps {
  envId: string;
  containerId: string;
  nodeId?: string;
  // The container's name, for the surface's label and the not-running state.
  name: string;
  // What the page already knows of the container. False shows the not-running state at once
  // and opens no stream; true or unknown opens one.
  running?: boolean;
  // The node's display name, for the notice when it stops reporting.
  nodeName?: string;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
}

function ChartFallback() {
  const t = useT();
  return (
    <div
      className="flex h-48 w-full flex-col gap-2 rounded-xl bg-card p-4 ring-1 ring-foreground/10"
      aria-busy="true"
    >
      <span role="status" className="sr-only">
        {t("stats.charts.loading")}
      </span>
      <Skeleton className="size-full" aria-hidden="true" />
    </div>
  );
}

interface StatsLayoutProps {
  // The surface's name, for its accessible label.
  name: string;
  status: StatsStatus;
  closedBy: StatsCloseReason | undefined;
  points: StatsPoint[];
  // The memory limit the chart's dotted line is drawn at, in bytes; null for none.
  limit: number | null;
  // True once the stream has answered with a round even though no task gave a sample (every
  // one is on a node that is not reporting): the tiles then read — instead of waiting.
  answered?: boolean;
  // The line under the memory tile, worked out from the latest sample by the surface.
  memoryLine: (latestMem: number | null) => string;
  // Alerts above the tiles that are specific to the surface, and what follows the charts.
  notices?: ReactNode;
  footer?: ReactNode;
}

// StatsLayout is what the container and the service Stats tabs share: four tiles with their
// trends and four charts, refreshed about every 2 seconds, with the stream's state. What is
// shown is the history it is given; this keeps none.
function StatsLayout({
  name,
  status,
  closedBy,
  points,
  limit,
  answered = false,
  memoryLine,
  notices,
  footer,
}: StatsLayoutProps) {
  const t = useT();
  const real = useMemo(() => points.filter((p) => !isGap(p)), [points]);
  const latest = real[real.length - 1];
  const waiting = latest === undefined && !answered;
  const cpu = points.map((p) => p.cpu);

  return (
    <section aria-label={t("stats.surface", { name })} className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="text-sm text-muted-foreground">{t("stats.note")}</p>
        {status === "live" || status === "reconnecting" ? <LiveChip state={status} /> : null}
      </div>

      {status === "paused" ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("stats.paused")}
        </p>
      ) : null}
      {status === "closed" && closedBy ? (
        <Alert variant="destructive">
          <AlertDescription>{t(`stats.closed.${closedBy}`)}</AlertDescription>
        </Alert>
      ) : null}
      {notices}

      <fieldset
        aria-live="off"
        className="m-0 grid min-w-0 gap-4 border-0 p-0 sm:grid-cols-2 lg:grid-cols-4"
      >
        <legend className="sr-only">{t("stats.tiles.label")}</legend>
        <StatTile
          label={t("stats.tiles.cpu")}
          hint={t("stats.tiles.cpuHint")}
          lines={[formatCpu(latest?.cpu)]}
          trend={cpu}
          loading={waiting}
        />
        <StatTile
          label={t("stats.tiles.memory")}
          lines={[formatMemory(latest?.mem)]}
          subline={memoryLine(latest?.mem ?? null)}
          trend={points.map((p) => p.mem)}
          loading={waiting}
        />
        <StatTile
          label={t("stats.tiles.network")}
          lines={[
            t("stats.tiles.networkIn", { rate: formatRate(latest?.netRx) }),
            t("stats.tiles.networkOut", { rate: formatRate(latest?.netTx) }),
          ]}
          trend={points.map((p) => p.netRx)}
          loading={waiting}
        />
        <StatTile
          label={t("stats.tiles.block")}
          lines={[
            t("stats.tiles.blockRead", { rate: formatRate(latest?.blkRead) }),
            t("stats.tiles.blockWrite", { rate: formatRate(latest?.blkWrite) }),
          ]}
          trend={points.map((p) => p.blkRead)}
          loading={waiting}
        />
      </fieldset>

      {waiting && status !== "paused" && status !== "closed" ? (
        <p role="status" className="text-sm text-muted-foreground">
          {t("stats.waiting")}
        </p>
      ) : null}

      <div className="grid gap-4 lg:grid-cols-2">
        <Suspense fallback={<ChartFallback />}>
          <StatsChart
            title={t("stats.charts.cpu")}
            points={points}
            series={[{ key: "cpu", label: t("stats.tiles.cpu") }]}
            format={formatCpu}
            floor={100}
          />
          <StatsChart
            title={t("stats.charts.memory")}
            points={points}
            series={[{ key: "mem", label: t("stats.tiles.memory") }]}
            format={formatMemory}
            limit={limit}
            limitLabel={limit ? t("stats.charts.limit", { size: formatBytes(limit) }) : undefined}
          />
          <StatsChart
            title={t("stats.charts.network")}
            points={points}
            series={[
              { key: "netRx", label: t("stats.charts.in") },
              { key: "netTx", label: t("stats.charts.out"), dashed: true },
            ]}
            format={formatRate}
          />
          <StatsChart
            title={t("stats.charts.block")}
            points={points}
            series={[
              { key: "blkRead", label: t("stats.charts.read") },
              { key: "blkWrite", label: t("stats.charts.write"), dashed: true },
            ]}
            format={formatRate}
          />
        </Suspense>
      </div>
      {footer}
    </section>
  );
}

// StatsPanel is a container's live resource use: four tiles with their trends and four
// charts, refreshed about every 2 seconds from one stream. The last five minutes are kept
// in this component's state and go when the page is left; nothing is stored (D-30).
export function StatsPanel({
  envId,
  containerId,
  nodeId,
  name,
  running,
  nodeName,
  createSource,
}: StatsPanelProps) {
  const t = useT();
  const { points, push, markGap, clear } = useStatsHistory();
  const [status, setStatus] = useState<StatsStatus>("connecting");
  const [closedBy, setClosedBy] = useState<StatsCloseReason | undefined>();
  const [ready, setReady] = useState<StatsReady | null>(null);
  const [silent, setSilent] = useState(false);
  const [stopped, setStopped] = useState(false);

  useEffect(() => {
    if (running === false) return;
    setStatus("connecting");
    setClosedBy(undefined);
    setSilent(false);
    setStopped(false);
    const stream = createStatsStream({
      envId,
      containerId,
      nodeId,
      createSource,
      onReady: setReady,
      onSample: push,
      onGap: markGap,
      onNode: (state) => setSilent(state === "not_reporting"),
      onContainerState: (state) => setStopped(state === "stopped"),
      onStatus: (s) => {
        setStatus(s.status);
        setClosedBy(s.reason);
      },
    });
    return () => {
      stream.close();
      // The history is this visit's; a stream for another container starts empty.
      clear();
    };
  }, [envId, containerId, nodeId, running, createSource, push, markGap, clear]);

  if (running === false || closedBy === "not_running" || ready?.running === false) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Activity aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("stats.empty.title", { name })}
          </EmptyTitle>
          <EmptyDescription>{t("stats.empty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }

  const limit = ready?.limitBytes ?? null;
  return (
    <StatsLayout
      name={name}
      status={status}
      closedBy={closedBy}
      points={points}
      limit={limit}
      memoryLine={(mem) =>
        limit && mem != null
          ? t("stats.tiles.memoryLimit", {
              percent: percentOf(mem, limit),
              size: formatBytes(limit),
            })
          : t("stats.tiles.memoryNoLimit")
      }
      notices={
        <>
          {silent ? (
            <Alert>
              <AlertDescription>
                {t("stats.notReporting", { node: nodeName ?? t("stats.thisNode") })}
              </AlertDescription>
            </Alert>
          ) : null}
          {stopped ? (
            <Alert>
              <AlertDescription>{t("stats.stopped")}</AlertDescription>
            </Alert>
          ) : null}
        </>
      }
    />
  );
}

interface ServiceStatsPanelProps {
  envId: string;
  serviceId: string;
  // The service's name, for the surface's label and the names of its tasks.
  name: string;
  // The real EventSource unless a test supplies one.
  createSource?: (url: string) => EventSourceLike;
}

// ServiceStatsPanel is a service's live resource use (D-29): the sum over its running tasks in
// the tiles and charts, and under them each task's own numbers with a CPU trend. A task on a
// node that is not reporting is left out of the totals and says so. Stopped tasks drop out,
// and new ones join within 10 seconds. The history is this component's and goes with it.
export function ServiceStatsPanel({
  envId,
  serviceId,
  name,
  createSource,
}: ServiceStatsPanelProps) {
  const t = useT();
  const s = useServiceStats(envId, serviceId, createSource);
  const counted = s.round?.total.counted ?? 0;
  const silent = s.round?.total.silent ?? 0;
  // The tasks' limits add up to the limit of the total, over the tasks that answered.
  const limitTotal = s.limitBytes && counted > 0 ? s.limitBytes * counted : null;

  const rows = useMemo<TaskStatsRow[]>(
    () =>
      (s.tasks ?? []).map((task) => {
        const sample = s.round?.tasks.find((x) => x.id === task.id);
        return {
          id: task.id,
          slot: task.slot,
          node: task.node,
          cpu: sample?.cpu ?? null,
          mem: sample?.mem ?? null,
          netRx: sample?.netRx ?? null,
          netTx: sample?.netTx ?? null,
          blkRead: sample?.blkRead ?? null,
          blkWrite: sample?.blkWrite ?? null,
          noData: sample?.noData ?? false,
          trend: s.trends.get(task.id) ?? [],
        };
      }),
    [s.tasks, s.round, s.trends],
  );

  if (s.tasks !== null && s.tasks.length === 0) {
    return (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Activity aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("stats.serviceEmpty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("stats.serviceEmpty.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }

  return (
    <StatsLayout
      name={name}
      status={s.status}
      closedBy={s.closedBy}
      points={s.points}
      limit={limitTotal}
      answered={s.round !== null}
      memoryLine={(mem) =>
        limitTotal && mem != null
          ? t("stats.tiles.memoryLimitTotal", {
              percent: percentOf(mem, limitTotal),
              size: formatBytes(limitTotal),
            })
          : t("stats.tiles.memoryNoSharedLimit")
      }
      notices={
        silent > 0 ? (
          <Alert variant="warning">
            <AlertDescription>{t("stats.partial", { count: silent })}</AlertDescription>
          </Alert>
        ) : null
      }
      footer={rows.length > 0 ? <TaskStatsTable serviceName={name} rows={rows} /> : null}
    />
  );
}
