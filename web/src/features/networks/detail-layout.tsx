// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { Info, Network } from "lucide-react";
import { useState } from "react";

import { isNetworkGone, type NetworkDetail, networkDetailQuery } from "@/api/networks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { UsedByList } from "@/components/used-by-list";
import { DeleteNetworkDialog } from "@/features/networks/delete-network-dialog";
import { deleteReason } from "@/features/networks/delete-reason";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface NetworkDetailLayoutProps {
  envId: string;
  networkId: string;
  // The node a local network is on, from the page's address.
  node?: string;
}

// The tabs of the page, in the order they show. Each is a child route of the page
// under networks_.$networkId.*; the Overview is the page's own address.
const TABS = [
  {
    id: "overview",
    to: "/env/$envId/networks/$networkId",
    labelKey: "networks.detail.tabs.overview",
    exact: true,
  },
  {
    id: "attachments",
    to: "/env/$envId/networks/$networkId/attachments",
    labelKey: "networks.detail.tabs.attachments",
    exact: false,
  },
  {
    id: "inspect",
    to: "/env/$envId/networks/$networkId/inspect",
    labelKey: "networks.detail.tabs.inspect",
    exact: false,
  },
] as const;

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("networks.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/networks"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("networks.detail.breadcrumb.networks")}
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

// Gone is what a network's page becomes when the network no longer exists: it was
// removed, or a deploy replaced it. It replaces the page, tabs included.
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
            <Network aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("networks.detail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("networks.detail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/networks" params={{ envId }}>
              {t("networks.detail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

// What the line under the name says: who owns the network, as plain text until the
// stack and project pages exist. A stack wins over a project.
function ownerLine(
  t: (key: string, options?: Record<string, unknown>) => string,
  d: NetworkDetail,
): string {
  if (!d.owner) return "";
  return t(
    d.ownerKind === "project" ? "networks.detail.owner.project" : "networks.detail.owner.stack",
    {
      name: d.owner,
    },
  );
}

// NetworkDetailLayout is the frame of a network's page: the breadcrumb, the header
// with the name, the driver and scope badges and the owner, the tab bar (Link
// triggers, the active tab follows the URL) and the tab under it. The tabs render at
// once, before the network has loaded (UI-SPEC "Loading").
export function NetworkDetailLayout({ envId, networkId, node }: NetworkDetailLayoutProps) {
  const t = useT();
  const isSwarm = useEnvIsSwarm(envId);
  const manage = useNeeds("networks.manage", envId);
  const [deleting, setDeleting] = useState(false);
  const query = useQuery(networkDetailQuery(envId, networkId, node));
  const { data } = query;
  const gone = isNetworkGone(query.error);
  useDocumentTitle(gone ? t("networks.detail.gone.heading") : data?.name);

  if (gone) return <Gone envId={envId} name={data?.name} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={networkId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("networks.detail.error.loadFailed", { name: networkId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("networks.detail.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const line = data ? ownerLine(t, data) : "";
  const reason = data ? deleteReason(t, manage.reason, data, true) : undefined;
  const attachments = data?.attachments ?? [];
  const inUse = data !== undefined && !data.builtIn && attachments.length > 0;
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
                <Badge variant="outline" className="shrink-0">
                  {data.driver}
                </Badge>
                <Badge variant="outline" className="shrink-0">
                  {data.scope === "swarm"
                    ? t("networks.list.scope.swarm")
                    : data.scope === "local"
                      ? t("networks.list.scope.local")
                      : data.scope}
                </Badge>
                {isSwarm && data.scope !== "swarm" && data.node ? (
                  <Badge variant="outline" className="shrink-0">
                    {data.node}
                  </Badge>
                ) : null}
              </div>
              {line ? <p className="text-sm break-words text-muted-foreground">{line}</p> : null}
            </div>
            <NeedsTooltip reason={reason}>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11 border-destructive-fill text-destructive"
                aria-label={t("networks.delete.label", { name: data.name })}
                onClick={reason ? undefined : () => setDeleting(true)}
              >
                {t("networks.delete.action")}
              </Button>
            </NeedsTooltip>
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {data?.builtIn ? (
        <Alert>
          <Info aria-hidden="true" />
          <AlertDescription>{t("networks.detail.builtin", { name: data.name })}</AlertDescription>
        </Alert>
      ) : null}
      {data && inUse ? (
        <Alert variant="warning">
          <AlertTitle>{t("networks.detail.inUse.title", { name: data.name })}</AlertTitle>
          <AlertDescription className="flex flex-col gap-2">
            <p>{t("networks.detail.inUse.body")}</p>
            <UsedByList
              containers={attachments
                .filter((a) => a.kind === "container")
                .map((a) => ({ name: a.name, node: a.node }))}
              services={attachments
                .filter((a) => a.kind === "service")
                .map((a) => ({ name: a.name }))}
            />
          </AlertDescription>
        </Alert>
      ) : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("networks.detail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, networkId }}
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
      {data && manage.allowed ? (
        <DeleteNetworkDialog
          envId={envId}
          network={deleting ? data : null}
          onClose={() => setDeleting(false)}
        />
      ) : null}
    </div>
  );
}
