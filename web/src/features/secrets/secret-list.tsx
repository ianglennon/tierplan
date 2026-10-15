// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";

import { type SecretItem, secretDetailQuery, secretsQuery } from "@/api/secrets";
import { isNotASwarm } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Skeleton } from "@/components/ui/skeleton";
import { SearchBox } from "@/features/containers/container-list";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { DeleteSecretDialog, secretDeleteReason } from "@/features/secrets/delete-secret-dialog";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface SecretListProps {
  envId: string;
  // The search text, from the URL.
  q?: string;
  onSearchChange: (q: string | undefined) => void;
  // Whether the create sheet is open: the address says so (new=1), so a link can open it.
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
}

// UsedByPopover is the "Used by {n} services" button of a row. It opens the services as links; the
// list itself carries the count only, so the services are read from the secret's page when the
// popover opens.
function UsedByPopover({ envId, secret }: { envId: string; secret: SecretItem }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const detail = useQuery({ ...secretDetailQuery(envId, secret.id), enabled: open });
  // A service that mounts the secret twice is one service.
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
          {t("secrets.list.usedBy.count", { count: secret.usedBy })}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto max-w-80">
        {detail.isError ? (
          <p className="text-sm">{t("secrets.list.usedBy.error")}</p>
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
              {t("secrets.list.usedBy.loading")}
            </span>
            <Skeleton className="h-4 w-32" />
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}

// SecretList is the secrets list of a swarm environment: the name, how many services use each
// secret, who owns it and when it was made and last updated. Nothing in it is a value: the swarm
// never returns one. For an env Admin it has Delete…; someone who may read but not manage sees it
// disabled with the reason, and so does Delete… on a secret any service spec mentions (D-27).
export function SecretList({ envId, q, onSearchChange }: SecretListProps) {
  const t = useT();
  const allowed = usePermission("secrets.read", envId);
  const manage = useNeeds("secrets.manage", envId);
  const [deleting, setDeleting] = useState<SecretItem | null>(null);
  const query = useQuery({ ...secretsQuery(envId, { q }), enabled: allowed });
  const envName = useEnvName(envId);
  const missing = t("secrets.list.missing");
  const { data } = query;

  // The table reads a list answer with a `truncated` flag; the secrets list calls it capped.
  const listQuery = useMemo<ListQuery<SecretItem>>(
    () => ({
      data: data ? { items: data.items, total: data.total, truncated: data.capped } : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const columns: Column<SecretItem>[] = [
    {
      id: "name",
      header: t("secrets.list.columns.name"),
      cell: (s) => (
        <Link
          to="/env/$envId/secrets/$secretId"
          params={{ envId, secretId: s.id }}
          className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <TruncatedText mono>{s.name}</TruncatedText>
        </Link>
      ),
    },
    {
      id: "usedBy",
      header: t("secrets.list.columns.usedBy"),
      cell: (s) =>
        s.usedBy > 0 ? (
          <UsedByPopover envId={envId} secret={s} />
        ) : (
          <span className="text-muted-foreground">{t("secrets.list.usedBy.none")}</span>
        ),
    },
    {
      id: "owner",
      header: t("secrets.list.columns.owner"),
      cell: (s) =>
        s.owner ? (
          <TruncatedText>{t("secrets.list.owner", { name: s.owner })}</TruncatedText>
        ) : (
          missing
        ),
    },
    {
      id: "created",
      header: t("secrets.list.columns.created"),
      cell: (s) => <RelativeTime value={s.createdAt} />,
    },
    {
      id: "updated",
      header: t("secrets.list.columns.updated"),
      cell: (s) => <RelativeTime value={s.updatedAt} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (s) => {
        const reason = secretDeleteReason(t, manage.reason, s.usedBy);
        return (
          <span className="flex items-center justify-end gap-1">
            <Button asChild variant="ghost" size="sm" className="max-md:h-11">
              <Link
                to="/env/$envId/secrets/$secretId"
                params={{ envId, secretId: s.id }}
                aria-label={t("secrets.list.openLabel", { name: s.name })}
              >
                {t("secrets.list.open")}
              </Link>
            </Button>
            <NeedsTooltip reason={reason}>
              <Button
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                aria-label={t("secrets.delete.label", { name: s.name })}
                onClick={reason ? undefined : () => setDeleting(s)}
              >
                {t("secrets.delete.action")}
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
        label={t("secrets.list.search.label")}
        placeholder={t("secrets.list.search.placeholder")}
        clearLabel={t("secrets.list.search.clear")}
      />
    </div>
  );

  // A search that matches nothing says so, with the way out.
  const empty = q ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {t("secrets.list.emptySearch.title", { q })}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={() => onSearchChange(undefined)}>
          {t("secrets.list.emptySearch.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  if (isNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;
  return (
    <>
      <ResourceTable
        resource="secrets"
        i18nKey="secrets.list"
        columns={columns}
        query={listQuery}
        envName={envName}
        toolbar={toolbar}
        caption={
          data && q
            ? t("secrets.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
            : undefined
        }
        empty={empty}
        capNote={data ? t("secrets.list.cap", { total: data.total }) : undefined}
        retryLabel={t("secrets.list.errorRetry")}
        getRowKey={(s) => s.id}
      />
      {manage.allowed ? (
        <DeleteSecretDialog envId={envId} secret={deleting} onClose={() => setDeleting(null)} />
      ) : null}
    </>
  );
}
