// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronsDownUp, ChevronsUpDown, Ellipsis, TriangleAlert } from "lucide-react";
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
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { PortsCell, SearchBox } from "@/features/containers/container-list";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { useFrozenRows } from "@/features/resources/frozen-rows";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  type RowState,
  type TableGroup,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
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
import { ScaleDialog, type ScaledService } from "@/features/services/scale-dialog";
import { ServiceStateCell } from "@/features/services/service-state-cell";
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
  // Called as the row's menu opens and closes: a list with a menu open does not move.
  onOpenChange: (open: boolean) => void;
}

// RowActions is the row menu of the services list. Open is there but off until the service's
// page exists; Scale… is for replicated services only, and a caller below Operator sees it
// disabled with the reason (D-19). The server decides either way.
function RowActions({ envId, service, onScale, onOpenChange }: RowActionsProps) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
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
        <DropdownMenuItem disabled className="max-md:min-h-11">
          {t("actions.menu.open")}
        </DropdownMenuItem>
        {canScale ? (
          <>
            <DropdownMenuSeparator />
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
          </>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
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
  const query = useQuery({
    ...servicesQuery(envId, { q: search.q, status: search.status }),
    enabled: allowed,
  });
  const data = query.data;
  const envName = useEnvName(envId);
  const collapse = useGroupCollapse();
  const [scaling, setScaling] = useState<ServiceItem | null>(null);
  const [open, setOpen] = useState(false);
  // The count each row was last asked to have, by service ID, while it is awaited.
  const [pending, setPending] = useState<ReadonlyMap<string, number>>(new Map());
  const timers = useRef(new Set<ReturnType<typeof setTimeout>>());
  const missing = t("dashboard.time.missing");

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
  const frozen = useFrozenRows(sorted, {
    frozen: openRows.size > 0 || open,
    getKey: rowKeyOf,
    fingerprint: fingerprintOf,
    // Rows of the previous filters, kept on screen while the new ones load, are not an
    // earlier answer to this question.
    ready: data !== undefined && !query.isPlaceholderData,
    resetKey: [envId, search.q, search.status].join("\u0000"),
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

  const groups = useMemo<TableGroup<ServiceItem>[]>(
    () =>
      groupServices(shownRows, by, null).map((g) => ({
        key: g.key,
        rows: g.rows,
        header:
          g.kind === "all"
            ? undefined
            : {
                name: g.kind === "ungrouped" ? t("services.list.group.ungrouped") : g.name,
                summary: t("services.list.group.healthy", { healthy: g.healthy, count: g.total }),
                badge:
                  g.kind === "stack" ? (
                    <Badge variant="outline">{t("services.list.group.kind.stack")}</Badge>
                  ) : undefined,
              },
      })),
    [shownRows, by, t],
  );

  const changesWaiting = frozen.heldCount + frozen.removedCount;
  const headed = groups.filter((g) => g.header).map((g) => g.key);
  const allCollapsed = headed.length > 0 && headed.every((k) => collapse.collapsed.has(k));

  const filtered = Boolean(search.q || search.status);
  const clearFilters = () => onSearchChange({ q: undefined, status: undefined });

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
  const searchOnly = Boolean(search.q) && !search.status;
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
    </Empty>
  ) : undefined;

  const columns: Column<ServiceItem>[] = [
    {
      id: "name",
      header: t("services.list.columns.name"),
      className: "min-w-[224px]",
      sortKey: "name",
      cell: (s) => (
        <span className="flex items-center gap-2">
          <TruncatedText className="min-w-0">{s.name}</TruncatedText>
          {s.agent ? <SelfContainerBadge kind="agent-service" /> : null}
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
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (s) => (
        <RowActions
          envId={envId}
          service={s}
          onOpenChange={(isOpen) => setRowOpen(rowKeyOf(s), isOpen)}
          onScale={(service) => {
            setScaling(service);
            setOpen(true);
          }}
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
        className="max-w-full max-md:w-full max-md:flex-nowrap max-md:overflow-x-auto md:flex-wrap"
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
      />
      <ScaleDialog
        envId={envId}
        targets={scaling ? [scaling] : []}
        open={open}
        onOpenChange={setOpen}
        onScaled={markScaling}
      />
    </>
  );
}
