// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { LayoutGrid } from "lucide-react";
import { useId } from "react";

import { placementQuery } from "@/api/placement";
import { isNotASwarm } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import { ColumnsSkeleton, NodeColumns } from "@/features/placement/node-columns";
import type { PlacementSearch, PlacementSearchPatch } from "@/features/placement/placement-search";
import { isFiltered, NO_STACK, project, stackNames } from "@/features/placement/project";
import { useEnvName } from "@/features/resources/use-env-name";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The Select cannot hold an empty value, and a stack is whatever its owner named it, so each
// stack is prefixed and the two fixed choices are the values without the prefix.
const ALL_STACKS = "all";
const STACK_PREFIX = "stack:";

function toSelect(stack: string | undefined): string {
  if (stack === undefined) return ALL_STACKS;
  return stack === NO_STACK ? NO_STACK : `${STACK_PREFIX}${stack}`;
}

function fromSelect(value: string): string | undefined {
  if (value === ALL_STACKS) return undefined;
  return value === NO_STACK ? NO_STACK : value.slice(STACK_PREFIX.length);
}

interface PlacementViewProps {
  envId: string;
  search: PlacementSearch;
  onSearchChange: (patch: PlacementSearchPatch) => void;
}

// PlacementView is the Placement page (RES-12, D-18, D-19): where the tasks of a swarm run, one
// column per node. The toolbar is drawn while the answer loads. The stack and service filters
// are in the URL and narrow the tasks, never the nodes.
export function PlacementView({ envId, search, onSearchChange }: PlacementViewProps) {
  const t = useT();
  const stackId = useId();
  const layoutId = useId();
  const allowed = usePermission("services.read", envId);
  const query = useQuery({ ...placementQuery(envId), enabled: allowed });
  const envName = useEnvName(envId);
  const data = query.data;
  const filters = { stack: search.stack, q: search.q };
  const filtered = isFiltered(filters);

  if (!allowed) return <NoPermission />;
  if (isNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;

  const stacks = data ? stackNames(data) : [];
  // The stack in the URL stays selectable even when no task is left in it, so the select never
  // shows a blank.
  const choices =
    search.stack && search.stack !== NO_STACK && !stacks.includes(search.stack)
      ? [...stacks, search.stack]
      : stacks;
  const clearFilters = () => onSearchChange({ stack: undefined, q: undefined });
  const columns = data ? project(data, filters) : [];
  const failed = query.isError && !data;
  const none = data !== undefined && (data.tasks ?? []).length === 0;
  const matched = columns.reduce((n, c) => n + c.count, 0);

  const toolbar = (
    <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
      <SearchBox
        value={search.q}
        onCommit={(q) => onSearchChange({ q })}
        label={t("placement.filters.search.label")}
        placeholder={t("placement.filters.search.placeholder")}
        clearLabel={t("placement.filters.search.clear")}
      />
      <div className="flex items-center gap-2">
        <Label htmlFor={stackId} className="text-sm text-muted-foreground">
          {t("placement.filters.stack")}
        </Label>
        <Select
          value={toSelect(search.stack)}
          onValueChange={(v) => onSearchChange({ stack: fromSelect(v) })}
        >
          <SelectTrigger id={stackId} size="sm" className="max-w-64 min-w-40 max-md:h-11">
            <SelectValue />
          </SelectTrigger>
          <SelectContent position="popper" align="start" className="max-h-72">
            <SelectItem value={ALL_STACKS}>{t("placement.filters.allStacks")}</SelectItem>
            {choices.map((s) => (
              <SelectItem key={s} value={`${STACK_PREFIX}${s}`}>
                {s}
              </SelectItem>
            ))}
            <SelectItem value={NO_STACK}>{t("placement.filters.noStack")}</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className="flex items-center gap-2">
        <Label id={layoutId} className="text-sm text-muted-foreground">
          {t("placement.layout.label")}
        </Label>
        {/* Only By node for now: the grid joins this group when it exists. */}
        <ToggleGroup
          type="single"
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-labelledby={layoutId}
          value="nodes"
          // Pressing the one that is on would turn it off: there is always a layout.
          onValueChange={(v) => {
            if (v) onSearchChange({ view: undefined });
          }}
        >
          <ToggleGroupItem
            value="nodes"
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {t("placement.layout.nodes")}
          </ToggleGroupItem>
        </ToggleGroup>
      </div>
      {filtered ? (
        <Button variant="ghost" className="max-md:h-11" onClick={clearFilters}>
          {t("placement.filters.clear")}
        </Button>
      ) : null}
    </div>
  );

  let body: React.ReactNode;
  if (failed) {
    body = (
      <Alert variant="destructive">
        <AlertTitle>{t("placement.error.title")}</AlertTitle>
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("placement.error.body")}</span>
          <Button
            variant="outline"
            size="sm"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {query.isFetching ? <Spinner aria-hidden="true" /> : null}
            {t("placement.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  } else if (!data) {
    body = <ColumnsSkeleton />;
  } else if (none) {
    body = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <LayoutGrid aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t("placement.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("placement.empty.body", { environment: envName })}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else if (matched === 0) {
    body = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t("placement.filteredEmpty.title")}
          </EmptyTitle>
        </EmptyHeader>
        <EmptyContent>
          <Button variant="outline" onClick={clearFilters}>
            {t("placement.filteredEmpty.action")}
          </Button>
        </EmptyContent>
      </Empty>
    );
  } else {
    body = <NodeColumns envId={envId} columns={columns} />;
  }

  return (
    <div className="flex w-full flex-col gap-4">
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h1 className="text-2xl font-semibold">{t("placement.title")}</h1>
        <span className="text-sm text-muted-foreground">
          {t("placement.caption", { environment: envName })}
        </span>
      </header>
      {toolbar}
      {body}
    </div>
  );
}
