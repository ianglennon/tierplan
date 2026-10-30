// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { useState } from "react";

import { type NetworkRow, networksQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyHeader, EmptyTitle } from "@/components/ui/empty";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SearchBox } from "@/features/containers/container-list";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { CreateNetworkDialog } from "@/features/networks/create-network-dialog";
import { DeleteNetworkDialog } from "@/features/networks/delete-network-dialog";
import { deleteReason } from "@/features/networks/delete-reason";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  TruncatedText,
} from "@/features/resources/resource-table";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface NetworkListProps {
  envId: string;
  // The node the list is narrowed to, from the URL.
  node?: string;
  // The search text, from the URL.
  q?: string;
  onNodeChange: (node: string | undefined) => void;
  onSearchChange: (q: string | undefined) => void;
}

// NetworkList is the networks list of an environment: what is attached to each
// network, who owns it, and the actions on it. For an env Admin it has the Create
// network button and Delete…; someone who may read but not manage sees both
// disabled with the reason, and so does Delete… on a network that is built in or
// in use (D-39, D-40, D-41, D-51).
export function NetworkList({ envId, node, q, onNodeChange, onSearchChange }: NetworkListProps) {
  const t = useT();
  const key = "dashboard.lists.networks";
  const allowed = usePermission("networks.read", envId);
  const manage = useNeeds("networks.manage", envId);
  const swarm = useEnvIsSwarm(envId);
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<NetworkRow | null>(null);
  const nodeColumn = useNodeColumn<NetworkRow>();
  const nodeFilter = swarm ? node : undefined;
  const query = useQuery({ ...networksQuery(envId, { node: nodeFilter, q }), enabled: allowed });
  // The create dialog checks the subnet against every node's networks and offers
  // every node, so while the list is narrowed by a node or a search it reads the
  // whole list.
  const narrowed = nodeFilter !== undefined || Boolean(q);
  const everything = useQuery({
    ...networksQuery(envId),
    enabled: allowed && narrowed && creating,
  });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  // Why a network cannot be deleted: who may comes first, then what the network is,
  // then what still uses it.
  const deleteReasonOf = (n: NetworkRow) => deleteReason(t, manage.reason, n);

  const columns: Column<NetworkRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      cell: (n) => (
        <Link
          to="/env/$envId/networks/$networkId"
          params={{ envId, networkId: n.id }}
          search={{ node: n.nodeId }}
          className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
        >
          <TruncatedText>{n.name}</TruncatedText>
        </Link>
      ),
    },
    {
      id: "driver",
      header: t(`${key}.columns.driver`),
      cell: (n) => n.driver || missing,
    },
    {
      id: "scope",
      header: t(`${key}.columns.scope`),
      cell: (n) =>
        n.scope === "swarm"
          ? t("networks.list.scope.swarm")
          : n.scope === "local"
            ? t("networks.list.scope.local")
            : n.scope || missing,
    },
    {
      id: "subnet",
      header: t(`${key}.columns.subnet`),
      cell: (n) => {
        const subnets = n.subnets ?? [];
        const [first, ...rest] = subnets;
        if (first === undefined) return missing;
        return (
          <span className="flex items-center gap-1">
            <TruncatedText mono>{first}</TruncatedText>
            {rest.length > 0 ? (
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="shrink-0 rounded text-xs text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50">
                    {/* Focusable, so the rest open from the keyboard too. */}+{rest.length}
                  </span>
                </TooltipTrigger>
                <TooltipContent className="max-w-80 break-all font-mono">
                  {rest.join(", ")}
                </TooltipContent>
              </Tooltip>
            ) : null}
          </span>
        );
      },
    },
    {
      id: "attached",
      header: t(`${key}.columns.attached`),
      cell: (n) => {
        if (n.attachedContainers === 0 && n.attachedServices === 0) {
          return <span className="text-muted-foreground">{t("networks.list.none")}</span>;
        }
        const containers = t("networks.list.attached.containers", { count: n.attachedContainers });
        // Services exist only on a swarm.
        return swarm
          ? t("networks.list.attached.both", {
              containers,
              services: t("networks.list.attached.services", { count: n.attachedServices }),
            })
          : containers;
      },
    },
    {
      id: "owner",
      header: t(`${key}.columns.owner`),
      cell: (n) => (n.owner ? <TruncatedText>{n.owner}</TruncatedText> : missing),
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (n) => <RelativeTime value={n.created} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (n) => {
        const reason = deleteReasonOf(n);
        return (
          <span className="flex items-center justify-end gap-1">
            <Button asChild variant="ghost" size="sm" className="max-md:h-11">
              <Link
                to="/env/$envId/networks/$networkId"
                params={{ envId, networkId: n.id }}
                search={{ node: n.nodeId }}
                aria-label={t("networks.detail.openLabel", { name: n.name })}
              >
                {t("networks.detail.open")}
              </Link>
            </Button>
            <NeedsTooltip reason={reason}>
              <Button
                variant="ghost"
                size="sm"
                className="max-md:h-11"
                aria-label={t("networks.delete.label", { name: n.name })}
                onClick={reason ? undefined : () => setDeleting(n)}
              >
                {t("networks.delete.action")}
              </Button>
            </NeedsTooltip>
          </span>
        );
      },
    },
  ];

  const toolbar = (
    <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
      <SearchBox
        value={q}
        onCommit={onSearchChange}
        label={t("networks.list.search.label")}
        placeholder={t("networks.list.search.placeholder")}
        clearLabel={t("networks.list.search.clear")}
      />
      {swarm ? (
        <NodeSelect value={node} onChange={onNodeChange} nodes={query.data?.nodes ?? []} />
      ) : null}
    </div>
  );

  // A search that matches nothing says so, with the way out.
  const empty = q ? (
    <Empty className="min-h-[30svh] border">
      <EmptyHeader>
        <EmptyTitle role="heading" aria-level={2} className="text-base break-words">
          {t("networks.list.empty.search.title", { q })}
        </EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button variant="outline" className="max-md:h-11" onClick={() => onSearchChange(undefined)}>
          {t("networks.list.empty.search.action")}
        </Button>
      </EmptyContent>
    </Empty>
  ) : undefined;

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="networks"
        columns={columns}
        query={query}
        envName={envName}
        nodeColumn={swarm ? nodeColumn : undefined}
        nodeFilter={swarm ? { value: node, onChange: onNodeChange } : undefined}
        banner={swarm ? <NodesBanner notReporting={query.data?.notReporting} /> : undefined}
        toolbar={toolbar}
        empty={empty}
        getRowKey={(n) => nodeRowKey(n.node, n.id)}
        captionExtra={
          <div className="ml-auto">
            <NeedsTooltip reason={manage.reason}>
              <Button
                className="max-md:h-11"
                onClick={manage.allowed ? () => setCreating(true) : undefined}
              >
                <Plus aria-hidden="true" />
                {t("networks.create.button")}
              </Button>
            </NeedsTooltip>
          </div>
        }
      />
      {manage.allowed ? (
        <DeleteNetworkDialog envId={envId} network={deleting} onClose={() => setDeleting(null)} />
      ) : null}
      {manage.allowed ? (
        <CreateNetworkDialog
          envId={envId}
          open={creating}
          onOpenChange={setCreating}
          swarm={swarm}
          networks={(narrowed ? everything.data : query.data)?.items ?? []}
        />
      ) : null}
    </>
  );
}
