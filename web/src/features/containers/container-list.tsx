// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
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
import { Toggle } from "@/components/ui/toggle";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  ActionDialog,
  type DialogVerb,
  isLostAnswer,
  reportOf,
  requestReasonOf,
  toastBulk,
  useBulkRunner,
} from "@/features/containers/action-dialog";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { ContainerStateCell } from "@/features/containers/container-state-cell";
import { eligibleFor, ownKind, type Verb } from "@/features/containers/eligibility";
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
import {
  PendingContext,
  pendingFingerprint,
  pendingKey,
  usePendingRows,
} from "@/features/containers/pending";
import { ContainerRowActions } from "@/features/containers/row-actions";
import { SelfContainerBadge } from "@/features/containers/self-container-badge";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { type BulkAction, BulkBar } from "@/features/resources/bulk-bar";
import { useFrozenRows } from "@/features/resources/frozen-rows";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  type RowState,
  SelectCheckbox,
  type TableGroup,
  type TableSelect,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useChosenKeys, useSelection } from "@/features/resources/selection";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { RestartLoopBadge } from "@/features/restart-loop/restart-loop-badge";
import { StackLink, tierplanStackIdOf } from "@/features/stacks/stack-link";
import { MAX_LIST_STATS, useListStats } from "@/features/stats/list-stats";
import { CpuCell, MemoryCell, NetworkCell } from "@/features/stats/stats-cells";
import { StatsToggle } from "@/features/stats/stats-toggle";
import { UpdateBadge } from "@/features/updates/update-badge";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo } from "@/lib/toast";

// How long the box waits after the last key before the search goes into the URL
// and to the server (UI-SPEC "Screens": 250 ms).
export const SEARCH_DEBOUNCE_MS = 250;

interface SearchBoxProps {
  // The search in the URL.
  value: string | undefined;
  onCommit: (q: string | undefined) => void;
  // The words of the box. They are the containers list's unless the owner gives its own.
  label?: string;
  placeholder?: string;
  clearLabel?: string;
}

// SearchBox is the "Search containers" input, which the networks list also uses with
// its own words. What is typed shows at once; the
// URL, and so the server, hear of it 250 ms after the last key. Clearing is
// immediate. The text is the user's own until the URL changes by some other
// route (Clear filters, the back button), and then the box follows it.
export function SearchBox({ value, onCommit, label, placeholder, clearLabel }: SearchBoxProps) {
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
        {label ?? t("containers.list.search.label")}
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
          placeholder={placeholder ?? t("containers.list.search.placeholder")}
          value={text}
          onChange={(e) => change(e.target.value)}
          className="[&::-webkit-search-cancel-button]:hidden"
        />
        {text ? (
          <InputGroupAddon align="inline-end">
            <InputGroupButton
              size="icon-xs"
              className="max-md:size-11"
              aria-label={clearLabel ?? t("containers.list.search.clear")}
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

// How the stats stream names a container: the node it runs on, then its ID.
const statId = (c: ContainerRow) => `${c.nodeId}|${c.id}`;

// What a row shows that can change under the user: its state and status text,
// the health check, the image and the ports. A change in any of them marks the
// row (D-05).
const fingerprintOf = (c: ContainerRow) =>
  [c.state, c.status, c.health, c.image, (c.ports ?? []).join(","), c.update?.state].join("|");

// PortsCell shows the first two published ports and "+n" for the rest, whose
// tooltip lists them all. A container with none shows a dash. The services list uses it too.
export function PortsCell({ ports, missing }: { ports: string[] | null; missing: string }) {
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

// checkedOf is a checkbox's state from a selection's: tri-state for a group.
function checkedOf(state: "checked" | "mixed" | "unchecked"): boolean | "indeterminate" {
  return state === "checked" ? true : state === "mixed" ? "indeterminate" : false;
}

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
  const runner = useBulkRunner(envId);
  const query = useQuery(
    containersQuery(envId, {
      node,
      q: search.q,
      status: search.status,
      updates: search.updates,
    }),
  );
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
  // Rows chosen for a bulk action also hold the list still (D-05): a row that
  // moved between choosing it and pressing the button would be the wrong one.
  const chosen = useChosenKeys<string>();
  const frozen = useFrozenRows(sorted, {
    frozen: openRows.size > 0 || chosen[0].size > 0,
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

  const {
    pending,
    api: pendingApi,
    announcement,
  } = usePendingRows(shownRows, {
    getKey: pendingKey,
    fingerprint: pendingFingerprint,
  });

  const baseGroups = useMemo<TableGroup<ContainerRow>[]>(
    () =>
      groupContainers(shownRows, by, null).map((g) => ({
        key: g.key,
        rows: g.rows,
        header:
          g.kind === "all"
            ? undefined
            : {
                name: g.kind === "ungrouped" ? t("containers.list.group.ungrouped") : g.name,
                nameLink:
                  g.kind === "stack" || g.kind === "project" ? (
                    <StackLink
                      envId={envId}
                      name={g.name}
                      tierplanStackId={tierplanStackIdOf(g.rows)}
                    />
                  ) : undefined,
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
    [shownRows, by, t, envId],
  );

  // Which rows can be chosen: not TierPlan's own, not gone, not waiting on an
  // action (D-05, D-12). The order a Shift range follows is the order on screen,
  // folded groups left out.
  const byKey = useMemo(() => new Map(shownRows.map((r) => [rowKeyOf(r), r])), [shownRows]);
  const orderedKeys = useMemo(
    () =>
      baseGroups
        .filter((g) => !g.header || !collapse.collapsed.has(g.key))
        .flatMap((g) => g.rows.map(rowKeyOf)),
    [baseGroups, collapse.collapsed],
  );
  const selection = useSelection(
    {
      orderedKeys,
      isEligible: (key) => {
        const r = byKey.get(key);
        return (
          r !== undefined && ownKind(r) === "" && !rowStates.get(key)?.removed && !pending.has(key)
        );
      },
    },
    chosen,
  );

  // Live stats (D-31): one stream for the running rows on screen and in a group that is open,
  // no more than the server takes. A container is named by its node and its ID.
  const statsOn = search.stats === 1;
  const statsIds = useMemo(() => {
    if (!statsOn) return [];
    const out: string[] = [];
    for (const key of orderedKeys) {
      const r = byKey.get(key);
      if (r && r.state === "running" && r.nodeId && !rowStates.get(key)?.removed) {
        out.push(statId(r));
        if (out.length >= MAX_LIST_STATS) break;
      }
    }
    return out;
  }, [statsOn, orderedKeys, byKey, rowStates]);
  const liveStats = useListStats(envId, "containers", statsIds);

  const groups: TableGroup<ContainerRow>[] = baseGroups.map((g) =>
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
  const anyFilter = filtered || node !== undefined;
  const clearFilters = () =>
    onSearchChange({ q: undefined, status: undefined, updates: undefined, node: undefined });

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

  const statsColumns: Column<ContainerRow>[] = statsOn
    ? [
        {
          id: "cpu",
          header: t("stats.tiles.cpu"),
          className: "w-24 min-w-24 text-end",
          cell: (c) => <CpuCell stat={liveStats.get(statId(c))} />,
        },
        {
          id: "memory",
          header: t("stats.tiles.memory"),
          className: "w-36 min-w-36 text-end",
          cell: (c) => <MemoryCell stat={liveStats.get(statId(c))} />,
        },
        {
          id: "network",
          header: t("stats.tiles.network"),
          className: "w-48 min-w-48 text-end",
          cell: (c) => <NetworkCell stat={liveStats.get(statId(c))} />,
        },
      ]
    : [];

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
            <Link
              to="/env/$envId/containers/$containerId"
              params={{ envId, containerId: c.id }}
              search={{ node: c.nodeId }}
              className="min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <TruncatedText>{c.name}</TruncatedText>
            </Link>
            {kind === "server" || kind === "agent" ? <SelfContainerBadge kind={kind} /> : null}
            {c.restartLoop ? <RestartLoopBadge loop={c.restartLoop} kind="container" /> : null}
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
          <ContainerStateCell
            state={c.state}
            health={c.health}
            pending={pending.get(rowKeyOf(c))?.verb}
          />
        ),
    },
    {
      id: "updates",
      header: t("updates.list.column"),
      className: "w-44 min-w-44",
      // The server's word for the row; a row it sent no update object for shows a dash, never "Up to date".
      cell: (c) => (c.update ? <UpdateBadge info={c.update} /> : <span>{missing}</span>),
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
    ...statsColumns,
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (c) => (
        <ContainerRowActions
          envId={envId}
          row={c}
          disabled={rowStates.get(rowKeyOf(c))?.removed || pending.has(rowKeyOf(c))}
          onOpenChange={(open) => setRowOpen(rowKeyOf(c), open)}
          onDetails={setReport}
        />
      ),
    },
  ];

  // The checkbox column. A container of TierPlan's own keeps a checkbox that
  // cannot be ticked and says why; a gone row or one waiting on an action has
  // none to tick (D-05, D-12).
  const select: TableSelect<ContainerRow> = {
    header: (
      <SelectCheckbox
        checked={checkedOf(selection.allState)}
        label={t("actions.select.all")}
        disabled={selection.eligibleCount === 0}
        onToggle={() => selection.selectAll()}
      />
    ),
    cell: (c) => {
      const key = rowKeyOf(c);
      const own = ownKind(c) !== "";
      return (
        <SelectCheckbox
          checked={selection.isSelected(key)}
          label={t("actions.select.row", { name: c.name })}
          disabled={!own && (Boolean(rowStates.get(key)?.removed) || pending.has(key))}
          reason={own ? t("actions.select.own") : undefined}
          onToggle={(shift) => selection.toggle(key, { shift })}
        />
      );
    },
    onEscape: selection.selected.size > 0 ? selection.clear : undefined,
  };

  const selectedRows = shownRows.filter((r) => selection.isSelected(rowKeyOf(r)));
  const canControl = useNeeds("containers.control", envId);
  const canRemove = useNeeds("containers.remove", envId);

  // The reason a bulk button cannot be used: the caller's role, or no chosen row
  // it applies to.
  function reasonFor(verb: Verb): string | undefined {
    const need = verb === "remove" ? canRemove : canControl;
    if (!need.allowed) return need.reason ?? t("needs.role.operator");
    if (!selectedRows.some((r) => eligibleFor(verb, r, { bulk: true })))
      return t("actions.bulk.none", { verb: t(`actions.bulk.can.${verb}`) });
    return undefined;
  }

  // The results a person asked to see from a toast's Details.
  const [report, setReport] = useState<BulkReport | null>(null);

  // Start, Restart, Pause and Unpause run at once on the rows they apply to, and
  // sum up in one toast (D-06, D-07).
  async function runNow(verb: "start" | "restart" | "pause" | "unpause") {
    const targets = selectedRows.filter((r) => eligibleFor(verb, r, { bulk: true }));
    try {
      const results = await runner.run(verb, targets);
      toastBulk(t, reportOf(t, verb, selectedRows, targets, results), setReport);
      selection.clear();
    } catch (err) {
      if (isLostAnswer(err)) {
        toastInfo(
          t("actions.toast.lostMany", { verb: t(`actions.verb.${verb}`), count: targets.length }),
        );
        return;
      }
      toastError(requestReasonOf(t, err));
    }
  }

  // Stop, Kill and Remove ask first, with every chosen container listed. The
  // ones the action does not apply to are listed too, as skipped.
  const [dialog, setDialog] = useState<{ verb: DialogVerb; rows: ContainerRow[] }>({
    verb: "stop",
    rows: [],
  });
  const [dialogOpen, setDialogOpen] = useState(false);
  function openDialog(verb: DialogVerb) {
    setDialog({ verb, rows: selectedRows });
    setDialogOpen(true);
  }

  const actions: BulkAction[] = [
    {
      id: "start",
      label: t("actions.bulk.start"),
      reason: reasonFor("start"),
      onClick: () => void runNow("start"),
    },
    {
      id: "stop",
      label: t("actions.bulk.stop"),
      reason: reasonFor("stop"),
      onClick: () => openDialog("stop"),
    },
    {
      id: "restart",
      label: t("actions.bulk.restart"),
      reason: reasonFor("restart"),
      onClick: () => void runNow("restart"),
    },
    {
      id: "pause",
      label: t("actions.bulk.pause"),
      reason: reasonFor("pause"),
      onClick: () => void runNow("pause"),
    },
    {
      id: "unpause",
      label: t("actions.bulk.unpause"),
      reason: reasonFor("unpause"),
      onClick: () => void runNow("unpause"),
    },
    {
      id: "kill",
      label: t("actions.bulk.kill"),
      reason: reasonFor("kill"),
      destructive: true,
      onClick: () => openDialog("kill"),
    },
    {
      id: "remove",
      label: t("actions.bulk.remove"),
      reason: reasonFor("remove"),
      destructive: true,
      onClick: () => openDialog("remove"),
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
      <div className="flex flex-wrap items-center gap-2">
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
          className={STATUS_CHIP_ROW}
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

  return (
    <PendingContext.Provider value={pendingApi}>
      <div className="sr-only" role="status">
        {announcement
          ? t(`actions.announce.${announcement.verb}`, { names: announcement.names.join(", ") })
          : null}
      </div>
      <ActionDialog
        envId={envId}
        verb={dialog.verb}
        rows={dialog.rows}
        swarm={swarm}
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onDone={selection.clear}
        onDetails={setReport}
      />
      <BulkResultDialog envId={envId} report={report} onClose={() => setReport(null)} />
      <ResourceTable
        resource="containers"
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
        caption={
          data && filtered
            ? t("containers.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
            : undefined
        }
        empty={empty}
        capNote={data ? t("containers.list.cap", { total: data.total }) : undefined}
        captionExtra={
          <NewItemsPill
            count={changesWaiting}
            label={t("containers.list.changes", { count: changesWaiting })}
            action={t("containers.list.showChanges")}
            onClick={frozen.release}
          />
        }
        sort={{ key: sortKey, dir: sortDir, onSort }}
        rowStates={rowStates}
        getRowKey={rowKeyOf}
        select={select}
        bulkBar={
          selection.selected.size > 0 ? (
            <BulkBar count={selection.selected.size} actions={actions} onClear={selection.clear} />
          ) : undefined
        }
      />
    </PendingContext.Provider>
  );
}
