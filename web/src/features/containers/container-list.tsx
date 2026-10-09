// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronsDownUp, ChevronsUpDown, Search, X } from "lucide-react";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";

import { type ContainerRow, containersQuery } from "@/api/resources";
import { NewItemsPill } from "@/components/new-items-pill";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { groupContainers, sortContainers } from "@/features/containers/group";
import {
  type ContainerSearch,
  type ContainerSearchPatch,
  GROUP_VALUES,
  type GroupBy,
  MAX_QUERY,
  type SortKey,
  STATUS_VALUES,
  type StatusValue,
  statusList,
  statusParam,
} from "@/features/containers/list-search";
import { ContainerRowActions } from "@/features/containers/row-actions";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { useFrozenRows } from "@/features/resources/frozen-rows";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  type RowState,
  type TableGroup,
  TruncatedText,
} from "@/features/resources/resource-table";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";

// How long the box waits after the last key before the search goes into the URL
// and to the server (UI-SPEC "Screens": 250 ms).
export const SEARCH_DEBOUNCE_MS = 250;

interface SearchBoxProps {
  // The search in the URL.
  value: string | undefined;
  onCommit: (q: string | undefined) => void;
}

// SearchBox is the "Search containers" input. What is typed shows at once; the
// URL, and so the server, hear of it 250 ms after the last key. Clearing is
// immediate. The text is the user's own until the URL changes by some other
// route (Clear filters, the back button), and then the box follows it.
function SearchBox({ value, onCommit }: SearchBoxProps) {
  const t = useT();
  const id = useId();
  const [text, setText] = useState(value ?? "");
  // The value this box last sent, so its own URL update is not taken for an
  // outside change that would overwrite what has been typed since.
  const sent = useRef(value);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  useEffect(() => {
    if (value === sent.current) return;
    sent.current = value;
    setText(value ?? "");
  }, [value]);

  useEffect(() => () => clearTimeout(timer.current), []);

  function change(next: string) {
    setText(next);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      const q = next.trim() || undefined;
      sent.current = q;
      onCommit(q);
    }, SEARCH_DEBOUNCE_MS);
  }

  function clear() {
    clearTimeout(timer.current);
    setText("");
    sent.current = undefined;
    onCommit(undefined);
  }

  return (
    <div className="w-full max-w-[360px] max-md:max-w-none">
      <Label htmlFor={id} className="sr-only">
        {t("containers.list.search.label")}
      </Label>
      <InputGroup className="max-md:h-11">
        <InputGroupAddon>
          <Search aria-hidden="true" />
        </InputGroupAddon>
        <InputGroupInput
          id={id}
          type="search"
          autoComplete="off"
          maxLength={MAX_QUERY}
          placeholder={t("containers.list.search.placeholder")}
          value={text}
          onChange={(e) => change(e.target.value)}
          className="[&::-webkit-search-cancel-button]:hidden"
        />
        {text ? (
          <InputGroupAddon align="inline-end">
            <InputGroupButton
              size="icon-xs"
              className="max-md:size-11"
              aria-label={t("containers.list.search.clear")}
              onClick={clear}
            >
              <X aria-hidden="true" />
            </InputGroupButton>
          </InputGroupAddon>
        ) : null}
      </InputGroup>
    </div>
  );
}

const rowKeyOf = (c: ContainerRow) => nodeRowKey(c.node, c.id);

// What a row shows that can change under the user: its state and status text,
// the health check, the image and the ports. A change in any of them marks the
// row (D-05).
const fingerprintOf = (c: ContainerRow) =>
  [c.state, c.status, c.health, c.image, (c.ports ?? []).join(",")].join("|");

// PortsCell shows the first two published ports and "+n" for the rest, whose
// tooltip lists them all. A container with none shows a dash.
function PortsCell({ ports, missing }: { ports: string[] | null; missing: string }) {
  const list = ports ?? [];
  if (list.length === 0) return missing;
  const rest = list.length - SHOWN_PORTS;
  return (
    <span className="flex items-center gap-1">
      <TruncatedText className="max-w-32">{list.slice(0, SHOWN_PORTS).join(", ")}</TruncatedText>
      {rest > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="shrink-0 rounded text-xs text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50">
              {/* Focusable, so the full list opens from the keyboard too. */}+{rest}
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-80 break-all">{list.join(", ")}</TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

// How many ports show before the rest fold into "+n".
const SHOWN_PORTS = 2;

interface ContainerListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to
  // undefined leaves the URL. Changes replace the history entry.
  search: ContainerSearch;
  onSearchChange: (patch: ContainerSearchPatch) => void;
}

// ContainerList is the containers page (D-01 to D-05): the rows grouped by stack
// or project, found by a search and the status chips that the server answers,
// sorted from the column headers. Docker's names are text only (T-04-33).
export function ContainerList({ envId, search, onSearchChange }: ContainerListProps) {
  const t = useT();
  const groupById = useId();
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<ContainerRow>();
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");
  const collapse = useGroupCollapse();

  const node = swarm ? search.node : undefined;
  const query = useQuery(containersQuery(envId, { node, q: search.q, status: search.status }));
  const data = query.data;

  // Grouping by node means nothing on a standalone host, which has one.
  const by: GroupBy = search.group === "node" && !swarm ? "stack" : (search.group ?? "stack");
  const sortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";
  const statuses = statusList(search.status);

  // The rows in the order the list shows them. The same array until the answer
  // or the sort changes, which is what lets the freeze tell an update from a
  // re-render.
  const items = data?.items;
  const sorted = useMemo(
    () => sortContainers(items ?? [], { key: sortKey, dir: sortDir }),
    [items, sortKey, sortDir],
  );

  // Rows with a menu or dialog open do not move (D-50): while any is open the
  // list is frozen. Changed rows still update in place, new rows wait behind
  // the pill, and gone rows stay dimmed until the freeze ends.
  const [openRows, setOpenRows] = useState<ReadonlySet<string>>(() => new Set());
  const setRowOpen = useCallback((key: string, open: boolean) => {
    setOpenRows((prev) => {
      if (prev.has(key) === open) return prev;
      const next = new Set(prev);
      if (open) next.add(key);
      else next.delete(key);
      return next;
    });
  }, []);
  const frozen = useFrozenRows(sorted, {
    frozen: openRows.size > 0,
    getKey: rowKeyOf,
    fingerprint: fingerprintOf,
    // Rows of the previous filters, kept on screen while the new ones load, are
    // not an earlier answer to this question.
    ready: data !== undefined && !query.isPlaceholderData,
    resetKey: [envId, node, search.q, search.status].join("\u0000"),
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

  const groups = useMemo<TableGroup<ContainerRow>[]>(
    () =>
      groupContainers(shownRows, by, null).map((g) => ({
        key: g.key,
        rows: g.rows,
        header:
          g.kind === "all"
            ? undefined
            : {
                name: g.kind === "ungrouped" ? t("containers.list.group.ungrouped") : g.name,
                summary: t("containers.list.group.running", {
                  running: g.running,
                  count: g.total,
                }),
                badge:
                  g.kind === "stack" || g.kind === "project" ? (
                    <Badge variant="outline">{t(`containers.list.group.kind.${g.kind}`)}</Badge>
                  ) : undefined,
              },
      })),
    [shownRows, by, t],
  );

  const pending = frozen.heldCount + frozen.removedCount;
  const headed = groups.filter((g) => g.header).map((g) => g.key);
  const allCollapsed = headed.length > 0 && headed.every((k) => collapse.collapsed.has(k));

  const filtered = Boolean(search.q || search.status);
  const anyFilter = filtered || node !== undefined;
  const clearFilters = () => onSearchChange({ q: undefined, status: undefined, node: undefined });

  function onSort(key: string) {
    const next = key as SortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  // Search alone gets its own empty state; any chip or node makes it a filter
  // match, since the filters may be the cause (UI-SPEC "Core elements"). A node
  // alone keeps the plain empty state, which names the node.
  const searchOnly = Boolean(search.q) && !search.status && node === undefined;
  const empty = !filtered ? undefined : (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {searchOnly
            ? t("containers.list.empty.search.title", { q: search.q })
            : t("containers.list.empty.filters.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button
          variant="outline"
          className="max-md:h-11"
          onClick={() => (searchOnly ? onSearchChange({ q: undefined }) : clearFilters())}
        >
          {searchOnly
            ? t("containers.list.empty.search.action")
            : t("containers.list.empty.filters.action")}
        </Button>
      </EmptyContent>
    </Empty>
  );

  const columns: Column<ContainerRow>[] = [
    {
      id: "name",
      header: t("dashboard.lists.containers.columns.name"),
      className: "min-w-[200px]",
      sortKey: "name",
      cell: (c) => {
        const kind = c.selfKind || (c.isSelf ? "server" : "");
        return (
          <span className="flex items-center gap-2">
            <TruncatedText>{c.name}</TruncatedText>
            {kind === "server" || kind === "agent" ? <SelfContainerBadge kind={kind} /> : null}
          </span>
        );
      },
    },
    {
      id: "state",
      header: t("dashboard.lists.containers.columns.state"),
      className: "w-40 min-w-40",
      sortKey: "state",
      cell: (c) =>
        rowStates.get(rowKeyOf(c))?.removed ? (
          <span className="text-xs text-muted-foreground">{t("containers.list.removed")}</span>
        ) : (
          <ContainerStateCell state={c.state} health={c.health} />
        ),
    },
    {
      id: "image",
      header: t("dashboard.lists.containers.columns.image"),
      cell: (c) => (c.image ? <TruncatedText>{c.image}</TruncatedText> : missing),
    },
    {
      id: "ports",
      header: t("dashboard.lists.containers.columns.ports"),
      className: "w-40 min-w-40",
      cell: (c) => <PortsCell ports={c.ports} missing={missing} />,
    },
    {
      id: "created",
      header: t("dashboard.lists.containers.columns.created"),
      className: "w-[120px] min-w-[120px]",
      sortKey: "created",
      cell: (c) => <RelativeTime value={c.created} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (c) => (
        <ContainerRowActions
          envId={envId}
          row={c}
          disabled={rowStates.get(rowKeyOf(c))?.removed}
          onOpenChange={(open) => setRowOpen(rowKeyOf(c), open)}
        />
      ),
    },
  ];

  const toolbar = (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox value={search.q} onCommit={(q) => onSearchChange({ q })} />
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
        {anyFilter ? (
          <Button variant="ghost" className="max-md:h-11" onClick={clearFilters}>
            {t("containers.list.clearFilters")}
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
        aria-label={t("containers.list.status.label")}
        value={statuses}
        onValueChange={(v) => onSearchChange({ status: statusParam(v as StatusValue[]) })}
        className="max-w-full max-md:w-full max-md:flex-nowrap max-md:overflow-x-auto md:flex-wrap"
      >
        {STATUS_VALUES.map((v) => {
          const label = t(`containers.list.status.${v}`);
          return (
            <ToggleGroupItem
              key={v}
              value={v}
              className="aria-pressed:border-ring aria-pressed:text-ring max-md:h-11"
            >
              {data
                ? t("containers.list.status.chip", { label, count: data.statusCounts?.[v] ?? 0 })
                : label}
            </ToggleGroupItem>
          );
        })}
      </ToggleGroup>
    </div>
  );

  return (
    <ResourceTable
      resource="containers"
      columns={columns}
      query={query}
      envName={envName}
      nodeColumn={swarm ? nodeColumn : undefined}
      nodeFilter={swarm ? { value: node, onChange: (n) => onSearchChange({ node: n }) } : undefined}
      banner={swarm ? <NodesBanner notReporting={data?.notReporting} /> : undefined}
      groups={{ items: groups, collapse }}
      toolbar={toolbar}
      caption={
        data && filtered
          ? t("containers.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
          : undefined
      }
      empty={empty}
      capNote={data ? t("containers.list.cap", { total: data.total }) : undefined}
      captionExtra={
        <NewItemsPill
          count={pending}
          label={t("containers.list.changes", { count: pending })}
          action={t("containers.list.showChanges")}
          onClick={frozen.release}
        />
      }
      sort={{ key: sortKey, dir: sortDir, onSort }}
      rowStates={rowStates}
      getRowKey={rowKeyOf}
    />
  );
}
