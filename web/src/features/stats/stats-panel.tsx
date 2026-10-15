// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Activity } from "lucide-react";
import { lazy, Suspense, useEffect, useMemo, useState } from "react";

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
import { StatTile } from "@/features/stats/stat-tile";
import {
  createStatsStream,
  type StatsCloseReason,
  type StatsReady,
  type StatsStatus,
} from "@/features/stats/stats-stream";
import { isGap, useStatsHistory } from "@/features/stats/use-stats-history";
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

  const real = useMemo(() => points.filter((p) => !isGap(p)), [points]);
  const latest = real[real.length - 1];

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

  const waiting = latest === undefined;
  const limit = ready?.limitBytes ?? null;
  const cpu = points.map((p) => p.cpu);
  const memLine =
    limit && latest?.mem != null
      ? t("stats.tiles.memoryLimit", {
          percent: percentOf(latest.mem, limit),
          size: formatBytes(limit),
        })
      : t("stats.tiles.memoryNoLimit");

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
          subline={memLine}
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
    </section>
  );
}
