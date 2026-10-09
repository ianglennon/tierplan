// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useState } from "react";

import { type NetworkRow, networksQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { CreateNetworkDialog } from "@/features/networks/create-network-dialog";
import { type Column, ResourceTable, TruncatedText } from "@/features/resources/resource-table";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface NetworkListProps {
  envId: string;
  // The node the list is narrowed to, from the URL.
  node?: string;
  onNodeChange: (node: string | undefined) => void;
}

// NetworkList is the networks list of an environment: the read-only columns of
// Phase 1 and, for an env Admin, the Create network button (D-39, D-51). Someone
// who may read but not manage sees the button disabled with the reason.
export function NetworkList({ envId, node, onNodeChange }: NetworkListProps) {
  const t = useT();
  const key = "dashboard.lists.networks";
  const allowed = usePermission("networks.read", envId);
  const manage = useNeeds("networks.manage", envId);
  const swarm = useEnvIsSwarm(envId);
  const [creating, setCreating] = useState(false);
  const nodeColumn = useNodeColumn<NetworkRow>();
  const query = useQuery({ ...networksQuery(envId, swarm ? node : undefined), enabled: allowed });
  // The create dialog checks the subnet against every node's networks and offers
  // every node, so while the list is narrowed to one node it reads the whole list.
  const narrowed = swarm && node !== undefined;
  const everything = useQuery({
    ...networksQuery(envId),
    enabled: allowed && narrowed && creating,
  });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  const columns: Column<NetworkRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      cell: (n) => <TruncatedText>{n.name}</TruncatedText>,
    },
    {
      id: "driver",
      header: t(`${key}.columns.driver`),
      cell: (n) => n.driver || missing,
    },
    {
      id: "scope",
      header: t(`${key}.columns.scope`),
      cell: (n) => n.scope || missing,
    },
    {
      id: "subnet",
      header: t(`${key}.columns.subnet`),
      cell: (n) => {
        const subnets = n.subnets ?? [];
        return subnets.length > 0 ? (
          <TruncatedText mono>{subnets.join(", ")}</TruncatedText>
        ) : (
          missing
        );
      },
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (n) => <RelativeTime value={n.created} />,
    },
  ];

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
