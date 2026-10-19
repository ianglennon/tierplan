// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { useMemo } from "react";

import { type StackItem, stacksQuery } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { CreateStackDialog } from "@/features/stacks/create-stack-dialog";
import { StackStateCell } from "@/features/stacks/stack-state-cell";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface StackListProps {
  envId: string;
  // Whether the Create stack dialog is open: the address says so (new=1), so a link can open it.
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
}

// StackList is the stacks list of an environment: the name (a link, with a Draft badge while the
// draft holds changes that are not deployed), the state, where the definition lives and when the
// stack was last deployed. Create stack is for a user who may create stacks and hidden for
// anyone else (UI-SPEC "Stack lists"). Every word of a stack's name renders as text.
export function StackList({ envId, creating, onCreatingChange }: StackListProps) {
  const t = useT();
  const allowed = usePermission("stacks.read", envId);
  const create = useNeeds("stacks.create", envId);
  const query = useQuery({ ...stacksQuery(envId), enabled: allowed });
  const envName = useEnvName(envId);
  const { data } = query;

  // The table reads a list answer with a total and a truncated flag; the stacks list has no cap.
  const listQuery = useMemo<ListQuery<StackItem>>(
    () => ({
      data: data
        ? { items: data.items ?? [], total: (data.items ?? []).length, truncated: false }
        : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const columns: Column<StackItem>[] = [
    {
      id: "name",
      header: t("stacks.list.columns.name"),
      cell: (s) => (
        <span className="flex min-w-0 items-center gap-2">
          <Link
            to="/env/$envId/stacks/$stackId"
            params={{ envId, stackId: s.id }}
            className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{s.name}</TruncatedText>
          </Link>
          {s.hasDraft ? <Badge variant="outline">{t("stacks.draft")}</Badge> : null}
        </span>
      ),
    },
    {
      id: "state",
      header: t("stacks.list.columns.state"),
      cell: (s) => <StackStateCell state={s.state} />,
    },
    {
      id: "source",
      header: t("stacks.list.columns.source"),
      cell: (s) => <Badge variant="outline">{t(`stacks.source.${s.source}`)}</Badge>,
    },
    {
      id: "lastDeployed",
      header: t("stacks.list.columns.lastDeployed"),
      cell: (s) =>
        s.lastDeployedAt ? (
          <span className="flex flex-col leading-tight">
            <RelativeTime value={s.lastDeployedAt} />
            {s.lastDeployedBy ? (
              <span className="text-xs text-muted-foreground max-md:hidden">
                {t("stacks.list.lastDeployedBy", { user: s.lastDeployedBy })}
              </span>
            ) : null}
          </span>
        ) : (
          <span className="text-muted-foreground">{t("stacks.list.never")}</span>
        ),
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (s) => (
        <Button asChild variant="ghost" size="sm" className="max-md:h-11">
          <Link
            to="/env/$envId/stacks/$stackId"
            params={{ envId, stackId: s.id }}
            aria-label={t("stacks.list.openLabel", { name: s.name })}
          >
            {t("stacks.list.open")}
          </Link>
        </Button>
      ),
    },
  ];

  // No stacks: a user who may create one is told what a stack is and given the button; anyone
  // else gets the table's own "Nothing to show on {environment} right now."
  const empty = create.allowed ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("stacks.list.empty")}
        </EmptyTitle>
        <EmptyDescription>{t("stacks.list.emptyBody")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button className="max-md:h-11" onClick={() => onCreatingChange(true)}>
          {t("stacks.create.button")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="stacks"
        i18nKey="stacks.list"
        columns={columns}
        query={listQuery}
        envName={envName}
        caption={
          data
            ? `${t("stacks.list.caption", { name: envName })} ${t("stacks.list.count", { count: (data.items ?? []).length })}`
            : undefined
        }
        empty={empty}
        retryLabel={t("stacks.list.errorRetry")}
        getRowKey={(s) => s.id}
        captionExtra={
          create.allowed ? (
            <div className="ml-auto">
              <Button className="max-md:h-11" onClick={() => onCreatingChange(true)}>
                <Plus aria-hidden="true" />
                {t("stacks.create.button")}
              </Button>
            </div>
          ) : null
        }
      />
      {create.allowed ? (
        <CreateStackDialog envId={envId} open={creating} onOpenChange={onCreatingChange} />
      ) : null}
    </>
  );
}
