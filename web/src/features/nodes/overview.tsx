// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";

import { type NodeDetail, nodeQuery } from "@/api/nodes";
import { CopyableCode } from "@/components/copyable-code";
import { type KeyValueRow, KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { archName, formatCpus, nodeStateKey, nodeTone } from "@/features/nodes/node-facts";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

interface NodeOverviewProps {
  envId: string;
  nodeId: string;
}

const key = "nodes.overview";

type Translate = ReturnType<typeof useT>;

function NodeCard({ detail, t, missing }: { detail: NodeDetail; t: Translate; missing: string }) {
  const availability =
    detail.availability === "pause"
      ? t("nodes.availability.pause")
      : detail.availability === "drain"
        ? t("nodes.availability.drain")
        : t("nodes.availability.active");
  const rows: KeyValueRow[] = [
    {
      key: t(`${key}.node.id`),
      value: <CopyableCode code={detail.id} label={t(`${key}.node.idLabel`)} />,
    },
    { key: t(`${key}.node.hostname`), value: detail.hostname || missing },
    { key: t(`${key}.node.address`), value: detail.address || missing },
    {
      key: t(`${key}.node.role`),
      value: detail.role === "manager" ? t("nodes.role.manager") : t("nodes.role.worker"),
    },
    {
      key: t(`${key}.node.status`),
      value: (
        <StatusDot
          state={nodeTone(detail.state)}
          label={t(`nodes.state.${nodeStateKey(detail.state)}`)}
        />
      ),
    },
    { key: t(`${key}.node.availability`), value: availability },
    { key: t(`${key}.node.created`), value: <RelativeTime value={detail.createdAt} /> },
    { key: t(`${key}.node.updated`), value: <RelativeTime value={detail.updatedAt} /> },
  ];
  return (
    <SettingsCard title={t(`${key}.node.title`)}>
      <KeyValueTable mono={false} label={t(`${key}.node.title`)} empty="" rows={rows} />
    </SettingsCard>
  );
}

function SystemCard({ detail, t, missing }: { detail: NodeDetail; t: Translate; missing: string }) {
  const rows: KeyValueRow[] = [
    { key: t(`${key}.system.engine`), value: detail.engineVersion || missing },
    { key: t(`${key}.system.os`), value: detail.os || missing },
    { key: t(`${key}.system.arch`), value: detail.arch ? archName(detail.arch) : missing },
    { key: t(`${key}.system.cpus`), value: formatCpus(detail.cpus) || missing },
    {
      key: t(`${key}.system.memory`),
      value: detail.memoryBytes > 0 ? formatBytes(detail.memoryBytes) : missing,
    },
  ];
  return (
    <SettingsCard title={t(`${key}.system.title`)}>
      <KeyValueTable mono={false} label={t(`${key}.system.title`)} empty="" rows={rows} />
    </SettingsCard>
  );
}

// ManagerCard is for managers only: whether the other managers reach it, whether it leads, and the
// address it answers on for the swarm.
function ManagerCard({
  detail,
  t,
  missing,
}: {
  detail: NodeDetail;
  t: Translate;
  missing: string;
}) {
  const reachability =
    detail.reachability === "reachable"
      ? t(`${key}.manager.reachable`)
      : detail.reachability === "unreachable"
        ? t(`${key}.manager.unreachable`)
        : t(`${key}.manager.unknown`);
  const rows: KeyValueRow[] = [
    {
      key: t(`${key}.manager.reachability`),
      value: (
        <StatusDot
          state={
            detail.reachability === "reachable"
              ? "online"
              : detail.reachability === "unreachable"
                ? "offline"
                : "unknown"
          }
          label={reachability}
        />
      ),
    },
    { key: t(`${key}.manager.leader`), value: detail.leader ? t(`${key}.yes`) : t(`${key}.no`) },
    { key: t(`${key}.manager.address`), value: detail.managerAddr || missing },
  ];
  return (
    <SettingsCard title={t(`${key}.manager.title`)}>
      <KeyValueTable mono={false} label={t(`${key}.manager.title`)} empty="" rows={rows} />
    </SettingsCard>
  );
}

function AgentCard({ detail, t, missing }: { detail: NodeDetail; t: Translate; missing: string }) {
  const rows: KeyValueRow[] = [
    {
      key: t(`${key}.agent.reporting`),
      value: detail.agent.reporting ? t(`${key}.yes`) : t(`${key}.no`),
    },
    { key: t(`${key}.agent.version`), value: detail.agent.version || missing },
    { key: t(`${key}.agent.lastSeen`), value: <RelativeTime value={detail.agent.lastSeen} /> },
  ];
  return (
    <SettingsCard title={t(`${key}.agent.title`)}>
      <KeyValueTable mono={false} label={t(`${key}.agent.title`)} empty="" rows={rows} />
    </SettingsCard>
  );
}

// EngineLabelsCard shows the labels set in the node's own Docker daemon settings. TierPlan cannot
// change them, so the card says where they are set. Keys and values are Docker's text.
function EngineLabelsCard({ detail, t }: { detail: NodeDetail; t: Translate }) {
  const rows: KeyValueRow[] = Object.entries(detail.engineLabels)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([k, value]) => ({ key: k, value }));
  return (
    <SettingsCard title={t(`${key}.engineLabels.title`)}>
      <p className="text-sm text-muted-foreground">{t(`${key}.engineLabels.caption`)}</p>
      <KeyValueTable
        label={t(`${key}.engineLabels.title`)}
        empty={t(`${key}.engineLabels.empty`)}
        rows={rows}
      />
    </SettingsCard>
  );
}

function OverviewSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true" className="grid items-start gap-4 lg:grid-cols-2">
      <span role="status" className="sr-only">
        {t("nodes.detail.loading")}
      </span>
      <Skeleton className="h-64 w-full" />
      <Skeleton className="h-64 w-full" />
    </div>
  );
}

// NodeOverview is the first tab of a node's page (UI-SPEC "Nodes"): the node's facts, its system,
// for a manager its manager status, the TierPlan agent on it and its engine labels. Everything is
// text from the swarm and the daemon (T-06-41).
export function NodeOverview({ envId, nodeId }: NodeOverviewProps) {
  const t = useT();
  const { data } = useQuery(nodeQuery(envId, nodeId));
  const missing = t("dashboard.time.missing");
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="grid items-start gap-4 lg:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-4">
        <NodeCard detail={data} t={t} missing={missing} />
        <SystemCard detail={data} t={t} missing={missing} />
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        {data.role === "manager" ? <ManagerCard detail={data} t={t} missing={missing} /> : null}
        <AgentCard detail={data} t={t} missing={missing} />
        <EngineLabelsCard detail={data} t={t} />
      </div>
    </div>
  );
}
