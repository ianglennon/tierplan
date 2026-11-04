// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronsDownUp, ChevronsUpDown, Ellipsis, Plus, TriangleAlert } from "lucide-react";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";

import { isNotASwarm, type ServiceItem, servicesQuery } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { NewItemsPill } from "@/components/new-items-pill";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { Toggle } from "@/components/ui/toggle";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { PortsCell, SearchBox } from "@/features/containers/container-list";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { type BulkAction, BulkBar } from "@/features/resources/bulk-bar";
import { useFrozenRows } from "@/features/resources/frozen-rows";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  type RowState,
  SelectCheckbox,
  type TableGroup,
  type TableSelect,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useChosenKeys, useSelection } from "@/features/resources/selection";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import { useEnvName } from "@/features/resources/use-env-name";
import { MuteMenuItem } from "@/features/restart-loop/mute-menu-item";
import { RestartLoopBadge } from "@/features/restart-loop/restart-loop-badge";
import { scaleEligibility } from "@/features/services/eligibility";
import { ForceUpdateDialog } from "@/features/services/force-update-dialog";
import { groupServices, sortServices } from "@/features/services/group";
import {
  SERVICE_STATUS_VALUES,
  type ServiceSearch,
  type ServiceSearchPatch,
  type ServiceSortKey,
  type ServiceStatusValue,
  serviceStatusList,
  serviceStatusParam,
} from "@/features/services/list-search";
import { NotASwarm } from "@/features/services/not-a-swarm";
import {
  RemoveServiceDialog,
  RemoveServiceMenuItem,
} from "@/features/services/remove-service-dialog";
import { RollbackDialog } from "@/features/services/rollback-dialog";
import { RollbackMenuItem } from "@/features/services/rollback-menu-item";
import { ScaleDialog, type ScaledService } from "@/features/services/scale-dialog";
import { ServiceStateCell } from "@/features/services/service-state-cell";
import { StackLink, tierplanStackIdOf } from "@/features/stacks/stack-link";
import { MAX_LIST_STATS, useListStats } from "@/features/stats/list-stats";
import { CpuCell, MemoryCell, NetworkCell } from "@/features/stats/stats-cells";
import { StatsToggle } from "@/features/stats/stats-toggle";
import { CheckNowItem } from "@/features/updates/check-now-item";
import { UpdateBadge } from "@/features/updates/update-badge";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How long a row reads "Scaling…" at most. The row goes back to the list's own word when the
// list shows the count that was asked for, or after this long, whichever comes first.
export const SCALING_MS = 30_000;

// The part of an image the list shows: the reference without its digest. The whole value,
// digest included, is in the tooltip.
function shownImage(image: string): string {
  return image.split("@")[0] ?? image;
}

function ImageCell({ image, missing }: { image: string; missing: string }) {
  if (!image) return <span className="text-muted-foreground">{missing}</span>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="block max-w-64 truncate font-mono text-xs">{shownImage(image)}</span>
      </TooltipTrigger>
      <TooltipContent className="max-w-96 break-all font-mono">{image}</TooltipContent>
    </Tooltip>
  );
}

interface RowActionsProps {
  envId: string;
  service: ServiceItem;
  onScale: (service: ServiceItem) => void;
  onForceUpdate: (service: ServiceItem) => void;
  onRollback: (service: ServiceItem) => void;
  onRemove: (service: ServiceItem) => void;
  // True while the row waits on a scale: its menu is off until the list shows the count.
  disabled?: boolean;
  // Called as the row's menu opens and closes: a list with a menu open does not move.
  onOpenChange: (open: boolean) => void;
}

// RowActions is the row menu of the services list. Open goes to the service's page; Scale… is
// for replicated services only; Force update… and Roll back… are for every service, Roll back…
// unavailable with the reason when the service has no earlier version. Remove…, last and in
// destructive text, needs Admin and is unavailable for a service a stack owns. A service in a
// restart loop also has Mute or Unmute restart-loop alerts, which a quiet service does not (the
// list does not know whether a quiet service is muted; its page does). A caller below the
// permission sees them disabled with the reason (D-19). The server decides either way.
function RowActions({
  envId,
  service,
  onScale,
  onForceUpdate,
  onRollback,
  onRemove,
  disabled,
  onOpenChange,
}: RowActionsProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const manage = useNeeds("services.manage", envId);
  const canScale = service.mode === "replicated";
  // Set when an item was chosen, so the menu does not hand focus back to its button before
  // the dialog has opened.
  const chose = useRef(false);
  return (
    <DropdownMenu onOpenChange={onOpenChange}>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="max-md:size-11"
          disabled={disabled}
          aria-label={t("actions.menu.label", { name: service.name })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="end"
        onCloseAutoFocus={(event) => {
          if (!chose.current) return;
          chose.current = false;
          event.preventDefault();
        }}
      >
        <DropdownMenuItem asChild className="max-md:min-h-11">
          <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId: service.id }}>
            {t("actions.menu.open")}
          </Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        {canScale ? (
          <NeedsTooltip reason={needs.reason}>
            <DropdownMenuItem
              className="max-md:min-h-11"
              disabled={!needs.allowed && !needs.reason}
              onSelect={() => {
                chose.current = true;
                onScale(service);
              }}
            >
              {t("services.list.menu.scale")}
            </DropdownMenuItem>
          </NeedsTooltip>
        ) : null}
        <NeedsTooltip reason={needs.reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={!needs.allowed && !needs.reason}
            onSelect={() => {
              if (!needs.allowed) return;
              chose.current = true;
              onForceUpdate(service);
            }}
          >
            {t("services.list.menu.forceUpdate")}
          </DropdownMenuItem>
        </NeedsTooltip>
        <RollbackMenuItem
          hasPreviousSpec={service.hasPreviousSpec}
          needs={needs}
          onSelect={() => {
            chose.current = true;
            onRollback(service);
          }}
        />
        {service.restartLoop ? (
          <MuteMenuItem
            envId={envId}
            serviceId={service.id}
            name={service.name}
            muted={service.restartLoop.state === "muted"}
            needs={needs}
          />
        ) : null}
        <CheckNowItem envId={envId} />
        <DropdownMenuSeparator />
        <RemoveServiceMenuItem
          stack={service.stack}
          needs={manage}
          onSelect={() => {
            chose.current = true;
            onRemove(service);
          }}
        />
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// checkedOf is a checkbox's state from a selection's: tri-state for a group.
function checkedOf(state: "checked" | "mixed" | "unchecked"): boolean | "indeterminate" {
  return state === "checked" ? true : state === "mixed" ? "indeterminate" : false;
}

interface ServiceListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to undefined leaves
  // the URL. Changes replace the history entry.
  search: ServiceSearch;
  onSearchChange: (patch: ServiceSearchPatch) => void;
}

// The modes a service can have. A row whose mode is none of these shows a dash.
const KNOWN_MODES: ReadonlySet<string> = new Set([
  "replicated",
  "global",
  "replicated-job",
  "global-job",
]);

// What a row shows that can change under the user: its state, counts, image, ports and
// last update. A change in any of them marks the row.
const fingerprintOf = (s: ServiceItem) =>
  [
    s.mode,
    s.running,
    s.desired,
    s.updateState,
    s.update?.state,
    s.image,
    (s.ports ?? []).join(","),
    s.updatedAt,
  ].join("|");

const rowKeyOf = (s: ServiceItem) => s.id;

// ServiceList is the Services page of a swarm environment (UI-SPEC "Services list"): the
// services grouped by stack, found by a search and the status chips that the server answers,
// sorted from the column headers, and kept current by the events stream. It reads the list
// the server serves from a manager; with no manager connected the last rows stay with a
// notice above them. A row reads "Scaling…" from the moment a scale is accepted until the
// list shows the count that was asked for, or for 30 seconds. Docker's names, images and
// ports are text only (T-06-14).
export function ServiceList({ envId, search, onSearchChange }: ServiceListProps) {
  const t = useT();
  const groupById = useId();
  const allowed = usePermission("services.read", envId);
  // Creating needs services.manage (Admin); the empty state says so only to those who can.
  const canCreate = usePermission("services.manage", envId);
  const createNeeds = useNeeds("services.manage", envId);
  const query = useQuery({
    ...servicesQuery(envId, { q: search.q, status: search.status, updates: search.updates }),
    enabled: allowed,
  });
  const data = query.data;
  const envName = useEnvName(envId);
  const collapse = useGroupCollapse();
  const [scaling, setScaling] = useState<readonly ServiceItem[]>([]);
  const [open, setOpen] = useState(false);
  // The service a Force update… or a Roll back… from a row menu is open for.
  const [forcing, setForcing] = useState<ServiceItem | null>(null);
  const [rollingBack, setRollingBack] = useState<ServiceItem | null>(null);
  const [removing, setRemoving] = useState<ServiceItem | null>(null);
  // The results a person asked to see from a bulk scale's Details.
  const [report, setReport] = useState<BulkReport | null>(null);
  const canScale = useNeeds("services.control", envId);
  // The count each row was last asked to have, by service ID, while it is awaited.
  const [pending, setPending] = useState<ReadonlyMap<string, number>>(new Map());
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const missing = t("dashboard.time.missing");

  // Create service is the page's one accent button (D-04). Below Admin, and while no manager is
  // connected, it is shown disabled with the reason; the server decides either way.
  const createReason =
    createNeeds.reason ?? (data?.noManager ? t("nodes.noManagerReason") : undefined);
  const createControl =
    createNeeds.allowed && !createReason ? (
      <Button asChild className="max-md:h-11">
        <Link to="/env/$envId/services/new" params={{ envId }}>
          <Plus aria-hidden="true" />
          {t("services.list.create")}
        </Link>
      </Button>
    ) : (
      <NeedsTooltip reason={createReason}>
        <Button className="max-md:h-11" disabled={createReason === undefined}>
          <Plus aria-hidden="true" />
          {t("services.list.create")}
        </Button>
      </NeedsTooltip>
    );

  const by = search.group ?? "stack";
  const sortKey: ServiceSortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";
  const statuses = serviceStatusList(search.status);

  useEffect(() => {
    const live = timers.current;
    return () => {
      for (const timer of live) clearTimeout(timer);
      live.clear();
    };
  }, []);

  const markScaling = useCallback((done: ScaledService[]) => {
    const sent = done.filter((d) => d.changed);
    if (sent.length === 0) return;
    setPending((prev) => {
      const next = new Map(prev);
      for (const d of sent) next.set(d.id, d.replicas);
      return next;
    });
    const timer = setTimeout(() => {
      timers.current.delete(timer);
      setPending((prev) => {
        const next = new Map(prev);
        for (const d of sent) if (next.get(d.id) === d.replicas) next.delete(d.id);
        return next.size === prev.size ? prev : next;
      });
    }, SCALING_MS);
    timers.current.add(timer);
  }, []);

  // A row stops waiting once the list shows the count it was asked for.
  const items = data?.items;
  useEffect(() => {
    if (!items) return;
    setPending((prev) => {
      let next: Map<string, number> | undefined;
      for (const [id, replicas] of prev) {
        if (items.find((r) => r.id === id)?.desired === replicas) {
          next ??= new Map(prev);
          next.delete(id);
        }
      }
      return next ?? prev;
    });
  }, [items]);

  // The rows in the order the list shows them. The same array until the answer or the sort
  // changes, which is what lets the freeze tell an update from a re-render.
  const sorted = useMemo(
    () => sortServices(items ?? [], { key: sortKey, dir: sortDir }),
    [items, sortKey, sortDir],
  );

  // Rows with a menu or the scale dialog open do not move: while any is open the list is
  // frozen. Changed rows still update in place, new rows wait behind the pill, and gone rows
  // stay dimmed until the freeze ends.
  const [openRows, setOpenRows] = useState<ReadonlySet<string>>(() => new Set());
  const setRowOpen = useCallback((key: string, isOpen: boolean) => {
    setOpenRows((prev) => {
      if (prev.has(key) === isOpen) return prev;
      const next = new Set(prev);
      if (isOpen) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);
  // Rows chosen for a bulk scale also hold the list still: a row that moved between choosing
  // it and pressing the button would be the wrong one.
  const chosen = useChosenKeys<string>();
  const frozen = useFrozenRows(sorted, {
    frozen: openRows.size > 0 || open || chosen[0].size > 0,
    getKey: rowKeyOf,
    fingerprint: fingerprintOf,
    // Rows of the previous filters, kept on screen while the new ones load, are not an
    // earlier answer to this question.
    ready: data !== undefined && !query.isPlaceholderData,
    resetKey: [envId, search.q, search.status, search.updates].join("\u0000"),
  });

  const shownRows = useMemo(() => frozen.rows.map((r) => r.row), [frozen.rows]);
  const rowStates = useMemo(() => {
    const out = new Map<string, RowState>();
    for (const r of frozen.rows) {
      if (r.changed || r.added || r.removed) {
        out.set(rowKeyOf(r.row), {
          changed: r.changed,
          added: r.added,
          removed: r.removed,
          leaving: r.leaving,
        });
      }
    }
    return out;
  }, [frozen.rows]);

  const baseGroups = useMemo<TableGroup<ServiceItem>[]>(
    () =>
      groupServices(shownRows, by, null).map((g) => ({
        key: g.key,
        rows: g.rows,
        header:
          g.kind === "all"
            ? undefined
            : {
                name: g.kind === "ungrouped" ? t("services.list.group.ungrouped") : g.name,
                nameLink:
                  g.kind === "stack" ? (
                    <StackLink
                      envId={envId}
                      name={g.name}
                      tierplanStackId={tierplanStackIdOf(g.rows)}
                    />
                  ) : undefined,
                summary: t("services.list.group.healthy", { healthy: g.healthy, count: g.total }),
                badge:
                  g.kind === "stack" ? (
                    <Badge variant="outline">{t("services.list.group.kind.stack")}</Badge>
                  ) : undefined,
              },
      })),
    [shownRows, by, t, envId],
  );

  // Which rows can be chosen: not gone and not waiting on a scale. A global service or a job
  // can be chosen too: the dialog lists it as skipped. The order a Shift range follows is the
  // order on screen, folded groups left out.
  const byKey = useMemo(() => new Map(shownRows.map((r) => [rowKeyOf(r), r])), [shownRows]);
  const orderedKeys = useMemo(
    () =>
      baseGroups
        .filter((g) => !g.header || !collapse.collapsed.has(g.key))
        .flatMap((g) => g.rows.map(rowKeyOf)),
    [baseGroups, collapse.collapsed],
  );
  const isEligible = useCallback(
    (key: string) => byKey.has(key) && !rowStates.get(key)?.removed && !pending.has(key),
    [byKey, rowStates, pending],
  );
  const selection = useSelection({ orderedKeys, isEligible }, chosen);

  // Live stats (D-31): one stream for the rows on screen and in a group that is open, no more
  // than the server takes. A folded group's rows are not streamed and read — until it opens.
  const statsOn = search.stats === 1;
  const statsIds = useMemo(
    () => (statsOn && allowed ? orderedKeys.slice(0, MAX_LIST_STATS) : []),
    [statsOn, allowed, orderedKeys],
  );
  const liveStats = useListStats(envId, "services", statsIds);

  const groups: TableGroup<ServiceItem>[] = baseGroups.map((g) =>
    g.header
      ? {
          ...g,
          header: {
            ...g.header,
            checkbox: (
              <SelectCheckbox
                checked={checkedOf(selection.groupState(g.rows.map(rowKeyOf)))}
                label={t("actions.select.group", { name: g.header.name })}
                onToggle={() => selection.selectGroup(g.rows.map(rowKeyOf))}
              />
            ),
          },
        }
      : g,
  );

  const changesWaiting = frozen.heldCount + frozen.removedCount;
  const headed = groups.filter((g) => g.header).map((g) => g.key);
  const allCollapsed = headed.length > 0 && headed.every((k) => collapse.collapsed.has(k));

  const filtered = Boolean(search.q || search.status || search.updates);
  const clearFilters = () =>
    onSearchChange({ q: undefined, status: undefined, updates: undefined });

  function onSort(key: string) {
    const next = key as ServiceSortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  // The table reads a list answer with a `truncated` flag; the services list calls it capped.
  const listQuery = useMemo<ListQuery<ServiceItem>>(
    () => ({
      data: data ? { items: data.items, total: data.total, truncated: data.capped } : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  // Search alone gets its own empty state; a chip makes it a filter match, since the filter
  // may be the cause (UI-SPEC "Core elements"). With neither set, someone who can create a
  // service is told how; everyone else gets the table's plain "nothing to show".
  const searchOnly = Boolean(search.q) && !search.status && !search.updates;
  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {searchOnly
            ? t("services.list.emptySearch.title", { q: search.q })
            : t("services.list.emptyFilters.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button
          variant="outline"
          className="max-md:h-11"
          onClick={() => (searchOnly ? onSearchChange({ q: undefined }) : clearFilters())}
        >
          {searchOnly
            ? t("services.list.emptySearch.action")
            : t("services.list.emptyFilters.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : canCreate ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("services.list.empty")}
        </EmptyTitle>
        <EmptyDescription>{t("services.list.emptyCreateBody")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>{createControl}</EmptyContent>
    </Empty>
  ) : undefined;

  // The checkbox column. A row gone or waiting on a scale has none to tick.
  const select: TableSelect<ServiceItem> = {
    header: (
      <SelectCheckbox
        checked={checkedOf(selection.allState)}
        label={t("services.list.select.all")}
        disabled={selection.eligibleCount === 0}
        onToggle={() => selection.selectAll()}
      />
    ),
    cell: (s) => {
      const key = rowKeyOf(s);
      return (
        <SelectCheckbox
          checked={selection.isSelected(key)}
          label={t("actions.select.row", { name: s.name })}
          disabled={Boolean(rowStates.get(key)?.removed) || pending.has(key)}
          onToggle={(shift) => selection.toggle(key, { shift })}
        />
      );
    },
    onEscape: selection.selected.size > 0 ? selection.clear : undefined,
  };

  // Scale… is the one bulk action of this phase (D-10). It cannot be used without Operator,
  // or when no chosen row is a replicated service.
  const selectedRows = shownRows.filter((r) => selection.isSelected(rowKeyOf(r)));
  const scaleReason = !canScale.allowed
    ? (canScale.reason ?? t("needs.role.operator"))
    : selectedRows.some((r) => scaleEligibility(r).eligible)
      ? undefined
      : t("services.list.bulk.none");
  const actions: BulkAction[] = [
    {
      id: "scale",
      label: t("services.list.menu.scale"),
      reason: scaleReason,
      onClick: () => {
        setScaling(selectedRows);
        setOpen(true);
      },
    },
  ];
  // Offered when every visible row that can be chosen is, and folded groups hold more.
  const everyKey = shownRows.map(rowKeyOf).filter(isEligible);
  const offerAll =
    selection.allState === "checked" && selection.selected.size < everyKey.length
      ? { count: everyKey.length, onSelect: () => selection.selectEvery(everyKey) }
      : undefined;

  const statsColumns: Column<ServiceItem>[] = statsOn
    ? [
        {
          id: "cpu",
          header: t("stats.tiles.cpu"),
          className: "w-24 min-w-24 text-end",
          cell: (s) => <CpuCell stat={liveStats.get(s.id)} />,
        },
        {
          id: "memory",
          header: t("stats.tiles.memory"),
          className: "w-36 min-w-36 text-end",
          cell: (s) => <MemoryCell stat={liveStats.get(s.id)} />,
        },
        {
          id: "network",
          header: t("stats.tiles.network"),
          className: "w-48 min-w-48 text-end",
          cell: (s) => <NetworkCell stat={liveStats.get(s.id)} />,
        },
      ]
    : [];

  const columns: Column<ServiceItem>[] = [
    {
      id: "name",
      header: t("services.list.columns.name"),
      className: "min-w-[224px]",
      sortKey: "name",
      cell: (s) => (
        <span className="flex items-center gap-2">
          <Link
            to="/env/$envId/services/$serviceId"
            params={{ envId, serviceId: s.id }}
            className="min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{s.name}</TruncatedText>
          </Link>
          {s.agent ? <SelfContainerBadge kind="agent-service" /> : null}
          {s.restartLoop ? <RestartLoopBadge loop={s.restartLoop} /> : null}
        </span>
      ),
    },
    {
      id: "state",
      header: t("services.list.columns.state"),
      className: "w-40 min-w-40",
      sortKey: "state",
      cell: (s) => (
        <ServiceStateCell service={s} pending={pending.has(s.id) ? "scale" : undefined} />
      ),
    },
    {
      id: "updates",
      header: t("updates.list.column"),
      className: "w-44 min-w-44",
      // The server's word for the row; a row it sent no update object for shows a dash, never "Up to date".
      cell: (s) => (s.update ? <UpdateBadge info={s.update} /> : <span>{missing}</span>),
    },
    {
      id: "mode",
      header: t("services.list.columns.mode"),
      className: "w-32 min-w-32",
      cell: (s) => (KNOWN_MODES.has(s.mode) ? t(`services.list.mode.${s.mode}`) : missing),
    },
    {
      id: "replicas",
      header: t("services.list.columns.replicas"),
      className: "w-24 min-w-24 text-end tabular-nums",
      cell: (s) => {
        // A job runs to completion and has no count to show.
        if (s.mode === "replicated-job" || s.mode === "global-job") return <span>{missing}</span>;
        // A scale waiting to show shows the count it was asked for ("2/5").
        return <span>{`${s.running}/${pending.get(s.id) ?? s.desired}`}</span>;
      },
    },
    {
      id: "image",
      header: t("services.list.columns.image"),
      cell: (s) => <ImageCell image={s.image} missing={missing} />,
    },
    {
      id: "ports",
      header: t("services.list.columns.ports"),
      className: "w-40 min-w-40",
      cell: (s) => (
        <PortsCell ports={(s.ports ?? []).map((p) => p.replace("->", "→"))} missing={missing} />
      ),
    },
    {
      id: "updated",
      header: t("services.list.columns.updated"),
      className: "w-[120px] min-w-[120px]",
      sortKey: "updated",
      cell: (s) => <RelativeTime value={s.updatedAt} />,
    },
    ...statsColumns,
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (s) => (
        <RowActions
          envId={envId}
          service={s}
          disabled={pending.has(s.id)}
          onOpenChange={(isOpen) => setRowOpen(rowKeyOf(s), isOpen)}
          onScale={(service) => {
            setScaling([service]);
            setOpen(true);
          }}
          onForceUpdate={setForcing}
          onRollback={setRollingBack}
          onRemove={setRemoving}
        />
      ),
    },
  ];

  const banner = data?.noManager ? (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("services.list.noManager.title")}</AlertTitle>
      <AlertDescription className={cn("text-foreground")}>
        {t("services.list.noManager.body")}
      </AlertDescription>
    </Alert>
  ) : undefined;

  const toolbar = (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox
          value={search.q}
          onCommit={(q) => onSearchChange({ q })}
          label={t("services.list.search.label")}
          placeholder={t("services.list.search.placeholder")}
          clearLabel={t("services.list.search.clear")}
        />
        <div className="flex items-center gap-2">
          <Label id={groupById} className="text-sm text-muted-foreground">
            {t("services.list.groupBy.label")}
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
              if (v) onSearchChange({ group: v === "stack" ? undefined : (v as "none") });
            }}
          >
            {(["stack", "none"] as const).map((g) => (
              <ToggleGroupItem
                key={g}
                value={g}
                className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
              >
                {t(`services.list.groupBy.${g}`)}
              </ToggleGroupItem>
            ))}
          </ToggleGroup>
        </div>
        <StatsToggle
          checked={statsOn}
          onCheckedChange={(on) => onSearchChange({ stats: on ? 1 : undefined })}
        />
        {headed.length > 0 ? (
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="max-md:size-11"
            aria-label={t(allCollapsed ? "services.list.expandAll" : "services.list.collapseAll")}
            onClick={() => (allCollapsed ? collapse.expandAll() : collapse.collapseAll(headed))}
          >
            {allCollapsed ? (
              <ChevronsUpDown aria-hidden="true" />
            ) : (
              <ChevronsDownUp aria-hidden="true" />
            )}
          </Button>
        ) : null}
        {filtered ? (
          <Button variant="ghost" className="max-md:h-11" onClick={clearFilters}>
            {t("services.list.clearFilters")}
          </Button>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <ToggleGroup
          type="multiple"
          // The chips are toggle buttons in a labelled group, not a toolbar.
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-label={t("services.list.status.label")}
          value={statuses}
          onValueChange={(v) =>
            onSearchChange({ status: serviceStatusParam(v as ServiceStatusValue[]) })
          }
          className={STATUS_CHIP_ROW}
        >
          {SERVICE_STATUS_VALUES.map((v) => {
            const label = t(`services.list.status.${v}`);
            return (
              <ToggleGroupItem
                key={v}
                value={v}
                className="aria-pressed:border-ring aria-pressed:text-ring max-md:h-11"
              >
                {data
                  ? t("services.list.status.chip", { label, count: data.statusCounts?.[v] ?? 0 })
                  : label}
              </ToggleGroupItem>
            );
          })}
        </ToggleGroup>
        <Toggle
          variant="outline"
          size="sm"
          className="aria-pressed:border-ring aria-pressed:text-ring max-md:h-11"
          pressed={search.updates === "available"}
          onPressedChange={(on) => onSearchChange({ updates: on ? "available" : undefined })}
        >
          {t("updates.list.filter")}
        </Toggle>
      </div>
    </div>
  );

  if (!allowed) return <NoPermission />;
  if (isNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;
  return (
    <>
      <ResourceTable
        resource="services"
        i18nKey="services.list"
        columns={columns}
        query={listQuery}
        envName={envName}
        banner={banner}
        groups={{ items: groups, collapse }}
        toolbar={toolbar}
        caption={
          data && filtered
            ? t("services.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
            : undefined
        }
        empty={empty}
        actions={<div className="ml-auto">{createControl}</div>}
        capNote={data ? t("services.list.cap", { total: data.total }) : undefined}
        retryLabel={t("services.list.errorRetry")}
        captionExtra={
          <NewItemsPill
            count={changesWaiting}
            label={t("services.list.changes", { count: changesWaiting })}
            action={t("services.list.showChanges")}
            onClick={frozen.release}
          />
        }
        sort={{ key: sortKey, dir: sortDir, onSort }}
        rowStates={rowStates}
        getRowKey={rowKeyOf}
        select={select}
        bulkBar={
          selection.selected.size > 0 ? (
            <BulkBar
              count={selection.selected.size}
              actions={actions}
              onClear={selection.clear}
              selectAll={offerAll}
            />
          ) : undefined
        }
      />
      <ScaleDialog
        envId={envId}
        targets={scaling}
        open={open}
        onOpenChange={setOpen}
        onScaled={(done) => {
          markScaling(done);
          selection.clear();
        }}
        onDetails={setReport}
      />
      {forcing ? (
        <ForceUpdateDialog
          envId={envId}
          target={forcing}
          open
          onOpenChange={(isOpen) => {
            if (!isOpen) setForcing(null);
          }}
        />
      ) : null}
      {rollingBack ? (
        <RollbackDialog
          envId={envId}
          target={rollingBack}
          open
          onOpenChange={(isOpen) => {
            if (!isOpen) setRollingBack(null);
          }}
        />
      ) : null}
      {removing ? (
        <RemoveServiceDialog envId={envId} target={removing} onClose={() => setRemoving(null)} />
      ) : null}
      <BulkResultDialog
        envId={envId}
        report={report}
        labels={{ target: t("services.scale.results.service"), node: null }}
        onClose={() => setReport(null)}
      />
    </>
  );
}
