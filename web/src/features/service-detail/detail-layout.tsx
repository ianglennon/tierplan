// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { Boxes, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { isServiceGone, type ServiceDetail, serviceQuery } from "@/api/services";
import { copyText } from "@/components/copyable-code";
import { RelativeTime } from "@/components/relative-time";
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
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { ServiceHeaderActions } from "@/features/service-detail/header-actions";
import { SERVICE_TABS } from "@/features/service-detail/tabs";
import { ManagedByStackNotice } from "@/features/services/managed-by-stack-notice";
import { ServiceStateCell } from "@/features/services/service-state-cell";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

interface ServiceDetailLayoutProps {
  envId: string;
  serviceId: string;
}

// How long the digest chip says "Copied" before it shows the digest again.
const COPIED_MS = 2000;

// The digest as the chip shows it: the algorithm and the first twelve characters.
const shortDigest = (digest: string) => digest.slice(0, "sha256:".length + 12);

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("servicedetail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/services"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("servicedetail.breadcrumb.services")}
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

// DigestChip is the image's digest as an outline badge with its short form. The full digest is
// in the tooltip, and a click copies it. A text node only: the digest is Docker's.
function DigestChip({ digest }: { digest: string }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLSpanElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(digest, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    toastError(t("env.copy.failed"));
  }

  return (
    <span ref={host}>
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            aria-label={t("servicedetail.header.copyDigest")}
            onClick={() => void onCopy()}
            className="rounded-full focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <Badge variant="outline" className="font-mono">
              {copied ? t("servicedetail.header.digestCopied") : shortDigest(digest)}
            </Badge>
          </button>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm break-all font-mono">{digest}</TooltipContent>
      </Tooltip>
    </span>
  );
}

// Gone is what a service's page becomes when the swarm no longer has the service. It replaces
// the page, tabs included, since there is nothing left for any of them to show.
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
            <Boxes aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("servicedetail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("servicedetail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/services" params={{ envId }}>
              {t("servicedetail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

function Header({ envId, data }: { envId: string; data: ServiceDetail }) {
  const t = useT();
  const modeKey = `services.list.mode.${data.mode}`;
  return (
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap items-center gap-2">
          <Tooltip>
            <TooltipTrigger asChild>
              <h2 className="max-w-full min-w-0 line-clamp-2 text-2xl font-semibold break-all">
                {data.name}
              </h2>
            </TooltipTrigger>
            <TooltipContent className="break-all">{data.name}</TooltipContent>
          </Tooltip>
          <ServiceStateCell
            service={{
              mode: data.mode,
              running: data.running,
              desired: data.desired,
              updateState: data.updateStatus,
            }}
          />
          <Badge variant="outline" className="shrink-0">
            {t(modeKey)}
          </Badge>
          {data.stack ? (
            <Badge variant="outline" className="max-w-full break-all">
              {t("servicedetail.header.stack", { stack: data.stack })}
            </Badge>
          ) : null}
          {data.agent ? <SelfContainerBadge kind="agent-service" /> : null}
        </div>
        <p className="flex flex-wrap items-center gap-x-2 gap-y-1 text-sm text-muted-foreground">
          {data.image.ref ? (
            <span className="font-mono break-all">{data.image.tag || data.image.ref}</span>
          ) : null}
          {data.image.digest ? <DigestChip digest={data.image.digest} /> : null}
          <span>
            {t("servicedetail.header.updated")} <RelativeTime value={data.updatedAt} />
          </span>
        </p>
      </div>
      <ServiceHeaderActions envId={envId} detail={data} />
    </div>
  );
}

// ServiceDetailLayout is the frame of a service's page: the breadcrumb, the header with the
// name, the state, the badges, the image line and the actions, the alerts, the tab bar (Link
// triggers, the active tab follows the URL) and the tab under it. The tabs render at once,
// before the service has loaded (UI-SPEC "Loading"). Rollout, restart-loop and
// replicas-short alerts are added by later plans, in the alerts region.
export function ServiceDetailLayout({ envId, serviceId }: ServiceDetailLayoutProps) {
  const t = useT();
  const query = useQuery(serviceQuery(envId, serviceId));
  const { data } = query;
  const gone = isServiceGone(query.error);
  useDocumentTitle(gone ? t("servicedetail.gone.heading") : data?.name);

  if (gone) return <Gone envId={envId} name={data?.name} />;

  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={serviceId} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("servicedetail.error.loadFailed", { name: serviceId })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("servicedetail.error.retry", { name: serviceId })}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={data?.name} />
        {data ? <Header envId={envId} data={data} /> : <Skeleton className="h-8 w-64" />}
      </header>
      {data?.agent ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">
            {t("servicedetail.agentService")}
          </AlertDescription>
        </Alert>
      ) : null}
      {data?.stack ? <ManagedByStackNotice stack={data.stack} /> : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("servicedetail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {SERVICE_TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, serviceId }}
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
