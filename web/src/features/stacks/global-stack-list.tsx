// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronsDownUp, ChevronsUpDown, Plus } from "lucide-react";
import { useCallback, useId, useMemo } from "react";

import { environmentsQuery } from "@/api/environments";
import { allStacksQuery, type FailedEnvironment } from "@/api/stacks";
import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type ListQuery,
  ResourceTable,
  type TableGroup,
} from "@/features/resources/resource-table";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import { CreateStackDialog } from "@/features/stacks/create-stack-dialog";
import {
  envList,
  envParam,
  type GlobalStackSearch,
  type GlobalStackSearchPatch,
  type StackGroupBy,
  type StackSortKey,
} from "@/features/stacks/list-search";
import { type StackRow, stackColumns, stackRowKey } from "@/features/stacks/stack-columns";
import {
  DiscoveryFailedNotice,
  StackFilterBar,
  StackKindEmpty,
  StackNoMatch,
} from "@/features/stacks/stack-filter-bar";
import { sortStackRows } from "@/features/stacks/stack-sort";
import { hasPermission, useAnyEnvironmentPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface GlobalStackListProps {
  // The route's validated search, and the way to change it: a key set to undefined leaves the URL.
  // Changes replace the history entry.
  search: GlobalStackSearch;
  onSearchChange: (patch: GlobalStackSearchPatch) => void;
  // Whether the Create stack dialog is open: the address says so (new=1), so a link can open it.
  creating: boolean;
  onCreatingChange: (open: boolean) => void;
}

interface EnvironmentFilterProps {
  value: string[];
  onChange: (env: string[]) => void;
  // The environments the signed-in user may read stacks on.
  options: { id: string; name: string }[];
}

// EnvironmentFilter is the multi-select of environments: a popover with a checkbox for each one the
// user may read stacks on. An ID in the URL that is not offered stays in the list, shown by its ID, so
// a linked view is not stuck.
function EnvironmentFilter({ value, onChange, options }: EnvironmentFilterProps) {
  const t = useT();
  const id = useId();
  const known = new Set(options.map((o) => o.id));
  const all = [...options, ...value.filter((v) => !known.has(v)).map((v) => ({ id: v, name: v }))];

  function toggle(envId: string, on: boolean) {
    onChange(on ? [...value, envId] : value.filter((v) => v !== envId));
  }

  return (
    <div className="flex items-center gap-2">
      <Label id={`${id}-label`} htmlFor={`${id}-trigger`} className="text-sm text-muted-foreground">
        {t("stackLists.envFilter.label")}
      </Label>
      <Popover>
        <PopoverTrigger asChild>
          <Button
            id={`${id}-trigger`}
            aria-labelledby={`${id}-label ${id}-value`}
            variant="outline"
            size="sm"
            className="min-w-44 justify-between font-normal max-md:h-11"
          >
            <span id={`${id}-value`} className="truncate">
              {value.length > 0
                ? t("stackLists.envFilter.count", { count: value.length })
                : t("stackLists.envFilter.all")}
            </span>
            <ChevronDown aria-hidden="true" className="text-muted-foreground" />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-72">
          <fieldset className="flex max-h-72 flex-col gap-1 overflow-y-auto">
            <legend className="sr-only">{t("stackLists.envFilter.list")}</legend>
            {all.map((env) => {
              const boxId = `${id}-${env.id}`;
              return (
                <div
                  key={env.id}
                  className="flex items-center gap-2 rounded-md px-1.5 py-1 max-md:py-2"
                >
                  <Checkbox
                    id={boxId}
                    checked={value.includes(env.id)}
                    onCheckedChange={(on) => toggle(env.id, on === true)}
                  />
                  <Label htmlFor={boxId} className="flex-1 cursor-pointer font-normal">
                    {env.name}
                  </Label>
                </div>
              );
            })}
          </fieldset>
        </PopoverContent>
      </Popover>
    </div>
  );
}

// FailedEnvironmentNotice is what stands in the place of an environment whose stacks could not be read:
// the others still render.
function FailedEnvironmentNotice({
  failed,
  onRetry,
  retrying,
}: {
  failed: FailedEnvironment;
  onRetry: () => void;
  retrying: boolean;
}) {
  const t = useT();
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription className="flex flex-wrap items-center gap-3">
        <span>{t("stackLists.envFailed.text", { name: failed.envName })}</span>
        <Button variant="outline" size="sm" disabled={retrying} onClick={onRetry}>
          {t("stackLists.envFailed.retry")}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

// GlobalStackList is the All stacks page (UI-SPEC "Stack lists", D-21): every stack of every environment
// the user may read stacks on, under a header band for each environment or in one list, with an
// Environment column. The environments chosen and the grouping live in the URL. An environment that
// cannot be read is named in place of its group while the others render. The server leaves out an
// environment the user holds no stacks.read on, so nothing here decides what may be seen. Every name
// renders as text.
export function GlobalStackList({
  search,
  onSearchChange,
  creating,
  onCreatingChange,
}: GlobalStackListProps) {
  const t = useT();
  const { data: me } = useMe();
  const allowed = useAnyEnvironmentPermission("stacks.read");
  // Create stack is for a user who may create stacks on at least one environment; its dialog asks which.
  const canCreate = useAnyEnvironmentPermission("stacks.create");
  const environments = useQuery(environmentsQuery);
  const chosen = envList(search.env);
  const query = useQuery({
    ...allStacksQuery({
      env: search.env,
      q: search.q,
      status: search.status,
      kind: search.kind,
      sort: search.sort,
      dir: search.dir,
    }),
    enabled: allowed,
  });
  const data = query.data;
  const collapse = useGroupCollapse();
  const by: StackGroupBy = search.group ?? "env";
  const sortKey: StackSortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";

  const options = useMemo(
    () =>
      (environments.data?.items ?? [])
        .filter((e) => hasPermission(me, "stacks.read", e.id))
        .map((e) => ({ id: e.id, name: e.name })),
    [environments.data, me],
  );

  const groupRows = useMemo(
    () =>
      (data?.groups ?? []).map((g) => ({
        group: g,
        rows: (g.stacks ?? []).map<StackRow>((s) => ({ ...s, envId: g.envId, envName: g.envName })),
      })),
    [data],
  );
  const failed = data?.failedEnvironments ?? [];
  const shownEnvironments = groupRows.length;

  const columns = stackColumns(t, { withEnvironment: true });

  const { refetch } = query;
  const retry = useCallback(() => void refetch(), [refetch]);
  const groups = useMemo<TableGroup<StackRow>[]>(() => {
    if (by === "none") {
      // The server sorts inside each environment; one list needs every row in a single order.
      return [
        {
          key: "all",
          rows: sortStackRows(
            groupRows.flatMap((g) => g.rows),
            sortKey,
            sortDir,
          ),
        },
      ];
    }
    const out: TableGroup<StackRow>[] = groupRows.map(({ group, rows }) => ({
      key: group.envId,
      rows,
      header: {
        name: group.envName,
        summary: t("stackLists.group.count", { count: rows.length }),
        badge: <Badge variant="outline">{t(`stackLists.target.${group.target}`)}</Badge>,
      },
    }));
    for (const g of out) {
      const found = groupRows.find((r) => r.group.envId === g.key)?.group;
      if (found?.discoveryFailed) {
        g.notice = (
          <DiscoveryFailedNotice name={found.envName} onRetry={retry} retrying={query.isFetching} />
        );
      }
    }
    for (const f of failed) {
      out.push({
        key: f.envId,
        rows: [],
        header: { name: f.envName },
        notice: <FailedEnvironmentNotice failed={f} onRetry={retry} retrying={query.isFetching} />,
      });
    }
    return out;
  }, [by, sortKey, sortDir, groupRows, failed, query.isFetching, retry, t]);

  const headed = groups.filter((g) => g.header).map((g) => g.key);
  const allCollapsed = headed.length > 0 && headed.every((k) => collapse.collapsed.has(k));

  const listQuery = useMemo<ListQuery<StackRow>>(
    () => ({
      data: data
        ? { items: groupRows.flatMap((g) => g.rows), total: data.total, truncated: data.capped }
        : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, groupRows, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const filtered = Boolean(search.q || search.status || search.env || search.kind);
  const clearFilters = () =>
    onSearchChange({ q: undefined, status: undefined, env: undefined, kind: undefined });
  const unmanagedCount = data?.kindCounts?.unmanaged ?? 0;
  // The Show group alone left nothing: every stack is managed, or every stack was created elsewhere.
  const kindOnly =
    search.kind !== undefined &&
    !search.q &&
    !search.status &&
    !search.env &&
    (search.kind === "unmanaged" || unmanagedCount > 0);
  // The environments whose look for unmanaged stacks failed, named where no group can carry the notice.
  const lookFailed = groupRows.filter((g) => g.group.discoveryFailed).map((g) => g.group);

  function onSort(key: string) {
    const next = key as StackSortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  // Nothing is listed. When stacks exist that the search or the filters let through none of, that is
  // said, with the way to clear it; with none anywhere, so is that. The notices of environments that
  // could not be read are shown here in place of the table, since no group is drawn for them.
  const noMatch = filtered && (data?.unfilteredTotal ?? 0) > 0;
  const empty = (
    <div className="flex flex-col gap-3">
      {by === "none"
        ? null
        : failed.map((f) => (
            <FailedEnvironmentNotice
              key={f.envId}
              failed={f}
              onRetry={retry}
              retrying={query.isFetching}
            />
          ))}
      {by === "none"
        ? null
        : lookFailed.map((g) => (
            <DiscoveryFailedNotice
              key={g.envId}
              name={g.envName}
              onRetry={retry}
              retrying={query.isFetching}
            />
          ))}
      {noMatch && kindOnly && search.kind ? (
        <StackKindEmpty kind={search.kind} onClear={clearFilters} />
      ) : noMatch ? (
        <StackNoMatch
          q={search.q}
          moreThanSearch={Boolean(search.status || search.env || search.kind)}
          onClearSearch={() => onSearchChange({ q: undefined })}
          onClear={clearFilters}
        />
      ) : (
        <Empty className="min-h-[30svh] border">
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={2} className="text-base">
              {t("stackLists.global.empty")}
            </EmptyTitle>
            <EmptyDescription>{t("stackLists.global.emptyBody")}</EmptyDescription>
          </EmptyHeader>
          {canCreate ? (
            <EmptyContent>
              <Button className="max-md:h-11" onClick={() => onCreatingChange(true)}>
                {t("stacks.create.button")}
              </Button>
            </EmptyContent>
          ) : null}
        </Empty>
      )}
    </div>
  );

  const banner =
    by === "none" && (failed.length > 0 || lookFailed.length > 0) ? (
      <div className="flex flex-col gap-2">
        {failed.map((f) => (
          <FailedEnvironmentNotice
            key={f.envId}
            failed={f}
            onRetry={retry}
            retrying={query.isFetching}
          />
        ))}
        {lookFailed.map((g) => (
          <DiscoveryFailedNotice
            key={g.envId}
            name={g.envName}
            onRetry={retry}
            retrying={query.isFetching}
          />
        ))}
      </div>
    ) : undefined;

  const groupById = useId();
  const toolbar = (
    <StackFilterBar
      q={search.q}
      status={search.status}
      counts={data?.statusCounts}
      kind={search.kind}
      kindCounts={data?.kindCounts}
      onKind={(kind) => onSearchChange({ kind })}
      filtered={filtered}
      onSearch={(q) => onSearchChange({ q })}
      onStatus={(status) => onSearchChange({ status })}
      onClear={clearFilters}
      chipRowClassName={STATUS_CHIP_ROW}
    >
      <EnvironmentFilter
        value={chosen}
        options={options}
        onChange={(env) => onSearchChange({ env: envParam(env) })}
      />
      <div className="flex items-center gap-2">
        <Label id={groupById} className="text-sm text-muted-foreground">
          {t("stackLists.groupBy.label")}
        </Label>
        <ToggleGroup
          type="single"
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-labelledby={groupById}
          value={by}
          // Pressing the one that is on would turn it off: there is always a grouping.
          onValueChange={(v) => {
            if (v) onSearchChange({ group: v === "env" ? undefined : (v as StackGroupBy) });
          }}
        >
          {(["env", "none"] as const).map((g) => (
            <ToggleGroupItem
              key={g}
              value={g}
              className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
            >
              {t(`stackLists.groupBy.${g}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      </div>
      {headed.length > 0 ? (
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="max-md:size-11"
          aria-label={t(allCollapsed ? "stackLists.expandAll" : "stackLists.collapseAll")}
          onClick={() => (allCollapsed ? collapse.expandAll() : collapse.collapseAll(headed))}
        >
          {allCollapsed ? (
            <ChevronsUpDown aria-hidden="true" />
          ) : (
            <ChevronsDownUp aria-hidden="true" />
          )}
        </Button>
      ) : null}
    </StackFilterBar>
  );

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="stacks"
        i18nKey="stackLists.global"
        columns={columns}
        query={listQuery}
        envName=""
        banner={banner}
        groups={{ items: groups, collapse }}
        toolbar={toolbar}
        caption={
          data
            ? `${t("stackUnmanaged.caption.global")} ${
                search.q || search.status || search.kind
                  ? t("stackLists.global.filtered", {
                      n: data.total,
                      count: data.unfilteredTotal,
                      environments: t("stackLists.global.environments", {
                        count: shownEnvironments,
                      }),
                    })
                  : t(
                      unmanagedCount > 0
                        ? "stackUnmanaged.caption.globalCount"
                        : "stackLists.global.count",
                      {
                        count: data.total,
                        unmanaged: unmanagedCount,
                        environments: t("stackLists.global.environments", {
                          count: shownEnvironments,
                        }),
                      },
                    )
              }`
            : undefined
        }
        empty={empty}
        capNote={data ? t("stackLists.cap", { total: data.total }) : undefined}
        sort={{ key: sortKey, dir: sortDir, onSort }}
        retryLabel={t("stackLists.global.errorRetry")}
        getRowKey={stackRowKey}
        captionExtra={
          canCreate ? (
            <div className="ml-auto">
              <Button className="max-md:h-11" onClick={() => onCreatingChange(true)}>
                <Plus aria-hidden="true" />
                {t("stacks.create.button")}
              </Button>
            </div>
          ) : null
        }
      />
      {canCreate ? <CreateStackDialog open={creating} onOpenChange={onCreatingChange} /> : null}
    </>
  );
}
