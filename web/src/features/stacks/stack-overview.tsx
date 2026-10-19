// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Rocket } from "lucide-react";

import { stackQuery } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";

interface StackOverviewProps {
  envId: string;
  stackId: string;
}

// StackOverview is the Overview tab: for a stack that was never deployed, the "Not deployed yet"
// state; for any other, the details of the stack. Later plans add the services table beside it.
// Every value renders as text.
export function StackOverview({ envId, stackId }: StackOverviewProps) {
  const t = useT();
  const envName = useEnvName(envId);
  const { data } = useQuery(stackQuery(envId, stackId));
  if (!data) return <Skeleton className="h-32 w-full" />;
  if (data.state === "not_deployed") {
    return (
      <Empty className="border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Rocket aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("stacks.detail.overview.notDeployed.title")}
          </EmptyTitle>
          <EmptyDescription>
            {t("stacks.detail.overview.notDeployed.body", { name: data.name })}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  }
  return (
    <section aria-labelledby="stack-details" className="rounded-xl p-4 ring-1 ring-foreground/10">
      <h3 id="stack-details" className="mb-3 text-base font-medium">
        {t("stacks.detail.overview.details.title")}
      </h3>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        <dt className="text-muted-foreground">{t("stacks.detail.overview.details.environment")}</dt>
        <dd>
          <Link
            to="/env/$envId/stacks"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {envName}
          </Link>
        </dd>
        <dt className="text-muted-foreground">{t("stacks.detail.overview.details.target")}</dt>
        <dd>{t(`stacks.target.${data.target}`)}</dd>
        <dt className="text-muted-foreground">{t("stacks.detail.overview.details.source")}</dt>
        <dd>{t(`stacks.source.${data.source}`)}</dd>
        <dt className="text-muted-foreground">{t("stacks.detail.overview.details.created")}</dt>
        <dd>
          <RelativeTime value={data.createdAt} />{" "}
          {t("stacks.list.lastDeployedBy", { user: data.createdBy })}
        </dd>
        <dt className="text-muted-foreground">
          {t("stacks.detail.overview.details.lastDeployed")}
        </dt>
        <dd>
          {data.lastDeployedAt ? (
            <>
              <RelativeTime value={data.lastDeployedAt} />{" "}
              {t("stacks.list.lastDeployedBy", { user: data.lastDeployedBy })}
            </>
          ) : (
            t("stacks.list.never")
          )}
        </dd>
        <dt className="text-muted-foreground">{t("stacks.detail.overview.details.id")}</dt>
        <dd className="font-mono text-xs break-all">{data.id}</dd>
      </dl>
    </section>
  );
}
