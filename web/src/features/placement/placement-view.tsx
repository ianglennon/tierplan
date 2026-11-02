// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { LayoutGrid } from "lucide-react";
import { useId, useState } from "react";

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
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { SearchBox } from "@/features/containers/container-list";
import { ColumnsSkeleton, NodeColumns } from "@/features/placement/node-columns";
import { NodeServiceGrid } from "@/features/placement/node-service-grid";
import { PlacementRulesCard } from "@/features/placement/placement-rules-card";
import type { PlacementSearch, PlacementSearchPatch } from "@/features/placement/placement-search";
import {
  filterTasks,
  isFiltered,
  NO_STACK,
  type PlacementData,
  project,
  stackNames,
} from "@/features/placement/project";
import { useEnvName } from "@/features/resources/use-env-name";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { useNodeStats } from "@/features/stats/list-stats";
import { StatsToggle } from "@/features/stats/stats-toggle";
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
  // Narrow the view to this service (its Placement tab).
  serviceId?: string;
}

// PlacementView is where the tasks of a swarm run (RES-12, D-18, D-19), in one of two layouts:
// a column per node, or the node x service grid. It is the Placement page and, with a serviceId,
// the Placement tab of that service: the same view for the one service, under a header and the
// rules card, with the node columns kept when they are empty. The toolbar is drawn while the
// answer loads. The stack and service filters are in the URL and narrow the tasks, never the
// nodes. While a grid cell's popover is open the answer on screen is held, so nothing moves
// under the pointer (UI-SPEC "Live updates").
export function PlacementView({ envId, search, onSearchChange, serviceId }: PlacementViewProps) {
  const t = useT();
  const stackId = useId();
  const layoutId = useId();
  const allowed = usePermission("services.read", envId);
  const query = useQuery({ ...placementQuery(envId, serviceId), enabled: allowed });
  const envName = useEnvName(envId);
  const [held, setHeld] = useState<PlacementData | null>(null);
  const live = query.data;
  const data = held ?? live;
  // The answer for one service is already narrowed, so the page's filters do not apply to it.
  const filters = serviceId ? {} : { stack: search.stack, q: search.q };
  const filtered = isFiltered(filters);
  const grid = search.view === "grid";
  // Live stats (D-31) adds bars to the node columns; the grid has none, so it streams nothing.
  const statsOn = search.stats === 1 && !grid;
  const nodeStats = useNodeStats(envId, statsOn && allowed);

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
  const matched = data ? filterTasks(data, filters).length : 0;
  const service = data?.services?.[0];
  const serviceName = service?.name ?? serviceId ?? "";
  // A popover holds the answer it was opened on; closing it lets the page catch up.
  const hold = (open: boolean) => setHeld(open ? (live ?? null) : null);

  const toolbar = (
    <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
      {serviceId ? null : (
        <>
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
        </>
      )}
      <div className="flex items-center gap-2">
        <Label id={layoutId} className="text-sm text-muted-foreground">
          {t("placement.layout.label")}
        </Label>
        <ToggleGroup
          type="single"
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-labelledby={layoutId}
          value={grid ? "grid" : "nodes"}
          // Pressing the one that is on would turn it off: there is always a layout. The
          // filters are not touched, so they carry over to the other layout.
          onValueChange={(v) => {
            if (v) onSearchChange({ view: v === "grid" ? "grid" : undefined });
          }}
        >
          <ToggleGroupItem
            value="nodes"
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {t("placement.layout.nodes")}
          </ToggleGroupItem>
          <ToggleGroupItem
            value="grid"
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {t("placement.layout.grid")}
          </ToggleGroupItem>
        </ToggleGroup>
      </div>
      {grid ? null : (
        <StatsToggle
          checked={statsOn}
          onCheckedChange={(on) => onSearchChange({ stats: on ? 1 : undefined })}
          hint={t("stats.placement.toggleHint")}
        />
      )}
      {filtered && !serviceId ? (
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
  } else if (none && !serviceId) {
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
  } else if (matched === 0 && !serviceId) {
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
  } else if (grid) {
    body = (
      <NodeServiceGrid
        envId={envId}
        data={data}
        filters={filters}
        keepServices={serviceId !== undefined}
        onPopoverOpenChange={hold}
      />
    );
  } else {
    body = (
      <NodeColumns
        envId={envId}
        columns={columns}
        stats={statsOn ? nodeStats : undefined}
        emptyLabel={serviceId ? t("placement.column.noneOf", { name: serviceName }) : undefined}
      />
    );
  }

  return (
    <div className="flex w-full flex-col gap-4">
      {serviceId ? (
        <>
          {data || failed ? (
            <h3 className="text-base font-semibold break-all">
              {t("placement.serviceTab.title", { name: serviceName })}
            </h3>
          ) : (
            <Skeleton className="h-5 w-48" />
          )}
          {service && data ? (
            <PlacementRulesCard service={service} nodes={data.nodes ?? []} />
          ) : null}
        </>
      ) : (
        <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <h1 className="text-2xl font-semibold">{t("placement.title")}</h1>
          <span className="text-sm text-muted-foreground">
            {t("placement.caption", { environment: envName })}
          </span>
        </header>
      )}
      {toolbar}
      {body}
    </div>
  );
}
