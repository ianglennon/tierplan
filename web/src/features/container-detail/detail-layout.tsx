// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { Box, Info } from "lucide-react";

import { type ContainerDetail, containerDetailQuery, isGone } from "@/api/container-detail";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { HeaderActions } from "@/features/container-detail/header-actions";
import { CONTAINER_TABS, type ContainerTab } from "@/features/container-detail/tabs";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { RestartLoopBadge } from "@/features/restart-loop/restart-loop-badge";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface DetailLayoutProps {
  envId: string;
  containerId: string;
  node?: string;
}

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("containerdetail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/containers"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("containerdetail.breadcrumb.containers")}
          </Link>
        </li>
        <li className="min-w-0">
          {name === undefined ? (
            <Skeleton className="h-3 w-24" />
          ) : (
            <span aria-current="page" className="break-all">
              {name}
            </span>
          )}
        </li>
      </ol>
    </nav>
  );
}

// The line under the name says what the container belongs to, as plain text: the
// stack and project pages come with Phases 6 and 7. A stack wins over a project,
// and either wins over a bare service.
function belongsTo(
  t: (key: string, options?: Record<string, unknown>) => string,
  d: ContainerDetail,
) {
  if (d.stack) return t("containerdetail.header.stack", { name: d.stack });
  if (d.project) return t("containerdetail.header.project", { name: d.project });
  if (d.service) return t("containerdetail.header.task", { service: d.service });
  return "";
}

// Gone is what a container's page becomes when the container no longer exists: it
// was removed, or recreated under a new ID. It replaces the page, tabs included,
// since there is nothing left for any of them to show.
function Gone({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <div className="flex flex-col gap-6">
      <header>
        <Breadcrumb envId={envId} name={name} />
      </header>
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <Box aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("containerdetail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("containerdetail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/containers" params={{ envId }}>
              {t("containerdetail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

// DetailLayout is the frame of a container's page: the breadcrumb, the header with
// the name, the state, the badges and the actions, the swarm-task banner, the tab
// bar (Link triggers, the active tab follows the URL) and the tab under it. The
// tabs render at once, before the container has loaded (UI-SPEC "Loading").
export function DetailLayout({ envId, containerId, node }: DetailLayoutProps) {
  const t = useT();
  const isSwarm = useEnvIsSwarm(envId);
  const shellNeeds = useNeeds("containers.exec", envId);
  const query = useQuery(containerDetailQuery(envId, containerId, node));
  const { data } = query;
  const gone = isGone(query.error);
  useDocumentTitle(gone ? t("containerdetail.gone.heading") : data?.name);

  if (gone) return <Gone envId={envId} name={data?.name} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={containerId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("containerdetail.error.loadFailed", { name: containerId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("containerdetail.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  // A tab never hides: when it cannot be used it is shown unavailable, with why. The
  // Shell tab needs the container shell permission, and is never for Coaming's own server
  // or an agent's container (D-36).
  const unavailableReason = (tab: ContainerTab): string | undefined => {
    if (tab.needs !== "containers.exec") return undefined;
    if (!shellNeeds.allowed) return shellNeeds.reason;
    if (data?.selfKind) return t("terminal.refusal.ownContainer");
    return undefined;
  };

  const line = data ? belongsTo(t, data) : "";
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? (
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex min-w-0 flex-col gap-1">
              <div className="flex flex-wrap items-center gap-2">
                <Tooltip>
                  <TooltipTrigger asChild>
                    <h2 className="max-w-full min-w-0 truncate text-2xl font-semibold">
                      {data.name}
                    </h2>
                  </TooltipTrigger>
                  <TooltipContent className="break-all">{data.name}</TooltipContent>
                </Tooltip>
                <ContainerStateCell state={data.state} health={data.health} />
                <RestartLoopBadge loop={data.restartLoop} muted={data.muted} kind="container" />
                {isSwarm && data.node ? (
                  <Badge variant="outline" className="shrink-0">
                    {data.node}
                  </Badge>
                ) : null}
                {data.selfKind === "server" || data.selfKind === "agent" ? (
                  <SelfContainerBadge kind={data.selfKind} />
                ) : null}
              </div>
              {line ? <p className="text-sm break-words text-muted-foreground">{line}</p> : null}
            </div>
            <HeaderActions envId={envId} detail={data} />
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {data?.service ? (
        <Alert>
          <Info aria-hidden="true" />
          <AlertDescription>
            {t("containerdetail.banner.task", { service: data.service })}
          </AlertDescription>
        </Alert>
      ) : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("containerdetail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {CONTAINER_TABS.map((tab) => (
            <NeedsTooltip key={tab.id} reason={unavailableReason(tab as ContainerTab)}>
              <TabsTrigger
                value={tab.id}
                asChild
                className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
              >
                <Link
                  to={tab.to}
                  params={{ envId, containerId }}
                  search={{ node }}
                  activeOptions={{ exact: tab.exact }}
                  activeProps={{ "data-active": true }}
                  inactiveProps={{ "data-active": undefined }}
                >
                  {t(tab.labelKey)}
                </Link>
              </TabsTrigger>
            </NeedsTooltip>
          ))}
        </TabsList>
      </Tabs>
      <Outlet />
    </div>
  );
}
