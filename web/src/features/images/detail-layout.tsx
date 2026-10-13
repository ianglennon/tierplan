// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link, Outlet } from "@tanstack/react-router";
import { ChevronDown, Download, Layers } from "lucide-react";
import { useState } from "react";

import {
  type ImageDetail,
  type ImageUsedBy,
  imageQuery,
  imageUsedByQuery,
  isImageGone,
} from "@/api/images";
import { allReadOnly, pullTargetsQuery } from "@/api/pulls";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { inUse, inUseCounts, inUseWhat, nodeUse } from "@/features/images/in-use";
import { RemoveImageDialog } from "@/features/images/remove-image-dialog";
import { IMAGE_TABS } from "@/features/images/tabs";
import { usedWhat } from "@/features/images/used-by";
import { PullDialog } from "@/features/pulls/pull-dialog";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useDocumentTitle } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface ImageDetailLayoutProps {
  envId: string;
  imageId: string;
  // The node to read the image on, from the page's address; absent reads every node.
  node?: string;
}

// displayTitle is what the page calls the image: its first tag, or the short ID when it has
// none (UI-SPEC "Image detail").
export function displayTitle(d: ImageDetail): string {
  return (d.names ?? []).find((n) => n.kind === "tag")?.name ?? d.shortId;
}

function Breadcrumb({ envId, name }: { envId: string; name: string | undefined }) {
  const t = useT();
  return (
    <nav aria-label={t("images.detail.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link
            to="/env/$envId/images"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {t("images.detail.breadcrumb.images")}
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

// Gone is what an image's page becomes when no node has the image any more: it was
// removed, or its tag moved to another image. It replaces the page, tabs included.
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
            <Layers aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2}>
            {t("images.detail.gone.heading")}
          </EmptyTitle>
          <EmptyDescription>{t("images.detail.gone.body")}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/images" params={{ envId }}>
              {t("images.detail.gone.back")}
            </Link>
          </Button>
        </EmptyContent>
      </Empty>
    </div>
  );
}

// HeaderActions are the buttons in an image page's header, outline only: Pull… opens the
// pull form with the image's first tag, and More holds Copy image ID. Pull… is shown to
// everyone and unavailable, with the reason, to a caller who may not pull or on a host
// whose owner turned writes off (D-19, D-30).
function HeaderActions({
  envId,
  detail,
  title,
  usage,
}: {
  envId: string;
  detail: ImageDetail;
  title: string;
  usage: ImageUsedBy | undefined;
}) {
  const t = useT();
  const swarm = useEnvIsSwarm(envId);
  const hostName = useEnvName(envId);
  const pull = useNeeds("images.pull", envId);
  const { data: targets } = useQuery(pullTargetsQuery(envId));
  const reason = pull.reason ?? (allReadOnly(targets) ? t("pulls.readOnly.reason") : undefined);
  const remove = useNeeds("images.delete", envId);
  const [open, setOpen] = useState(false);
  const [removing, setRemoving] = useState(false);
  const holders = (detail.nodes ?? []).filter((n) => n.present);
  const removeReason =
    // Who may comes first, then whether the host lets anything change (D-30), then whether
    // something uses the image on every node that has it (D-28).
    remove.reason ??
    (allReadOnly(targets) ? t("pulls.readOnly.reason") : undefined) ??
    (blockedEverywhere(detail, usage)
      ? t("images.remove.inUseReason", {
          what: inUseWhat(
            t,
            inUseCounts(usage?.items ?? []).containers,
            inUseCounts(usage?.items ?? []).services,
          ),
        })
      : undefined);
  const firstTag = (detail.names ?? []).find((n) => n.kind === "tag")?.name;

  async function copyId() {
    if (await copyText(detail.id)) toastSuccess(t("images.detail.actions.copied"));
    else toastError(t("images.detail.actions.copyFailed"));
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <NeedsTooltip reason={reason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          aria-label={t("images.detail.actions.pullLabel", { name: title })}
          onClick={pull.allowed && !reason ? () => setOpen(true) : undefined}
        >
          <Download aria-hidden="true" />
          {t("images.detail.actions.pull")}
        </Button>
      </NeedsTooltip>
      <NeedsTooltip reason={removeReason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11 border-destructive-fill text-destructive"
          aria-label={t("images.remove.label", { name: title })}
          onClick={remove.allowed && !removeReason ? () => setRemoving(true) : undefined}
        >
          {t("images.remove.action")}
        </Button>
      </NeedsTooltip>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            aria-label={t("images.detail.actions.more")}
          >
            {t("images.detail.actions.moreText")}
            <ChevronDown aria-hidden="true" data-icon="inline-end" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copyId()}>
            {t("images.detail.actions.copyId")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {remove.allowed ? (
        <RemoveImageDialog
          envId={envId}
          imageId={detail.id}
          name={title}
          nodes={holders.map((n) => ({ nodeId: n.nodeId, name: n.name }))}
          swarm={swarm}
          open={removing}
          onClose={() => setRemoving(false)}
        />
      ) : null}
      {pull.allowed ? (
        <PullDialog
          envId={envId}
          open={open}
          onOpenChange={setOpen}
          initialRef={firstTag}
          hostName={hostName}
          swarm={swarm}
        />
      ) : null}
    </div>
  );
}

// blockedEverywhere says whether something uses the image on every node that has it, so there
// is nowhere to remove it: a container blocks its own node, a service spec every node.
function blockedEverywhere(detail: ImageDetail, usage: ImageUsedBy | undefined): boolean {
  if (!usage) return false;
  const holders = (detail.nodes ?? []).filter((n) => n.present);
  return holders.length > 0 && holders.every((n) => inUse(nodeUse(usage.items ?? [], n.nodeId)));
}

// ImageDetailLayout is the frame of an image's page: the breadcrumb, the header with the
// Display title, the Dangling and node-count badges and the actions, the tab bar (Link
// triggers, the active tab follows the URL) and the tab under it. The tabs render at once,
// before the image has loaded (UI-SPEC "Loading").
export function ImageDetailLayout({ envId, imageId, node }: ImageDetailLayoutProps) {
  const t = useT();
  const swarm = useEnvIsSwarm(envId);
  const query = useQuery(imageQuery(envId, imageId, node));
  const { data } = query;
  // What uses the image, for the muted line under the title. It is read with the page and
  // shares its key, so the events feed refreshes both; a failed read just leaves the line out.
  const usage = useQuery(imageUsedByQuery(envId, imageId, node)).data;
  const gone = isImageGone(query.error);
  const title = data ? displayTitle(data) : undefined;
  useDocumentTitle(gone ? t("images.detail.gone.heading") : title);

  if (gone) return <Gone envId={envId} name={title} />;

  const shortName = imageId.replace(/^sha256:/, "").slice(0, 12);
  if (query.isError && !data) {
    return (
      <div className="flex flex-col gap-6">
        <header>
          <Breadcrumb envId={envId} name={shortName} />
        </header>
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("images.detail.error.loadFailed", { name: shortName })}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {t("images.detail.error.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    );
  }

  const nodes = data?.nodes ?? [];
  const present = nodes.filter((n) => n.present).length;
  const total = nodes.length;
  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-col gap-2">
        <Breadcrumb envId={envId} name={title} />
        {data && title ? (
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <Tooltip>
                <TooltipTrigger asChild>
                  <h2 className="line-clamp-2 max-w-full min-w-0 font-mono text-2xl font-semibold break-all">
                    {title}
                  </h2>
                </TooltipTrigger>
                <TooltipContent className="break-all">{title}</TooltipContent>
              </Tooltip>
              {data.dangling ? (
                <Badge variant="outline" className="shrink-0">
                  {t("images.detail.header.dangling")}
                </Badge>
              ) : null}
              {swarm && total > 0 ? (
                <Badge variant="outline" className="shrink-0">
                  {t("images.detail.header.onNodes", { present, total })}
                </Badge>
              ) : null}
            </div>
            <HeaderActions envId={envId} detail={data} title={title} usage={usage} />
          </div>
        ) : (
          <Skeleton className="h-8 w-64" />
        )}
        {usage ? (
          <p className="text-sm text-muted-foreground">
            {usage.usedByContainers + usage.usedByServices > 0
              ? t("images.detail.header.usedBy", {
                  what: usedWhat(t, usage.usedByContainers, usage.usedByServices, swarm),
                })
              : t("images.detail.header.notInUse")}
          </p>
        ) : null}
      </header>
      {data && title && usage && blockedEverywhere(data, usage) ? (
        <Alert variant="warning">
          <AlertTitle className="break-all">{t("usedBy.image.title", { name: title })}</AlertTitle>
          <AlertDescription className="flex flex-col gap-2">
            <p>{t("usedBy.image.body")}</p>
            <UsedByList
              containers={(usage.items ?? [])
                .filter((u) => u.kind === "container")
                .map((u) => ({ name: u.name, node: u.node }))}
              services={(usage.items ?? [])
                .filter((u) => u.kind === "service")
                .map((u) => ({ name: u.name }))}
            />
          </AlertDescription>
        </Alert>
      ) : null}
      <Tabs value="" className="gap-4">
        <TabsList
          variant="line"
          aria-label={t("images.detail.tabs.label")}
          className="h-9 max-md:h-11"
        >
          {IMAGE_TABS.map((tab) => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              asChild
              className="data-active:text-accent-ink after:bg-accent-ink max-md:h-11"
            >
              <Link
                to={tab.to}
                params={{ envId, imageId }}
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
