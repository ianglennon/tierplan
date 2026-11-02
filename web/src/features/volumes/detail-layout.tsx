// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { HardDrive } from "lucide-react";
import { useState } from "react";

import { isVolumeGone, volumeDetailQuery } from "@/api/volumes";
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
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { DeleteVolumesDialog } from "@/features/volumes/delete-volumes-dialog";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface VolumeDetailLayoutProps {
  envId: string;
  volumeName: string;
  // The node the volume is on, from the page's address: a name repeats per node.
  node?: string;
}

// The tabs of the page, in the order they show. Each is a child route of the page under
// volumes_.$volumeName.*; the Overview is the page's own address.
const TABS = [
  {
    id: "overview",
    to: "/env/$envId/volumes/$volumeName",
    labelKey: "volumes.detail.tabs.overview",
    exact: true,
  },
  {
    id: "inspect",
    to: "/env/$envId/volumes/$volumeName/inspect",
    labelKey: "volumes.detail.tabs.inspect",
    exact: false,
  },
] as const;

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("volumes.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/volumes"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("volumes.detail.breadcrumb.volumes")}
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

// Gone is what a volume's page becomes when the volume no longer exists: it was removed,
// or a deploy replaced it. It replaces the page, tabs included.
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
            <HardDrive aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("volumes.detail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("volumes.detail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/volumes" params={{ envId }}>
              {t("volumes.detail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

// VolumeDetailLayout is the frame of a volume's page: the breadcrumb, the header with the
// name, the driver, node and Anonymous badges and Delete…, the in-use callout, the tab bar
// (Link triggers, the active tab follows the URL) and the tab under it. The tabs render at
// once, before the volume has loaded (UI-SPEC "Loading").
export function VolumeDetailLayout({ envId, volumeName, node }: VolumeDetailLayoutProps) {
  const t = useT();
  const navigate = useNavigate();
  const isSwarm = useEnvIsSwarm(envId);
  const remove = useNeeds("volumes.delete", envId);
  const [deleting, setDeleting] = useState(false);
  const query = useQuery(volumeDetailQuery(envId, volumeName, node));
  const { data } = query;
  const gone = isVolumeGone(query.error);
  useDocumentTitle(gone ? t("volumes.detail.gone.heading") : data?.name);

  if (gone) return <Gone envId={envId} name={data?.name} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={volumeName} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("volumes.detail.error.loadFailed", { name: volumeName })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("volumes.detail.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const usedBy = data?.usedBy ?? [];
  const inUse = usedBy.length > 0;
  // Who may comes first, then what still uses the volume.
  const reason = data
    ? remove.allowed
      ? inUse
        ? t("volumes.detail.inUse.reason", {
            what: usedByWhat(t, data.usedByContainers, data.usedByServices),
          })
        : undefined
      : (remove.reason ?? t("needs.extra.volumes.delete"))
    : undefined;
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? (
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <Tooltip>
                <TooltipTrigger asChild>
                  <h2 className="max-w-full min-w-0 truncate font-mono text-2xl font-semibold">
                    {data.name}
                  </h2>
                </TooltipTrigger>
                <TooltipContent className="break-all">{data.name}</TooltipContent>
              </Tooltip>
              <Badge variant="outline" className="shrink-0">
                {data.driver}
              </Badge>
              {isSwarm && data.node ? (
                <Badge variant="outline" className="shrink-0">
                  {data.node}
                </Badge>
              ) : null}
              {data.anonymous ? (
                <Badge variant="secondary" className="shrink-0">
                  {t("volumes.list.anonymous")}
                </Badge>
              ) : null}
            </div>
            <NeedsTooltip reason={reason}>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11 border-destructive-fill text-destructive"
                aria-label={t("volumes.delete.label", { name: data.name })}
                onClick={reason ? undefined : () => setDeleting(true)}
              >
                {t("volumes.delete.action")}
              </Button>
            </NeedsTooltip>
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
      </header>
      {data && inUse ? (
        <Alert variant="warning">
          <AlertTitle>{t("volumes.detail.inUse.title", { name: data.name })}</AlertTitle>
          <AlertDescription className="flex flex-col gap-2">
            <p>{t("volumes.detail.inUse.body")}</p>
            <UsedByList
              containers={usedBy
                .filter((u) => u.kind === "container")
                .map((u) => ({ name: u.name, node: u.node }))}
              services={usedBy.filter((u) => u.kind === "service").map((u) => ({ name: u.name }))}
            />
          </AlertDescription>
        </Alert>
      ) : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("volumes.detail.tabs.label")}
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
                params={{ envId, volumeName }}
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
      {data && remove.allowed ? (
        <DeleteVolumesDialog
          envId={envId}
          rows={deleting ? [{ name: data.name, node: data.node, nodeId: data.nodeId }] : []}
          swarm={isSwarm}
          open={deleting}
          onClose={() => setDeleting(false)}
          // The volume is gone: its page has nothing left to show, so go back to the list.
          onDone={() => void navigate({ to: "/env/$envId/volumes", params: { envId } })}
        />
      ) : null}
    </div>
  );
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// usedByWhat words what keeps a volume in use: only the kinds that are there.
function usedByWhat(t: Translate, containers: number, services: number): string {
  const parts = [
    containers > 0 ? t("volumes.list.used.containers", { count: containers }) : null,
    services > 0 ? t("volumes.list.used.services", { count: services }) : null,
  ].filter((p): p is string => p !== null);
  return parts.length === 2
    ? t("volumes.list.used.and", { first: parts[0], second: parts[1] })
    : (parts[0] ?? "");
}
