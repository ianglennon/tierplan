// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { lazy, Suspense } from "react";

import { configDetailQuery } from "@/api/configs";
import { CopyableCode } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { formatSize } from "@/features/secrets/value-source-field";
import { useT } from "@/i18n/use-t";

// The Content card holds CodeMirror, which the list pages and the other tabs do not need, so it
// loads when a config's Overview is opened (UI-SPEC "Libraries").
const ConfigContentCard = lazy(() =>
  import("@/features/configs/content-card").then((m) => ({ default: m.ConfigContentCard })),
);

interface ConfigOverviewProps {
  envId: string;
  configId: string;
}

const key = "configs.detail.overview";

// The cards while the config is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="flex flex-col gap-4" aria-busy="true">
      <span role="status" className="sr-only">
        {t(`${key}.loading`)}
      </span>
      <div className="grid gap-4 lg:grid-cols-2">
        <Skeleton className="h-64 w-full" />
        <Skeleton className="h-32 w-full" />
      </div>
      <Skeleton className="h-[480px] w-full" />
    </div>
  );
}

// ContentSkeleton is the viewer's block while the viewer itself loads.
function ContentSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("configs.detail.content.loading")}
      </span>
      <Skeleton className="h-[480px] w-full" />
    </div>
  );
}

// ConfigOverview is the first tab of a config: its facts in cards, two columns above 1024px, and
// under them the full-width Content card. The page frame owns the load error, the gone state and
// the in-use callout, so this draws only what a loaded config has.
export function ConfigOverview({ envId, configId }: ConfigOverviewProps) {
  const t = useT();
  const { data } = useQuery(configDetailQuery(envId, configId));
  if (!data) return <OverviewSkeleton />;
  const labels = Object.entries(data.labels ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  const missing = t("configs.detail.missing");
  return (
    <div className="flex flex-col gap-4">
      <div className="grid items-start gap-4 lg:grid-cols-2">
        <div className="flex min-w-0 flex-col gap-4">
          <SettingsCard title={t(`${key}.details.title`)}>
            <KeyValueTable
              mono={false}
              label={t(`${key}.details.title`)}
              empty=""
              rows={[
                {
                  key: t(`${key}.details.id`),
                  value: <CopyableCode code={data.id} label={t(`${key}.details.idLabel`)} />,
                },
                {
                  key: t(`${key}.details.name`),
                  value: <span className="font-mono text-xs break-all">{data.name}</span>,
                },
                {
                  key: t(`${key}.details.created`),
                  value: <RelativeTime value={data.createdAt} />,
                },
                {
                  key: t(`${key}.details.updated`),
                  value: <RelativeTime value={data.updatedAt} />,
                },
                {
                  key: t(`${key}.details.owner`),
                  value: data.owner ? t("configs.detail.owner", { name: data.owner }) : missing,
                },
                {
                  key: t(`${key}.details.templating`),
                  value: t(`${key}.templating.${data.templating}`),
                },
                {
                  key: t(`${key}.details.size`),
                  value: formatSize(data.size),
                },
              ]}
            />
          </SettingsCard>
        </div>
        <div className="flex min-w-0 flex-col gap-4">
          <SettingsCard title={t(`${key}.labels.title`)}>
            <KeyValueTable
              label={t(`${key}.labels.label`)}
              empty={t(`${key}.labels.empty`)}
              rows={labels.map(([name, value]) => ({ key: name, value }))}
            />
          </SettingsCard>
        </div>
      </div>
      <Suspense fallback={<ContentSkeleton />}>
        <ConfigContentCard name={data.name} data={data.data} text={data.text} />
      </Suspense>
    </div>
  );
}
