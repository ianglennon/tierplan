// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { environmentsQuery, summaryQuery } from "@/api/environments";
import { CountTile } from "@/components/count-tile";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { EngineCard } from "@/features/dashboard/engine-card";
import { EventsFeed } from "@/features/dashboard/events-feed";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Dashboard is the environment's home (D-55): header, the worker notice on a
// swarm worker, the count tiles, the engine card and the events feed. It reads
// the same ["env", envId, "summary"] query as the layout, so there is one request; the
// layout has already turned offline, unsupported, missing and forbidden
// environments into full-page states, so what reaches here is data, a load in
// progress, or a failure that is not one of those.
function Dashboard() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("dashboard.tabTitle"));
  const healthLabel = useHealthLabel();
  const summary = useQuery(summaryQuery(envId));
  const environments = useQuery(environmentsQuery);
  // A tile links to its list only for a user who may read it, so no tile leads
  // to a "no permission" page (the list routes also check, and the server
  // answers 403, D-48).
  const canContainers = usePermission("containers.read", envId);
  const canImages = usePermission("images.read", envId);
  const canVolumes = usePermission("volumes.read", envId);
  const canNetworks = usePermission("networks.read", envId);
  const params = { envId };

  const data = summary.data;
  const listed = environments.data?.items?.find((e) => e.id === envId);
  const loading = summary.isPending;
  const failed = summary.isError;
  const counts = data?.counts;
  const status = data?.environment.status ?? listed?.status;
  // D-27: the badge needs both the server's answer that this is Bayplan's own
  // host and the container ID it found.
  const isSelf = Boolean(listed?.isSelfHost && data?.selfContainerId);
  const name = data?.environment.name ?? listed?.name;

  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-8">
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <h1 className="min-w-0 text-2xl font-semibold break-words line-clamp-2" title={name}>
          {name}
        </h1>
        <TypeBadge
          envType={listed?.envType}
          type={data?.environment.type ?? listed?.type}
          isLocalSocket={listed?.isLocalSocket}
        />
        {status ? <StatusDot state={status.state} label={healthLabel(status)} /> : null}
        {isSelf ? <Badge variant="outline">{t("dashboard.selfBadge")}</Badge> : null}
      </header>

      {data?.isWorker ? (
        <Alert>
          <AlertDescription>{t("dashboard.workerNotice")}</AlertDescription>
        </Alert>
      ) : null}

      {failed ? (
        <Alert variant="destructive">
          <AlertTitle>{t("dashboard.error.heading")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("dashboard.error.body")}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={summary.isFetching}
              onClick={() => void summary.refetch()}
            >
              {summary.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("dashboard.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}

      <section
        aria-label={t("dashboard.tabTitle")}
        className="grid grid-cols-[repeat(auto-fill,minmax(160px,1fr))] gap-4"
      >
        <CountTile
          label={t("dashboard.tiles.containers")}
          value={counts?.containers}
          subline={
            counts
              ? t("dashboard.tiles.containersSubline", {
                  running: counts.containersRunning,
                  stopped: counts.containersStopped,
                })
              : undefined
          }
          to={canContainers ? "/env/$envId/containers" : undefined}
          params={params}
          loading={loading}
          error={failed}
        />
        <CountTile
          label={t("dashboard.tiles.images")}
          value={counts?.images}
          to={canImages ? "/env/$envId/images" : undefined}
          params={params}
          loading={loading}
          error={failed}
        />
        <CountTile
          label={t("dashboard.tiles.volumes")}
          value={counts?.volumes}
          to={canVolumes ? "/env/$envId/volumes" : undefined}
          params={params}
          loading={loading}
          error={failed}
        />
        <CountTile
          label={t("dashboard.tiles.networks")}
          value={counts?.networks}
          to={canNetworks ? "/env/$envId/networks" : undefined}
          params={params}
          loading={loading}
          error={failed}
        />
        {counts?.nodes !== undefined ? (
          <CountTile label={t("dashboard.tiles.nodes")} value={counts.nodes} />
        ) : null}
        {counts?.services !== undefined ? (
          <CountTile label={t("dashboard.tiles.services")} value={counts.services} />
        ) : null}
      </section>

      <div className="grid gap-6 lg:grid-cols-[1fr_2fr]">
        <EngineCard engine={data?.engine} loading={loading} />
        <EventsFeed />
      </div>
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/")({
  component: Dashboard,
});
