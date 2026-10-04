// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { type NetworkRow, networksQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { RelativeTime } from "@/components/relative-time";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { type Column, ResourceTable, TruncatedText } from "@/features/resources/resource-table";
import {
  nodeRowKey,
  nodeSearchSchema,
  useEnvIsSwarm,
  useNodeColumn,
} from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Networks() {
  const { envId } = Route.useParams();
  const t = useT();
  const key = "dashboard.lists.networks";
  useDocumentTitle(t(`${key}.title`));
  const allowed = usePermission("networks.read", envId);
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<NetworkRow>();
  const query = useQuery({ ...networksQuery(envId, swarm ? node : undefined), enabled: allowed });
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
    <ResourceTable
      resource="networks"
      columns={columns}
      query={query}
      envName={envName}
      nodeColumn={swarm ? nodeColumn : undefined}
      nodeFilter={
        swarm
          ? {
              value: node,
              onChange: (n) => void navigate({ search: { node: n }, replace: true }),
            }
          : undefined
      }
      banner={swarm ? <NodesBanner notReporting={query.data?.notReporting} /> : undefined}
      getRowKey={(n) => nodeRowKey(n.node, n.id)}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/networks")({
  validateSearch: nodeSearchSchema,
  component: Networks,
});
