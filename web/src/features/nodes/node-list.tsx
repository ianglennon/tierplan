// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Crown, Lock, TriangleAlert } from "lucide-react";
import { useMemo } from "react";

import { type NodeItem, nodesQuery } from "@/api/nodes";
import { isNotASwarm } from "@/api/services";
import { NoPermission } from "@/components/full-page-state";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SearchBox } from "@/features/containers/container-list";
import { formatCpus, nodeStateKey, nodeTone, platformOf } from "@/features/nodes/node-facts";
import {
  NODE_STATUS_VALUES,
  type NodeSearch,
  type NodeSearchPatch,
  type NodeSortKey,
  type NodeStatusValue,
  nodeStatusList,
  nodeStatusParam,
  sortNodes,
} from "@/features/nodes/node-search";
import {
  type Column,
  type ListQuery,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useEnvName } from "@/features/resources/use-env-name";
import { NotASwarm } from "@/features/services/not-a-swarm";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

interface NodeListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to undefined leaves the
  // URL. Changes replace the history entry.
  search: NodeSearch;
  onSearchChange: (patch: NodeSearchPatch) => void;
}

const rowKeyOf = (n: NodeItem) => n.id;

type Translate = ReturnType<typeof useT>;

function AvailabilityCell({ node, tasksKnown }: { node: NodeItem; tasksKnown: boolean }) {
  const t = useT();
  if (node.availability === "pause") {
    return <StatusDot state="degraded" label={t("nodes.availability.pause")} />;
  }
  if (node.availability === "drain") {
    // Draining while it still has tasks that should run; Drained once it has none. With the tasks
    // unread, only the word.
    const label = !tasksKnown
      ? t("nodes.availability.drain")
      : node.tasksRunning > 0
        ? t("nodes.availability.drainTasks", { count: node.tasksRunning })
        : t("nodes.availability.drained");
    return <StatusDot state="degraded" label={label} />;
  }
  return <span>{t("nodes.availability.active")}</span>;
}

function RoleCell({ node }: { node: NodeItem }) {
  const t = useT();
  return (
    <span className="flex items-center gap-2">
      <span>{node.role === "manager" ? t("nodes.role.manager") : t("nodes.role.worker")}</span>
      {node.role === "manager" && node.reachability === "unreachable" ? (
        <StatusDot state="offline" label={t("nodes.role.unreachable")} />
      ) : null}
    </span>
  );
}

// TasksCell is the node's running tasks, with the ones that are not running in its tooltip. The
// number is a button with nothing to do, so the tooltip opens from the keyboard too.
function TasksCell({ node }: { node: NodeItem }) {
  const t = useT();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className="cursor-default rounded-sm tabular-nums outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
        >
          {node.tasksRunning}
        </button>
      </TooltipTrigger>
      <TooltipContent>
        {t("nodes.list.tasksTooltip", {
          running: node.tasksRunning,
          notRunning: node.tasksNotRunning,
        })}
      </TooltipContent>
    </Tooltip>
  );
}

function columnsOf(
  envId: string,
  t: Translate,
  tasksKnown: boolean,
  missing: string,
): Column<NodeItem>[] {
  return [
    {
      id: "name",
      header: t("nodes.list.columns.name"),
      className: "min-w-[224px]",
      sortKey: "name",
      cell: (n) => (
        <span className="flex items-center gap-2">
          <Link
            to="/env/$envId/nodes/$nodeId"
            params={{ envId, nodeId: n.id }}
            className="min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            <TruncatedText>{n.hostname}</TruncatedText>
          </Link>
          {n.leader ? (
            <Badge variant="outline" className="shrink-0">
              <Crown aria-hidden="true" />
              {t("nodes.badge.leader")}
            </Badge>
          ) : null}
          {n.runsBayplan ? (
            <Badge variant="outline" className="shrink-0">
              <Lock aria-hidden="true" />
              {t("nodes.badge.runsBayplan")}
            </Badge>
          ) : null}
        </span>
      ),
    },
    {
      id: "status",
      header: t("nodes.list.columns.status"),
      className: "w-36 min-w-36",
      sortKey: "status",
      cell: (n) => (
        <StatusDot state={nodeTone(n.state)} label={t(`nodes.state.${nodeStateKey(n.state)}`)} />
      ),
    },
    {
      id: "availability",
      header: t("nodes.list.columns.availability"),
      className: "w-48 min-w-48",
      cell: (n) => <AvailabilityCell node={n} tasksKnown={tasksKnown} />,
    },
    {
      id: "role",
      header: t("nodes.list.columns.role"),
      className: "w-44 min-w-44",
      sortKey: "role",
      cell: (n) => <RoleCell node={n} />,
    },
    {
      id: "address",
      header: t("nodes.list.columns.address"),
      className: "w-36 min-w-36",
      cell: (n) => (n.address ? <TruncatedText mono>{n.address}</TruncatedText> : missing),
    },
    {
      id: "engine",
      header: t("nodes.list.columns.engine"),
      className: "w-28 min-w-28",
      cell: (n) => n.engineVersion || missing,
    },
    {
      id: "platform",
      header: t("nodes.list.columns.platform"),
      className: "w-32 min-w-32",
      cell: (n) => platformOf(n.os, n.arch) || missing,
    },
    {
      id: "cpu",
      header: t("nodes.list.columns.cpu"),
      className: "w-20 min-w-20 text-end tabular-nums",
      cell: (n) => formatCpus(n.cpus) || missing,
    },
    {
      id: "memory",
      header: t("nodes.list.columns.memory"),
      className: "w-28 min-w-28 text-end tabular-nums",
      cell: (n) => (n.memoryBytes > 0 ? formatBytes(n.memoryBytes) : missing),
    },
    {
      id: "tasks",
      header: t("nodes.list.columns.tasks"),
      className: "w-20 min-w-20 text-end",
      sortKey: "tasks",
      cell: (n) => (tasksKnown ? <TasksCell node={n} /> : missing),
    },
    {
      id: "agent",
      header: t("nodes.list.columns.agent"),
      className: "w-36 min-w-36",
      cell: (n) =>
        n.agentReporting ? (
          <StatusDot state="online" label={t("nodes.agent.reporting")} />
        ) : (
          <StatusDot state="unknown" label={t("nodes.agent.notReporting")} />
        ),
    },
  ];
}

// NodeList is the Nodes page of a swarm environment (UI-SPEC "Nodes"): every node with its role,
// health, platform, resources, task count and whether a Bayplan agent reports from it, found by a
// search and the status chips the server answers, and sorted from the column headers. It reads
// the list the server serves from a manager; with no manager connected the last rows stay with a
// notice above them. A node with no reporting agent keeps its row. Docker's host names and
// addresses are text only (T-06-41).
export function NodeList({ envId, search, onSearchChange }: NodeListProps) {
  const t = useT();
  const allowed = usePermission("nodes.read", envId);
  const query = useQuery({
    ...nodesQuery(envId, { q: search.q, status: search.status }),
    enabled: allowed,
  });
  const data = query.data;
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");
  const tasksKnown = data?.tasksKnown ?? true;
  const statuses = nodeStatusList(search.status);
  const filtered = Boolean(search.q || search.status);
  const clearFilters = () => onSearchChange({ q: undefined, status: undefined });

  const sortKey: NodeSortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";
  const rows = useMemo(
    () => sortNodes(data?.items ?? [], search.sort, search.dir),
    [data?.items, search.sort, search.dir],
  );

  function onSort(key: string) {
    const next = key as NodeSortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  // The table reads a list answer with a `truncated` flag; the nodes list is never capped.
  const listQuery = useMemo<ListQuery<NodeItem>>(
    () => ({
      data: data ? { items: rows, total: data.total, truncated: false } : undefined,
      isPending: query.isPending,
      isError: query.isError,
      isFetching: query.isFetching,
      refetch: query.refetch,
    }),
    [data, rows, query.isPending, query.isError, query.isFetching, query.refetch],
  );

  const caption = data
    ? filtered
      ? t("nodes.list.caption.filtered", { n: data.total, count: data.unfilteredTotal })
      : t("nodes.list.caption.summary", {
          nodes: t("nodes.list.caption.nodes", { count: data.total }),
          managers: t("nodes.list.caption.managers", { count: data.counts.managers }),
        })
    : undefined;

  const toolbar = (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox
          value={search.q}
          onCommit={(q) => onSearchChange({ q })}
          label={t("nodes.list.search.label")}
          placeholder={t("nodes.list.search.placeholder")}
          clearLabel={t("nodes.list.search.clear")}
        />
        {filtered ? (
          <Button variant="ghost" className="max-md:h-11" onClick={clearFilters}>
            {t("nodes.list.clearFilters")}
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
        aria-label={t("nodes.list.status.label")}
        value={statuses}
        onValueChange={(v) => onSearchChange({ status: nodeStatusParam(v as NodeStatusValue[]) })}
        className="max-w-full max-md:w-full max-md:flex-nowrap max-md:overflow-x-auto md:flex-wrap"
      >
        {NODE_STATUS_VALUES.map((v) => {
          const label = t(`nodes.list.status.${v}`);
          return (
            <ToggleGroupItem
              key={v}
              value={v}
              className="aria-pressed:border-ring aria-pressed:text-ring max-md:h-11"
            >
              {data ? t("nodes.list.status.chip", { label, count: data.counts[v] ?? 0 }) : label}
            </ToggleGroupItem>
          );
        })}
      </ToggleGroup>
    </div>
  );

  // A search or a chip that matches nothing says so, with the way out. With neither set the
  // table's plain "nothing to show" speaks.
  const empty = filtered ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {t("nodes.list.emptyFilters.title")}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={clearFilters}>
          {t("nodes.list.emptyFilters.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : undefined;

  const banner = data?.noManager ? (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("nodes.list.noManager.title")}</AlertTitle>
      <AlertDescription className="text-foreground">
        {t("nodes.list.noManager.body")}
      </AlertDescription>
    </Alert>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  if (isNotASwarm(query.error)) return <NotASwarm envId={envId} envName={envName} />;
  return (
    <ResourceTable
      resource="nodes"
      i18nKey="nodes.list"
      columns={columnsOf(envId, t, tasksKnown, missing)}
      query={listQuery}
      envName={envName}
      banner={banner}
      toolbar={toolbar}
      caption={caption}
      empty={empty}
      retryLabel={t("nodes.list.errorRetry")}
      sort={{ key: sortKey, dir: sortDir, onSort }}
      getRowKey={rowKeyOf}
    />
  );
}
