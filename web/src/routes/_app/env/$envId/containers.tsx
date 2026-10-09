// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import type { HealthState } from "@/api/environments";
import { type ContainerRow, containersQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { ContainerRowActions } from "@/features/containers/row-actions";
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

function Containers() {
  const { envId } = Route.useParams();
  const t = useT();
  const key = "dashboard.lists.containers";
  useDocumentTitle(t(`${key}.title`));
  const allowed = usePermission("containers.read", envId);
  const { node } = Route.useSearch();
  const navigate = Route.useNavigate();
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<ContainerRow>();
  const query = useQuery({ ...containersQuery(envId, swarm ? node : undefined), enabled: allowed });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  // The server's order is kept; the table never sorts (D-56).
  const columns: Column<ContainerRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      cell: (c) => (
        <span className="flex items-center gap-2">
          <TruncatedText>{c.name}</TruncatedText>
          {c.isSelf ? <Badge variant="outline">{t("dashboard.selfBadge")}</Badge> : null}
        </span>
      ),
    },
    {
      id: "state",
      header: t(`${key}.columns.state`),
      cell: (c) => (
        <StatusDot
          state={STATE_DOT[c.state] ?? "unknown"}
          label={t(`dashboard.lists.states.${c.state}`)}
        />
      ),
    },
    {
      id: "image",
      header: t(`${key}.columns.image`),
      cell: (c) => (c.image ? <TruncatedText>{c.image}</TruncatedText> : missing),
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      cell: (c) => <RelativeTime value={c.created} />,
    },
    {
      id: "status",
      header: t(`${key}.columns.status`),
      cell: (c) => (c.status ? <TruncatedText>{c.status}</TruncatedText> : missing),
    },
    {
      id: "ports",
      header: t(`${key}.columns.ports`),
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

  if (!allowed) return <NoPermission />;
  return (
    <ResourceTable
      resource="containers"
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
      getRowKey={(c) => nodeRowKey(c.node, c.id)}
    />
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers")({
  validateSearch: nodeSearchSchema,
  component: Containers,
});
