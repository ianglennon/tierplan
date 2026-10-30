// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { useMemo, useState } from "react";

import { type ConfigItem, configDetailQuery, configsQuery } from "@/api/configs";
import { isNotASwarm, ServicesLoadError } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Skeleton } from "@/components/ui/skeleton";
import { configDeleteReason, DeleteConfigDialog } from "@/features/configs/delete-config-dialog";
import { SearchBox } from "@/features/containers/container-list";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { ObjectSheet } from "@/features/secrets/object-sheet";
import { RotateDialog } from "@/features/secrets/rotate-dialog";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { StackSentence } from "@/features/stacks/stack-link";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface ConfigListProps {
  envId: string;
  // The search text, from the URL.
  q?: string;
  onSearchChange: (q: string | undefined) => void;
  // Whether the create sheet is open: the address says so (new=1), so a link can open it.
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
}

// UsedByPopover is the "Used by {n} services" button of a row. It opens the services as links; the
// list itself carries the count only, so the services are read from the config's page when the
// popover opens.
function UsedByPopover({ envId, config }: { envId: string; config: ConfigItem }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const detail = useQuery({ ...configDetailQuery(envId, config.id), enabled: open });
  // A service that mounts the config twice is one service.
  const services = useMemo(() => {
    const seen = new Map<string, string>();
    for (const row of detail.data?.usedByRows ?? []) seen.set(row.serviceId, row.service);
    return [...seen];
  }, [detail.data]);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 text-accent-ink max-md:h-11"
        >
          {t("configs.list.usedBy.count", { count: config.usedBy })}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto max-w-80">
        {detail.isError ? (
          <p className="text-sm">{t("configs.list.usedBy.error")}</p>
        ) : detail.data ? (
          <ul className="flex flex-col gap-1 text-sm">
            {services.map(([serviceId, service]) => (
              <li key={serviceId} className="break-all">
                <Link
                  to="/env/$envId/services/$serviceId"
                  params={{ envId, serviceId }}
                  className="rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                >
                  {service}
                </Link>
              </li>
            ))}
          </ul>
        ) : (
          <div aria-busy="true">
            <span role="status" className="sr-only">
              {t("configs.list.usedBy.loading")}
            </span>
            <Skeleton className="h-4 w-32" />
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}

// ConfigList is the configs list of a swarm environment: the name, how many services use each
// config, who owns it and when it was made and last updated. The list never carries content: that
// is read on a config's own page. For an env Admin it has Delete…; someone who may read but not
// manage sees it disabled with the reason, and so does Delete… on a config any service spec
// mentions (D-27).
export function ConfigList({
  envId,
  q,
  onSearchChange,
  creating,
  onCreatingChange,
}: ConfigListProps) {
  const t = useT();
  const allowed = usePermission("configs.read", envId);
  const manage = useNeeds("configs.manage", envId);
  // A new version updates the services that use the old one, which an Operator may do and an Admin may add to.
  const control = useNeeds("services.control", envId);
  const rotateReason = manage.reason ?? control.reason;
  const [deleting, setDeleting] = useState<ConfigItem | null>(null);
  const [rotating, setRotating] = useState<ConfigItem | null>(null);
  const query = useQuery({ ...configsQuery(envId, { q }), enabled: allowed });
  const envName = useEnvName(envId);
  const missing = t("configs.list.missing");
  const { data } = query;
  // The server answers 502 config_list_too_large when the configs, each carrying its content, are
  // too many or too large for one answer; the words say what to do about it.
  const tooLarge =
    query.error instanceof ServicesLoadError && query.error.type === "config_list_too_large";

  // The table reads a list answer with a `truncated` flag; the configs list calls it capped.
  const listQuery = useMemo<ListQuery<ConfigItem>>(
    () => ({
      data: data ? { items: data.items, total: data.total, truncated: data.capped } : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const columns: Column<ConfigItem>[] = [
    {
      id: "name",
      header: t("configs.list.columns.name"),
      cell: (s) => (
        <Link
          to="/env/$envId/configs/$configId"
          params={{ envId, configId: s.id }}
          className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <TruncatedText mono>{s.name}</TruncatedText>
        </Link>
      ),
    },
    {
      id: "usedBy",
      header: t("configs.list.columns.usedBy"),
      cell: (s) =>
        s.usedBy > 0 ? (
          <UsedByPopover envId={envId} config={s} />
        ) : (
          <span className="text-muted-foreground">{t("configs.list.usedBy.none")}</span>
        ),
    },
    {
      id: "owner",
      header: t("configs.list.columns.owner"),
      cell: (s) =>
        s.owner ? (
          s.coamingStackId ? (
            <span className="block max-w-64 truncate">
              <StackSentence
                i18nKey="configs.list.owner"
                variable="name"
                stack={s.owner}
                envId={envId}
                coamingStackId={s.coamingStackId}
              />
            </span>
          ) : (
            <TruncatedText>{t("configs.list.owner", { name: s.owner })}</TruncatedText>
          )
        ) : (
          missing
        ),
    },
    {
      id: "created",
      header: t("configs.list.columns.created"),
      cell: (s) => <RelativeTime value={s.createdAt} />,
    },
    {
      id: "updated",
      header: t("configs.list.columns.updated"),
      cell: (s) => <RelativeTime value={s.updatedAt} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (s) => {
        const reason = configDeleteReason(t, manage.reason, s.usedBy);
        return (
          <span className="flex items-center justify-end gap-1">
            <Button asChild variant="ghost" size="sm" className="max-md:h-11">
              <Link
                to="/env/$envId/configs/$configId"
                params={{ envId, configId: s.id }}
                aria-label={t("configs.list.openLabel", { name: s.name })}
              >
                {t("configs.list.open")}
              </Link>
            </Button>
            <NeedsTooltip reason={rotateReason}>
              <Button
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                aria-label={t("secrets.rotate.label", { name: s.name })}
                onClick={rotateReason ? undefined : () => setRotating(s)}
              >
                {t("secrets.rotate.action")}
              </Button>
            </NeedsTooltip>
            <NeedsTooltip reason={reason}>
              <Button
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                aria-label={t("configs.delete.label", { name: s.name })}
                onClick={reason ? undefined : () => setDeleting(s)}
              >
                {t("configs.delete.action")}
              </Button>
            </NeedsTooltip>
          </span>
        );
      },
    },
  ];

  const toolbar = (
    <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
      <SearchBox
        value={q}
        onCommit={onSearchChange}
        label={t("configs.list.search.label")}
        placeholder={t("configs.list.search.placeholder")}
        clearLabel={t("configs.list.search.clear")}
      />
    </div>
  );

  // A search that matches nothing says so, with the way out.
  const empty = q ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {t("configs.list.emptySearch.title", { q })}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={() => onSearchChange(undefined)}>
          {t("configs.list.emptySearch.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : manage.allowed ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("configs.list.empty")}
        </EmptyTitle>
        <EmptyDescription>{t("configs.list.emptyCreateBody")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button className="max-md:h-11" onClick={() => onCreatingChange(true)}>
          {t("configs.create.button")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  if (isNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;
  return (
    <>
      <ResourceTable
        resource="configs"
        i18nKey="configs.list"
        columns={columns}
        query={listQuery}
        envName={envName}
        toolbar={toolbar}
        caption={
          data && q
            ? t("configs.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
            : undefined
        }
        empty={empty}
        capNote={data ? t("configs.list.cap", { total: data.total }) : undefined}
        retryLabel={t("configs.list.errorRetry")}
        errorBody={tooLarge ? t("configs.list.tooLarge") : undefined}
        getRowKey={(s) => s.id}
        captionExtra={
          <div className="ml-auto">
            <NeedsTooltip reason={manage.reason}>
              <Button
                className="max-md:h-11"
                onClick={manage.allowed ? () => onCreatingChange(true) : undefined}
              >
                <Plus aria-hidden="true" />
                {t("configs.create.button")}
              </Button>
            </NeedsTooltip>
          </div>
        }
      />
      {manage.allowed ? (
        <ObjectSheet envId={envId} kind="config" open={creating} onOpenChange={onCreatingChange} />
      ) : null}
      {manage.allowed ? (
        <DeleteConfigDialog envId={envId} config={deleting} onClose={() => setDeleting(null)} />
      ) : null}
      {manage.allowed && control.allowed ? (
        <RotateDialog
          kind="config"
          envId={envId}
          object={rotating ? { id: rotating.id, name: rotating.name } : null}
          onClose={() => setRotating(null)}
        />
      ) : null}
    </>
  );
}
