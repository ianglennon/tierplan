// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";

import { containerDetailQuery } from "@/api/container-detail";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { CONTAINER_TABS } from "@/features/container-detail/tabs";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useDocumentTitle } from "@/hooks/use-me";
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

// DetailLayout is the frame of a container's page: the breadcrumb, the header with
// the name and the state, the tab bar (Link triggers, the active tab follows the
// URL) and the tab under it. The tabs render at once, before the container has
// loaded (UI-SPEC "Loading").
export function DetailLayout({ envId, containerId, node }: DetailLayoutProps) {
  const t = useT();
  const isSwarm = useEnvIsSwarm(envId);
  const { data } = useQuery(containerDetailQuery(envId, containerId, node));
  useDocumentTitle(data?.name);

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? (
          <div className="flex flex-wrap items-center gap-2">
            <Tooltip>
              <TooltipTrigger asChild>
                <h2 className="min-w-0 max-w-full truncate text-2xl font-semibold">{data.name}</h2>
              </TooltipTrigger>
              <TooltipContent className="break-all">{data.name}</TooltipContent>
            </Tooltip>
            <ContainerStateCell state={data.state} health={data.health} />
            {isSwarm && data.node ? (
              <Badge variant="outline" className="shrink-0">
                {data.node}
              </Badge>
            ) : null}
            {data.selfKind === "server" || data.selfKind === "agent" ? (
              <SelfContainerBadge kind={data.selfKind} />
            ) : null}
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("containerdetail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {CONTAINER_TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to="/env/$envId/containers/$containerId"
                params={{ envId, containerId }}
                search={{ node }}
                activeOptions={{ exact: tab.exact }}
                activeProps={{ "data-active": true }}
                inactiveProps={{ "data-active": undefined }}
              >
                {t(tab.labelKey)}
              </Link>
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      <Outlet />
    </div>
  );
}
