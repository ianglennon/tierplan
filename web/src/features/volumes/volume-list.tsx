// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ChevronsDownUp, ChevronsUpDown, HardDrive, Plus } from "lucide-react";
import { useId, useMemo, useState } from "react";
import { networksQuery, type VolumeRow, volumesQuery } from "@/api/resources";
import { sizeKey, useVolumeSizes } from "@/api/volumes";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { SearchBox } from "@/features/containers/container-list";
import { GROUP_VALUES, type GroupBy, MAX_QUERY } from "@/features/containers/list-search";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { type BulkAction, BulkBar } from "@/features/resources/bulk-bar";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  SelectCheckbox,
  type TableGroup,
  type TableSelect,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useSelection } from "@/features/resources/selection";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import {
  nodeRowKey,
  nodeSearchSchema,
  useEnvIsSwarm,
  useNodeColumn,
} from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { CreateVolumeDialog } from "@/features/volumes/create-volume-dialog";
import { DeleteVolumesDialog } from "@/features/volumes/delete-volumes-dialog";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastError, toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The usage filter, as it is written in the URL. Without it every volume is listed.
export const USAGE_VALUES = ["used", "unused"] as const;
export type UsageFilter = (typeof USAGE_VALUES)[number];

// The volumes route's search (UI-SPEC "Screens"): the node, the search text, the usage
// filter and how the rows are grouped. Every value that does not fit is dropped, not
// sent, and a missing group means the default, stack or project.
export const volumeSearchSchema = nodeSearchSchema.extend({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  usage: z.enum(USAGE_VALUES).optional().catch(undefined),
  group: z.enum(GROUP_VALUES).optional().catch(undefined),
});

export type VolumeSearch = z.output<typeof volumeSearchSchema>;

// A change to the search: a key set to undefined leaves the URL.
export type VolumeSearchPatch = { [K in keyof VolumeSearch]?: VolumeSearch[K] | undefined };

const rowKeyOf = (v: VolumeRow) => nodeRowKey(v.node, v.name);

// A volume is in use when any container on its node or any swarm service mounts it.
const inUse = (v: VolumeRow) => v.usedByContainers + v.usedByServices > 0;

// Case-insensitive first, so "Web" and "api" sort as a person reads them, then the exact
// text so the order never depends on how the rows arrived.
function byText(a: string, b: string): number {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a === b ? 0 : a < b ? -1 : 1;
}

const byName = (a: VolumeRow, b: VolumeRow) =>
  byText(a.name, b.name) || byText(a.node ?? "", b.node ?? "");

type GroupKind = "stack" | "project" | "node" | "ungrouped" | "all";

interface VolumeGroup {
  key: string;
  name: string;
  kind: GroupKind;
  rows: VolumeRow[];
}

function groupKeyOf(row: VolumeRow, by: GroupBy): { key: string; name: string; kind: GroupKind } {
  if (by === "node") {
    return row.node
      ? { key: `node:${row.node}`, name: row.node, kind: "node" }
      : { key: "ungrouped", name: "", kind: "ungrouped" };
  }
  if (row.stack) return { key: `stack:${row.stack}`, name: row.stack, kind: "stack" };
  if (row.project) return { key: `project:${row.project}`, name: row.project, kind: "project" };
  return { key: "ungrouped", name: "", kind: "ungrouped" };
}

// groupVolumes sorts the rows into the groups of the list (D-45). By stack or project, a
// volume sits under its swarm stack when it has one, else under its compose project, else
// under Ungrouped; by node, under the node that holds it. Groups are ordered by name
// without regard to case with Ungrouped last, and rows within one by name, then node.
export function groupVolumes(rows: readonly VolumeRow[], by: GroupBy): VolumeGroup[] {
  const ordered = [...rows].sort(byName);
  if (by === "none") {
    return ordered.length === 0 ? [] : [{ key: "all", name: "", kind: "all", rows: ordered }];
  }
  const groups = new Map<string, VolumeGroup>();
  for (const row of ordered) {
    const { key, name, kind } = groupKeyOf(row, by);
    let group = groups.get(key);
    if (!group) {
      group = { key, name, kind, rows: [] };
      groups.set(key, group);
    }
    group.rows.push(row);
  }
  return [...groups.values()].sort((a, b) => {
    if (a.kind === "ungrouped" || b.kind === "ungrouped") {
      return a.kind === b.kind ? 0 : a.kind === "ungrouped" ? 1 : -1;
    }
    return byText(a.name, b.name) || byText(a.kind, b.kind);
  });
}

// checkedOf is a checkbox's state from a selection's: tri-state for a group.
function checkedOf(state: "checked" | "mixed" | "unchecked"): boolean | "indeterminate" {
  return state === "checked" ? true : state === "mixed" ? "indeterminate" : false;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// usedWhat words what uses a volume: "2 containers", "1 service" or both. Services exist
// only on a swarm.
function usedWhat(t: Translate, v: VolumeRow, swarm: boolean): string {
  const containers = t("volumes.list.used.containers", { count: v.usedByContainers });
  const services = t("volumes.list.used.services", { count: v.usedByServices });
  if (swarm) return t("volumes.list.used.both", { containers, services });
  return containers;
}

// inUseWhat words what keeps a volume in use for the reason on a disabled Delete…:
// only the kinds that are there, so a count of zero is never named.
function inUseWhat(t: Translate, v: VolumeRow): string {
  const parts = [
    v.usedByContainers > 0
      ? t("volumes.list.used.containers", { count: v.usedByContainers })
      : null,
    v.usedByServices > 0 ? t("volumes.list.used.services", { count: v.usedByServices }) : null,
  ].filter((p): p is string => p !== null);
  return parts.length === 2
    ? t("volumes.list.used.and", { first: parts[0], second: parts[1] })
    : (parts[0] ?? "");
}

interface VolumeListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to undefined
  // leaves the URL. Changes replace the history entry.
  search: VolumeSearch;
  onSearchChange: (patch: VolumeSearchPatch) => void;
}

// VolumeList is the volumes list of an environment: what uses each volume, which are
// anonymous, grouped by stack or project, found by a search and the usage filter that the
// server answers. For a holder of the delete volumes permission it deletes one volume
// from its row and several from the bulk bar; someone who may read but not delete sees
// both disabled with the reason, and a volume in use cannot be chosen (D-45, D-46). For
// an env Admin it has the Create volume button (D-44, D-51).
export function VolumeList({ envId, search, onSearchChange }: VolumeListProps) {
  const t = useT();
  const key = "dashboard.lists.volumes";
  const groupById = useId();
  const allowed = usePermission("volumes.read", envId);
  const create = useNeeds("volumes.create", envId);
  const remove = useNeeds("volumes.delete", envId);
  const swarm = useEnvIsSwarm(envId);
  const collapse = useGroupCollapse();
  const [creating, setCreating] = useState(false);
  // The rows a delete is about stay while the dialog closes, so its title never reads
  // "Delete ?" on the way out.
  const [deleting, setDeleting] = useState<{ rows: VolumeRow[]; open: boolean }>({
    rows: [],
    open: false,
  });
  // The results a person asked to see from a toast's Details.
  const [report, setReport] = useState<BulkReport | null>(null);
  const nodeColumn = useNodeColumn<VolumeRow>();
  const node = swarm ? search.node : undefined;
  const query = useQuery({
    ...volumesQuery(envId, { node, q: search.q, usage: search.usage }),
    enabled: allowed,
  });
  const data = query.data;
  // The create dialog offers every node and checks a name against every node's volumes,
  // so while the list is narrowed it reads the whole list. The local networks name the
  // nodes that hold no volume yet.
  const narrowed = node !== undefined || Boolean(search.q) || search.usage !== undefined;
  const everything = useQuery({
    ...volumesQuery(envId),
    enabled: allowed && narrowed && creating,
  });
  const nodeSource = useQuery({
    ...networksQuery(envId),
    enabled: allowed && swarm && creating,
  });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");
  // Sizes are worked out when asked and kept until asked again: the engine walks every
  // volume's files to answer, so no refresh does it (D-47).
  const sizing = useVolumeSizes(envId);
  const calculate = () =>
    sizing.mutate(undefined, {
      onSuccess: () => toastSuccess(t("volumes.sizes.done")),
      onError: () => toastError(t("volumes.sizes.error")),
    });
  const sizeCell = (v: VolumeRow) => {
    const size = sizing.data?.sizes.get(sizeKey(v.nodeId, v.name));
    if (size !== undefined) return <span className="tabular-nums">{formatBytes(size)}</span>;
    const failedNode = sizing.data?.failed.get(v.nodeId);
    if (failedNode === undefined) return missing;
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <button
            type="button"
            className="cursor-help rounded-sm focus-visible:ring-2 focus-visible:ring-ring"
            aria-label={`${missing} ${t("volumes.sizes.failedOn", { node: failedNode })}`}
          >
            {missing}
          </button>
        </TooltipTrigger>
        <TooltipContent>{t("volumes.sizes.failedOn", { node: failedNode })}</TooltipContent>
      </Tooltip>
    );
  };

  // Grouping by node means nothing on a standalone host, which has one.
  const by: GroupBy = search.group === "node" && !swarm ? "stack" : (search.group ?? "stack");
  const items = data?.items;
  const baseGroups = useMemo<TableGroup<VolumeRow>[]>(
    () =>
      groupVolumes(items ?? [], by).map((g) => ({
        key: g.key,
        rows: g.rows,
        header:
          g.kind === "all"
            ? undefined
            : {
                name: g.kind === "ungrouped" ? t("containers.list.group.ungrouped") : g.name,
                summary: t("volumes.list.group.count", { count: g.rows.length }),
                badge:
                  g.kind === "stack" || g.kind === "project" ? (
                    <Badge variant="outline">{t(`containers.list.group.kind.${g.kind}`)}</Badge>
                  ) : undefined,
              },
      })),
    [items, by, t],
  );

  // Which rows can be chosen: not one in use. The order a Shift range follows is the
  // order on screen, folded groups left out.
  const byKey = useMemo(() => new Map((items ?? []).map((r) => [rowKeyOf(r), r])), [items]);
  const orderedKeys = useMemo(
    () =>
      baseGroups
        .filter((g) => !g.header || !collapse.collapsed.has(g.key))
        .flatMap((g) => g.rows.map(rowKeyOf)),
    [baseGroups, collapse.collapsed],
  );
  const selection = useSelection({
    orderedKeys,
    isEligible: (k) => {
      const r = byKey.get(k);
      return r !== undefined && !inUse(r);
    },
  });

  const groups: TableGroup<VolumeRow>[] = baseGroups.map((g) =>
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
  const headed = groups.filter((g) => g.header).map((g) => g.key);
  const allCollapsed = headed.length > 0 && headed.every((k) => collapse.collapsed.has(k));

  const filtered = Boolean(search.q) || search.usage !== undefined || node !== undefined;
  const clearFilters = () => onSearchChange({ q: undefined, usage: undefined, node: undefined });

  // The reason Delete… is unavailable on a row: who may comes first, then what still
  // uses the volume.
  const needReason = remove.allowed
    ? undefined
    : (remove.reason ?? t("needs.extra.volumes.delete"));
  const deleteReason = (v: VolumeRow) =>
    needReason ?? (inUse(v) ? t("volumes.list.inUseBy", { what: inUseWhat(t, v) }) : undefined);

  const columns: Column<VolumeRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      className: "min-w-[200px]",
      cell: (v) => (
        <span className="flex items-center gap-2">
          <Link
            to="/env/$envId/volumes/$volumeName"
            params={{ envId, volumeName: v.name }}
            search={{ node: v.nodeId }}
            className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText mono>{v.name}</TruncatedText>
          </Link>
          {v.anonymous ? <Badge variant="secondary">{t("volumes.list.anonymous")}</Badge> : null}
        </span>
      ),
    },
    {
      id: "driver",
      header: t(`${key}.columns.driver`),
      cell: (v) => v.driver || missing,
    },
    {
      id: "usedBy",
      header: t("volumes.list.columns.usedBy"),
      cell: (v) =>
        inUse(v) ? (
          usedWhat(t, v, swarm)
        ) : (
          <span className="text-muted-foreground">{t("volumes.list.notInUse")}</span>
        ),
    },
    {
      // Sizes are worked out on demand; until they are, a dash.
      id: "size",
      header: t("volumes.list.columns.size"),
      cell: sizeCell,
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (v) => <RelativeTime value={v.created} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (v) => (
        <span className="flex items-center justify-end gap-1">
          <Button asChild variant="ghost" size="sm" className="max-md:h-11">
            <Link
              to="/env/$envId/volumes/$volumeName"
              params={{ envId, volumeName: v.name }}
              search={{ node: v.nodeId }}
              aria-label={t("volumes.list.openLabel", { name: v.name })}
            >
              {t("volumes.list.open")}
            </Link>
          </Button>
          <NeedsTooltip reason={deleteReason(v)}>
            <Button
              variant="ghost"
              size="sm"
              className="max-md:h-11"
              aria-label={t("volumes.delete.label", { name: v.name })}
              onClick={deleteReason(v) ? undefined : () => setDeleting({ rows: [v], open: true })}
            >
              {t("volumes.delete.action")}
            </Button>
          </NeedsTooltip>
        </span>
      ),
    },
  ];

  // The checkbox column. A volume in use keeps a checkbox that cannot be ticked and says
  // why.
  const select: TableSelect<VolumeRow> = {
    header: (
      <SelectCheckbox
        checked={checkedOf(selection.allState)}
        label={t("volumes.list.select.all")}
        disabled={selection.eligibleCount === 0}
        onToggle={() => selection.selectAll()}
      />
    ),
    cell: (v) => (
      <SelectCheckbox
        checked={selection.isSelected(rowKeyOf(v))}
        label={t("actions.select.row", { name: v.name })}
        reason={inUse(v) ? t("volumes.list.select.inUse") : undefined}
        onToggle={(shift) => selection.toggle(rowKeyOf(v), { shift })}
      />
    ),
    onEscape: selection.selected.size > 0 ? selection.clear : undefined,
  };

  const selectedRows = (items ?? []).filter((r) => selection.isSelected(rowKeyOf(r)));
  const actions: BulkAction[] = [
    {
      id: "delete",
      label: t("volumes.list.bulkDelete"),
      destructive: true,
      reason: needReason,
      onClick: () => setDeleting({ rows: selectedRows, open: true }),
    },
  ];

  const nodeCount = data?.nodes?.length ?? 0;
  const sizesButton = (
    <Button
      variant="outline"
      className="max-md:h-11"
      disabled={sizing.isPending}
      onClick={calculate}
    >
      {sizing.isPending ? <Spinner aria-hidden="true" /> : <HardDrive aria-hidden="true" />}
      {sizing.isPending
        ? swarm && nodeCount > 1
          ? t("volumes.sizes.calculatingNodes", { count: nodeCount })
          : t("volumes.sizes.calculating")
        : t("volumes.sizes.button")}
    </Button>
  );

  const createButton = (className?: string) => (
    <NeedsTooltip reason={create.reason}>
      <Button
        className={className ?? "max-md:h-11"}
        onClick={create.allowed ? () => setCreating(true) : undefined}
      >
        <Plus aria-hidden="true" />
        {t("volumes.create.button")}
      </Button>
    </NeedsTooltip>
  );

  // No volumes at all says what they are for, with the way to make one; a filter that
  // matches nothing says so, with the way out.
  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("volumes.list.empty_filtered.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={clearFilters}>
          {t("volumes.list.empty_filtered.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base">
          {t("volumes.list.empty.title")}
        </EmptyTitle>
        <EmptyDescription>{t("volumes.list.empty.body")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>{createButton()}</EmptyContent>
    </Empty>
  );

  const toolbar = (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox
          value={search.q}
          onCommit={(q) => onSearchChange({ q })}
          label={t("volumes.list.search.label")}
          placeholder={t("volumes.list.search.placeholder")}
          clearLabel={t("volumes.list.search.clear")}
        />
        <div className="flex items-center gap-2">
          <Label htmlFor={groupById} className="text-sm text-muted-foreground">
            {t("containers.list.groupBy.label")}
          </Label>
          <Select
            value={by}
            onValueChange={(v) =>
              onSearchChange({ group: v === "stack" ? undefined : (v as GroupBy) })
            }
          >
            <SelectTrigger id={groupById} size="sm" className="min-w-40 max-md:h-11">
              <SelectValue />
            </SelectTrigger>
            <SelectContent position="popper" align="start">
              {GROUP_VALUES.filter((g) => g !== "node" || swarm).map((g) => (
                <SelectItem key={g} value={g}>
                  {t(`containers.list.groupBy.${g}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {swarm ? (
          <NodeSelect
            value={node}
            onChange={(n) => onSearchChange({ node: n })}
            nodes={data?.nodes ?? []}
          />
        ) : null}
        {headed.length > 0 ? (
          <Button
            type="button"
            variant="outline"
            size="icon"
            className="max-md:size-11"
            aria-label={t(
              allCollapsed ? "containers.list.expandAll" : "containers.list.collapseAll",
            )}
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
            {t("volumes.list.clearFilters")}
          </Button>
        ) : null}
      </div>
      <ToggleGroup
        type="single"
        // The chips are toggle buttons in a labelled group, not a toolbar.
        role="group"
        variant="outline"
        size="sm"
        spacing={2}
        aria-label={t("volumes.list.usage.label")}
        value={search.usage ?? "all"}
        onValueChange={(v) =>
          onSearchChange({ usage: v === "used" || v === "unused" ? v : undefined })
        }
        className={STATUS_CHIP_ROW}
      >
        {(["all", ...USAGE_VALUES] as const).map((v) => (
          <ToggleGroupItem
            key={v}
            value={v}
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {t(`volumes.list.usage.${v}`)}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
    </div>
  );

  if (!allowed) return <NoPermission />;
  return (
    <>
      <BulkResultDialog envId={envId} report={report} onClose={() => setReport(null)} />
      <ResourceTable
        resource="volumes"
        columns={columns}
        query={query}
        envName={envName}
        nodeColumn={swarm ? nodeColumn : undefined}
        nodeFilter={
          swarm ? { value: node, onChange: (n) => onSearchChange({ node: n }) } : undefined
        }
        banner={swarm ? <NodesBanner notReporting={data?.notReporting} /> : undefined}
        groups={{ items: groups, collapse }}
        toolbar={toolbar}
        empty={empty}
        getRowKey={rowKeyOf}
        captionExtra={
          <div className="ml-auto flex flex-wrap items-center gap-2">
            {sizesButton}
            {createButton()}
          </div>
        }
        select={select}
        bulkBar={
          selection.selected.size > 0 ? (
            <BulkBar count={selection.selected.size} actions={actions} onClear={selection.clear} />
          ) : undefined
        }
      />
      {remove.allowed ? (
        <DeleteVolumesDialog
          envId={envId}
          rows={deleting.rows}
          swarm={swarm}
          open={deleting.open}
          onClose={() => setDeleting((d) => ({ ...d, open: false }))}
          onDone={selection.clear}
          onDetails={setReport}
        />
      ) : null}
      {create.allowed ? (
        <CreateVolumeDialog
          envId={envId}
          open={creating}
          onOpenChange={setCreating}
          swarm={swarm}
          networks={nodeSource.data?.items ?? []}
          volumes={(narrowed ? everything.data : data)?.items ?? []}
        />
      ) : null}
    </>
  );
}
