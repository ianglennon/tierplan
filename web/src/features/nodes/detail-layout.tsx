// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { Crown, Lock, Server } from "lucide-react";
import { useMemo } from "react";

import { isNodeGone, type NodeDetail, nodeQuery } from "@/api/nodes";
import { StatusDot } from "@/components/status-dot";
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
import { nodeStateKey, nodeTone } from "@/features/nodes/node-facts";
import { NodeHeaderActions, useNodeActions } from "@/features/nodes/row-actions";
import { NODE_TABS } from "@/features/nodes/tabs";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface NodeDetailLayoutProps {
  envId: string;
  nodeId: string;
}

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("nodes.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/nodes"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("nodes.detail.breadcrumb.nodes")}
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

// Gone is what a node's page becomes when the swarm no longer lists the node. It replaces the
// page, tabs included, since there is nothing left for any of them to show.
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
            <Server aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("nodes.detail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("nodes.detail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/nodes" params={{ envId }}>
              {t("nodes.detail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

function Header({ data }: { data: NodeDetail }) {
  const t = useT();
  const docker = data.engineVersion
    ? t("nodes.detail.header.docker", { version: data.engineVersion })
    : "";
  const facts = [data.address, docker].filter(Boolean).join(t("nodes.detail.header.separator"));
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div className="flex flex-wrap items-center gap-2">
        <Tooltip>
          <TooltipTrigger asChild>
            <h2 className="max-w-full min-w-0 line-clamp-2 text-2xl font-semibold break-all">
              {data.hostname}
            </h2>
          </TooltipTrigger>
          <TooltipContent className="break-all">{data.hostname}</TooltipContent>
        </Tooltip>
        <StatusDot
          state={nodeTone(data.state)}
          label={t(`nodes.state.${nodeStateKey(data.state)}`)}
        />
        <Badge variant="outline" className="shrink-0">
          {data.role === "manager" ? t("nodes.role.manager") : t("nodes.role.worker")}
        </Badge>
        {data.leader ? (
          <Badge variant="outline" className="shrink-0">
            <Crown aria-hidden="true" />
            {t("nodes.badge.leader")}
          </Badge>
        ) : null}
        {data.runsBayplan ? (
          <Badge variant="outline" className="shrink-0">
            <Lock aria-hidden="true" />
            {t("nodes.badge.runsBayplan")}
          </Badge>
        ) : null}
      </div>
      {facts ? <p className="text-sm text-muted-foreground break-all">{facts}</p> : null}
    </div>
  );
}

// NodeDetailLayout is the frame of a node's page: the breadcrumb Nodes / name, the header with the
// host name, the status and the badges, the tab bar (Link triggers, the active tab follows the URL)
// and the tab under it. The tabs render at once, before the node has loaded. A node that leaves
// the swarm while its page is open turns the page into the gone state. The header carries the
// node's availability and role changes and its removal.
export function NodeDetailLayout({ envId, nodeId }: NodeDetailLayoutProps) {
  const t = useT();
  const query = useQuery(nodeQuery(envId, nodeId));
  const { data } = query;
  const rows = useMemo(() => (data ? [data] : undefined), [data]);
  const navigate = useNavigate();
  // A node that was removed has no page left, so the page goes to the list.
  const actions = useNodeActions(envId, rows, {
    onRemoved: () => void navigate({ to: "/env/$envId/nodes", params: { envId } }),
  });
  const gone = isNodeGone(query.error);
  useDocumentTitle(gone ? t("nodes.detail.gone.heading") : data?.hostname);

  if (gone) return <Gone envId={envId} name={data?.hostname} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={nodeId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("nodes.detail.error.loadFailed", { name: nodeId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("nodes.detail.error.retry", { name: nodeId })}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.hostname} />
        <div className="flex flex-wrap items-start justify-between gap-3">
          {data ? <Header data={data} /> : <Skeleton className="h-8 w-64" />}
          {data ? <NodeHeaderActions envId={envId} node={data} actions={actions} /> : null}
        </div>
      </header>
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("nodes.detail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {NODE_TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, nodeId }}
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
      {data ? (
        <Outlet />
      ) : (
        <div aria-busy="true">
          <span role="status" className="sr-only">
            {t("nodes.detail.loading")}
          </span>
          <Skeleton className="h-64 w-full" />
        </div>
      )}
      {actions.dialog}
    </div>
  );
}
