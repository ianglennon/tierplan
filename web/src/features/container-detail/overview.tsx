// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";

import { type ContainerDetail, containerDetailQuery } from "@/api/container-detail";
import { KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

interface OverviewProps {
  envId: string;
  containerId: string;
  // The node the container is on, from the page's address.
  node?: string;
}

// StateCard is the State rows: where the container is in its life.
function StateCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  return (
    <SettingsCard title={t("containerdetail.state.title")}>
      <KeyValueTable
        mono={false}
        label={t("containerdetail.state.title")}
        empty=""
        rows={[
          {
            key: t("containerdetail.state.status"),
            value: t(`dashboard.lists.states.${detail.state}`),
          },
          {
            key: t("containerdetail.state.health"),
            value: t(`containerdetail.health.${detail.health}`),
          },
          {
            key: t("containerdetail.state.started"),
            value: <RelativeTime value={detail.startedAt} />,
          },
          {
            key: t("containerdetail.state.finished"),
            value: <RelativeTime value={detail.finishedAt} />,
          },
          { key: t("containerdetail.state.exitCode"), value: String(detail.exitCode) },
          { key: t("containerdetail.state.restarts"), value: String(detail.restartCount) },
          {
            key: t("containerdetail.state.oomKilled"),
            value: t(detail.oomKilled ? "containerdetail.state.yes" : "containerdetail.state.no"),
          },
        ]}
      />
    </SettingsCard>
  );
}

// EnvCard lists the variables by name. A value comes only when the server sent it,
// because the caller holds secrets.reveal; it is still hidden until the person asks
// to see it. Without the permission there is no value in the page at all.
function EnvCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  const env = detail.env ?? [];
  const anyMasked = env.some((v) => v.masked);
  return (
    <SettingsCard title={t("containerdetail.env.title")}>
      <KeyValueTable
        label={t("containerdetail.env.label")}
        empty={t("containerdetail.env.empty")}
        rows={env.map((v) =>
          v.masked || v.value === undefined
            ? { key: v.name, masked: true as const }
            : { key: v.name, masked: true as const, canReveal: true as const, value: v.value },
        )}
      />
      {anyMasked ? (
        <p className="text-sm text-muted-foreground">{t("containerdetail.env.masked")}</p>
      ) : null}
    </SettingsCard>
  );
}

// The cards while the container is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="grid gap-4 lg:grid-cols-2" aria-busy="true">
      <span role="status" className="sr-only">
        {t("containerdetail.loading")}
      </span>
      <Skeleton className="h-56 w-full" />
      <Skeleton className="h-56 w-full" />
    </div>
  );
}

// Overview is the first tab of a container: everything about it in cards, two
// columns above 1024px (D-04). The page frame owns the load error and the gone
// state, so this draws only what a loaded container has.
export function Overview({ envId, containerId, node }: OverviewProps) {
  const { data } = useQuery(containerDetailQuery(envId, containerId, node));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="grid items-start gap-4 lg:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-4">
        <StateCard detail={data} />
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        <EnvCard detail={data} />
      </div>
    </div>
  );
}
