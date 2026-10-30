// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Info } from "lucide-react";

import { secretDetailQuery } from "@/api/secrets";
import { CopyableCode } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

interface SecretOverviewProps {
  envId: string;
  secretId: string;
}

const key = "secrets.detail.overview";

// The cards while the secret is read, in the layout they take afterwards.
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

// SecretOverview is the first tab of a secret: its facts in cards, two columns above 1024px, and
// the Value card, which says there is nothing to show and why. The page frame owns the load error,
// the gone state and the in-use callout, so this draws only what a loaded secret has.
export function SecretOverview({ envId, secretId }: SecretOverviewProps) {
  const t = useT();
  const { data } = useQuery(secretDetailQuery(envId, secretId));
  if (!data) return <OverviewSkeleton />;
  const labels = Object.entries(data.labels ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  const missing = t("secrets.detail.missing");
  return (
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
                value: data.owner ? t("secrets.detail.owner", { name: data.owner }) : missing,
              },
            ]}
          />
        </SettingsCard>
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        <SettingsCard title={t(`${key}.value.title`)}>
          <Alert>
            <Info aria-hidden="true" />
            <AlertDescription>{t(`${key}.value.body`)}</AlertDescription>
          </Alert>
        </SettingsCard>
        <SettingsCard title={t(`${key}.labels.title`)}>
          <KeyValueTable
            label={t(`${key}.labels.label`)}
            empty={t(`${key}.labels.empty`)}
            rows={labels.map(([name, value]) => ({ key: name, value }))}
          />
        </SettingsCard>
      </div>
    </div>
  );
}
