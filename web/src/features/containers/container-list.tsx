// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Search, X } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import type { HealthState } from "@/api/environments";
import { type ContainerRow, containersQuery } from "@/api/resources";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Label } from "@/components/ui/label";
import { groupContainers, sortContainers } from "@/features/containers/group";
import {
  type ContainerSearch,
  type ContainerSearchPatch,
  MAX_QUERY,
  type SortKey,
} from "@/features/containers/list-search";
import { ContainerRowActions } from "@/features/containers/row-actions";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { useGroupCollapse } from "@/features/resources/group-header-row";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  type TableGroup,
  TruncatedText,
} from "@/features/resources/resource-table";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";

// The container state badge (UI-SPEC D-56): running is online; restarting,
// paused and removing are warning; dead is offline; exited and created are
// neutral. The dot is always paired with the state's text.
const STATE_DOT: Record<ContainerRow["state"], HealthState> = {
  running: "online",
  restarting: "unsupported",
  paused: "unsupported",
  removing: "unsupported",
  dead: "offline",
  exited: "unknown",
  created: "unknown",
};

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

interface ContainerListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to
  // undefined leaves the URL. Changes replace the history entry.
  search: ContainerSearch;
  onSearchChange: (patch: ContainerSearchPatch) => void;
}

// ContainerList is the containers page (D-01 to D-05): the rows grouped by stack
// or project, found by a search the server answers, sorted from the column
// headers. The host's Docker names are text only (T-04-33).
export function ContainerList({ envId, search, onSearchChange }: ContainerListProps) {
  const t = useT();
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<ContainerRow>();
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");
  const collapse = useGroupCollapse();

  const node = swarm ? search.node : undefined;
  const query = useQuery(containersQuery(envId, { node, q: search.q, status: search.status }));

  // Grouping by node means nothing on a standalone host, which has one.
  const by = search.group === "node" && !swarm ? "stack" : (search.group ?? "stack");
  const sortKey = search.sort ?? "name";
  const sortDir = search.dir ?? "asc";

  const items = query.data?.items;
  const groups = useMemo<TableGroup<ContainerRow>[]>(() => {
    const sorted = sortContainers(items ?? [], { key: sortKey, dir: sortDir });
    return groupContainers(sorted, by, null).map((g) => ({
      key: g.key,
      rows: g.rows,
      header:
        g.kind === "all"
          ? undefined
          : {
              name: g.kind === "ungrouped" ? t("containers.list.group.ungrouped") : g.name,
              summary: t("containers.list.group.running", { running: g.running, count: g.total }),
              badge:
                g.kind === "stack" || g.kind === "project" ? (
                  <Badge variant="outline">{t(`containers.list.group.kind.${g.kind}`)}</Badge>
                ) : undefined,
            },
    }));
  }, [items, by, sortKey, sortDir, t]);

  function onSort(key: string) {
    const next = key as SortKey;
    if (next === sortKey) onSearchChange({ sort: next, dir: sortDir === "asc" ? "desc" : "asc" });
    else onSearchChange({ sort: next, dir: "asc" });
  }

  const columns: Column<ContainerRow>[] = [
    {
      id: "name",
      header: t("dashboard.lists.containers.columns.name"),
      sortKey: "name",
      cell: (c) => (
        <span className="flex items-center gap-2">
          <TruncatedText>{c.name}</TruncatedText>
          {c.isSelf ? <Badge variant="outline">{t("dashboard.selfBadge")}</Badge> : null}
        </span>
      ),
    },
    {
      id: "state",
      header: t("dashboard.lists.containers.columns.state"),
      sortKey: "state",
      cell: (c) => (
        <StatusDot
          state={STATE_DOT[c.state] ?? "unknown"}
          label={t(`dashboard.lists.states.${c.state}`)}
        />
      ),
    },
    {
      id: "image",
      header: t("dashboard.lists.containers.columns.image"),
      cell: (c) => (c.image ? <TruncatedText>{c.image}</TruncatedText> : missing),
    },
    {
      id: "created",
      header: t("dashboard.lists.containers.columns.created"),
      sortKey: "created",
      cell: (c) => <RelativeTime value={c.created} />,
    },
    {
      id: "status",
      header: t("dashboard.lists.containers.columns.status"),
      cell: (c) => (c.status ? <TruncatedText>{c.status}</TruncatedText> : missing),
    },
    {
      id: "ports",
      header: t("dashboard.lists.containers.columns.ports"),
      cell: (c) => {
        const ports = c.ports ?? [];
        return ports.length > 0 ? <TruncatedText>{ports.join(", ")}</TruncatedText> : missing;
      },
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (c) => <ContainerRowActions envId={envId} row={c} />,
    },
  ];

  return (
    <ResourceTable
      resource="containers"
      columns={columns}
      query={query}
      envName={envName}
      nodeColumn={swarm ? nodeColumn : undefined}
      nodeFilter={swarm ? { value: node, onChange: (n) => onSearchChange({ node: n }) } : undefined}
      banner={swarm ? <NodesBanner notReporting={query.data?.notReporting} /> : undefined}
      groups={{ items: groups, collapse }}
      toolbar={
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <SearchBox value={search.q} onCommit={(q) => onSearchChange({ q })} />
            {swarm ? (
              <NodeSelect
                value={node}
                onChange={(n) => onSearchChange({ node: n })}
                nodes={query.data?.nodes ?? []}
              />
            ) : null}
          </div>
        </div>
      }
      sort={{ key: sortKey, dir: sortDir, onSort }}
      getRowKey={(c) => nodeRowKey(c.node, c.id)}
    />
  );
}
