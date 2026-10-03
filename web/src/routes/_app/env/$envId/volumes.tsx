// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { type VolumeRow, volumesQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { RelativeTime } from "@/components/relative-time";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { type Column, ResourceTable, TruncatedText } from "@/features/resources/resource-table";
import { nodeSearchSchema, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Volumes() {
  const { envId } = Route.useParams();
  const t = useT();
  const key = "dashboard.lists.volumes";
  useDocumentTitle(t(`${key}.title`));
  const allowed = usePermission("volumes.read", envId);
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<VolumeRow>();
  const query = useQuery({ ...volumesQuery(envId, swarm ? node : undefined), enabled: allowed });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  const columns: Column<VolumeRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      cell: (v) => <TruncatedText>{v.name}</TruncatedText>,
    },
    {
      id: "driver",
      header: t(`${key}.columns.driver`),
      cell: (v) => v.driver || missing,
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (v) => <RelativeTime value={v.created} />,
    },
  ];

  if (!allowed) return <NoPermission />;
  return (
    <ResourceTable
      resource="volumes"
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
      getRowKey={(v) => v.name}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/volumes")({
  validateSearch: nodeSearchSchema,
  component: Volumes,
});
