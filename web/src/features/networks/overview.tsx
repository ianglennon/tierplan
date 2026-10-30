// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";

import { type NetworkDetail, networkDetailQuery } from "@/api/networks";
import { CopyableCode } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

interface NetworkOverviewProps {
  envId: string;
  networkId: string;
  // The node a local network is on, from the page's address.
  node?: string;
}

const key = "networks.detail.overview";

// DetailsCard is the network's scalar facts. The ID is mono with a copy button.
function DetailsCard({ detail }: { detail: NetworkDetail }) {
  const t = useT();
  const yesNo = (v: boolean) => t(v ? `${key}.details.yes` : `${key}.details.no`);
  const scope =
    detail.scope === "swarm"
      ? t("networks.list.scope.swarm")
      : detail.scope === "local"
        ? t("networks.list.scope.local")
        : detail.scope;
  const owner = detail.owner
    ? t(
        detail.ownerKind === "project"
          ? "networks.detail.owner.project"
          : "networks.detail.owner.stack",
        { name: detail.owner },
      )
    : t("networks.detail.missing");
  return (
    <SettingsCard title={t(`${key}.details.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.details.title`)}
        empty=""
        rows={[
          {
            key: t(`${key}.details.id`),
            value: <CopyableCode code={detail.id} label={t(`${key}.details.idLabel`)} />,
          },
          { key: t(`${key}.details.driver`), value: detail.driver },
          { key: t(`${key}.details.scope`), value: scope },
          { key: t(`${key}.details.ipv6`), value: yesNo(detail.ipv6) },
          { key: t(`${key}.details.internal`), value: yesNo(detail.internal) },
          { key: t(`${key}.details.attachable`), value: yesNo(detail.attachable) },
          { key: t(`${key}.details.created`), value: <RelativeTime value={detail.created} /> },
          { key: t(`${key}.details.owner`), value: owner },
        ]}
      />
    </SettingsCard>
  );
}

// PoolsCard is the address pools: the subnet, its gateway and the range containers
// are given addresses from. A part the network does not set shows a dash.
function PoolsCard({ detail }: { detail: NetworkDetail }) {
  const t = useT();
  const missing = t("networks.detail.missing");
  const pools = detail.ipam ?? [];
  return (
    <SettingsCard title={t(`${key}.pools.title`)}>
      {pools.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.pools.empty`)}</p>
      ) : (
        <Table aria-label={t(`${key}.pools.title`)}>
          <TableHeader>
            <TableRow>
              <TableHead>{t(`${key}.pools.subnet`)}</TableHead>
              <TableHead>{t(`${key}.pools.gateway`)}</TableHead>
              <TableHead>{t(`${key}.pools.ipRange`)}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pools.map((p) => (
              <TableRow key={`${p.subnet}|${p.gateway}|${p.ipRange}`}>
                {[p.subnet, p.gateway, p.ipRange].map((v, i) => (
                  <TableCell
                    // biome-ignore lint/suspicious/noArrayIndexKey: three fixed columns
                    key={i}
                    className="align-top font-mono text-xs break-all whitespace-normal"
                  >
                    {v || missing}
                  </TableCell>
                ))}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </SettingsCard>
  );
}

// MapCard lists a map of text from Docker, sorted by name, wrapping at any character.
function MapCard({
  title,
  label,
  empty,
  values,
}: {
  title: string;
  label: string;
  empty: string;
  values: Record<string, string> | undefined;
}) {
  const rows = Object.entries(values ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return (
    <SettingsCard title={title}>
      <KeyValueTable
        label={label}
        empty={empty}
        rows={rows.map(([name, value]) => ({ key: name, value }))}
      />
    </SettingsCard>
  );
}

// The cards while the network is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="grid gap-4 lg:grid-cols-2" aria-busy="true">
      <span role="status" className="sr-only">
        {t(`${key}.loading`)}
      </span>
      <Skeleton className="h-64 w-full" />
      <div className="flex flex-col gap-4">
        <Skeleton className="h-32 w-full" />
        <Skeleton className="h-32 w-full" />
      </div>
    </div>
  );
}

// NetworkOverview is the first tab of a network: its facts in cards, two columns above
// 1024px. The page frame owns the load error, the gone state and the in-use callout, so
// this draws only what a loaded network has.
export function NetworkOverview({ envId, networkId, node }: NetworkOverviewProps) {
  const t = useT();
  const { data } = useQuery(networkDetailQuery(envId, networkId, node));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="grid items-start gap-4 lg:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-4">
        <DetailsCard detail={data} />
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        <PoolsCard detail={data} />
        <MapCard
          title={t(`${key}.options.title`)}
          label={t(`${key}.options.label`)}
          empty={t(`${key}.options.empty`)}
          values={data.options}
        />
        <MapCard
          title={t(`${key}.labels.title`)}
          label={t(`${key}.labels.label`)}
          empty={t(`${key}.labels.empty`)}
          values={data.labels}
        />
      </div>
    </div>
  );
}
