// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useMemo } from "react";

import { stacksQuery } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { type ListQuery, ResourceTable } from "@/features/resources/resource-table";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import { useEnvName } from "@/features/resources/use-env-name";
import { CreateStackDialog } from "@/features/stacks/create-stack-dialog";
import type {
  StackListSearch,
  StackListSearchPatch,
  StackSortKey,
} from "@/features/stacks/list-search";
import { type StackRow, stackColumns, stackRowKey } from "@/features/stacks/stack-columns";
import { StackFilterBar, StackNoMatch } from "@/features/stacks/stack-filter-bar";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface StackListProps {
  envId: string;
  // Whether the Create stack dialog is open: the address says so (new=1), so a link can open it.
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
  // The route's validated search, and the way to change it: a key set to undefined leaves the URL.
  // Changes replace the history entry.
  search: StackListSearch;
  onSearchChange: (patch: StackListSearchPatch) => void;
}

// StackList is the stacks list of an environment: the name (a link, with a Draft badge while the
// draft holds changes that are not deployed), the state, the services that run, where the definition
// lives and when the stack was last deployed. It is found by a search and the status chips, which the
// server answers before its 500-row cap, and sorted from the column headers. Create stack is for a
// user who may create stacks and hidden for anyone else (UI-SPEC "Stack lists"). Every word of a
// stack's name renders as text.
export function StackList({
  envId,
  creating,
  onCreatingChange,
  search,
  onSearchChange,
}: StackListProps) {
  const t = useT();
  const allowed = usePermission("stacks.read", envId);
  const create = useNeeds("stacks.create", envId);
  const sortKey: StackSortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";
  const query = useQuery({
    ...stacksQuery(envId, {
      q: search.q,
      status: search.status,
      sort: search.sort,
      dir: search.dir,
    }),
    enabled: allowed,
  });
  const envName = useEnvName(envId);
  const { data } = query;

  const rows = useMemo<StackRow[]>(
    () => (data?.items ?? []).map((s) => ({ ...s, envId, envName })),
    [data, envId, envName],
  );
  // The table reads a list answer with a total and a truncated flag; the stacks list calls it capped.
  const listQuery = useMemo<ListQuery<StackRow>>(
    () => ({
      data: data ? { items: rows, total: data.total, truncated: data.capped } : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, rows, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const columns = stackColumns(t);
  const filtered = Boolean(search.q || search.status);
  const clearFilters = () => onSearchChange({ q: undefined, status: undefined });

  function onSort(key: string) {
    const next = key as StackSortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  // A search or a chip that lets nothing through has its own empty state when stacks exist; with none
  // at all, a user who may create one is told what a stack is and given the button, and anyone else
  // gets the table's own "Nothing to show on {environment} right now."
  const noneAtAll = !filtered || (data?.unfilteredTotal ?? 0) === 0;
  const empty = !noneAtAll ? (
    <StackNoMatch
      q={search.q}
      moreThanSearch={Boolean(search.status)}
      onClearSearch={() => onSearchChange({ q: undefined })}
      onClear={clearFilters}
    />
  ) : create.allowed ? (
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

  const toolbar = (
    <StackFilterBar
      q={search.q}
      status={search.status}
      counts={data?.statusCounts}
      filtered={filtered}
      onSearch={(q) => onSearchChange({ q })}
      onStatus={(status) => onSearchChange({ status })}
      onClear={clearFilters}
      chipRowClassName={STATUS_CHIP_ROW}
    />
  );

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="stacks"
        i18nKey="stacks.list"
        columns={columns}
        query={listQuery}
        envName={envName}
        toolbar={toolbar}
        caption={
          data
            ? `${t("stacks.list.caption", { name: envName })} ${
                filtered
                  ? t("stackLists.filtered", { n: data.total, count: data.unfilteredTotal })
                  : t("stacks.list.count", { count: data.total })
              }`
            : undefined
        }
        empty={empty}
        capNote={data ? t("stackLists.cap", { total: data.total }) : undefined}
        retryLabel={t("stacks.list.errorRetry")}
        sort={{ key: sortKey, dir: sortDir, onSort }}
        getRowKey={stackRowKey}
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
