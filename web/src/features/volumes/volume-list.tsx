// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useState } from "react";

import { networksQuery, type VolumeRow, volumesQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { NodesBanner } from "@/features/environments/nodes-banner";
import { type Column, ResourceTable, TruncatedText } from "@/features/resources/resource-table";
import { nodeRowKey, useEnvIsSwarm, useNodeColumn } from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { CreateVolumeDialog } from "@/features/volumes/create-volume-dialog";
import { DeleteVolumesDialog } from "@/features/volumes/delete-volumes-dialog";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface VolumeListProps {
  envId: string;
  // The node the list is narrowed to, from the URL.
  node?: string;
  onNodeChange: (node: string | undefined) => void;
}

// VolumeList is the volumes list of an environment. For an env Admin it has the Create
// volume button; someone who may read but not create sees it disabled with the reason
// (D-44, D-51).
export function VolumeList({ envId, node, onNodeChange }: VolumeListProps) {
  const t = useT();
  const key = "dashboard.lists.volumes";
  const allowed = usePermission("volumes.read", envId);
  const create = useNeeds("volumes.create", envId);
  const remove = useNeeds("volumes.delete", envId);
  const swarm = useEnvIsSwarm(envId);
  const [creating, setCreating] = useState(false);
  // The rows a delete is about stay while the dialog closes, so its title never reads
  // "Delete ?" on the way out.
  const [deleting, setDeleting] = useState<{ rows: VolumeRow[]; open: boolean }>({
    rows: [],
    open: false,
  });
  const nodeColumn = useNodeColumn<VolumeRow>();
  const nodeFilter = swarm ? node : undefined;
  const query = useQuery({ ...volumesQuery(envId, nodeFilter), enabled: allowed });
  // The create dialog offers every node, so while the list is narrowed to one it reads
  // the whole list. The local networks name the nodes that hold no volume yet.
  const everything = useQuery({
    ...volumesQuery(envId),
    enabled: allowed && nodeFilter !== undefined && creating,
  });
  const nodeSource = useQuery({
    ...networksQuery(envId),
    enabled: allowed && swarm && creating,
  });
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");

  const columns: Column<VolumeRow>[] = [
    {
      id: "name",
      header: t(`${key}.columns.name`),
      cell: (v) => <TruncatedText mono>{v.name}</TruncatedText>,
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
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-px px-1 text-end",
      cell: (v) => (
        <NeedsTooltip
          reason={remove.allowed ? undefined : (remove.reason ?? t("needs.extra.volumes.delete"))}
        >
          <Button
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            aria-label={t("volumes.delete.label", { name: v.name })}
            onClick={remove.allowed ? () => setDeleting({ rows: [v], open: true }) : undefined}
          >
            {t("volumes.delete.action")}
          </Button>
        </NeedsTooltip>
      ),
    },
  ];

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

  // No volumes at all says what they are for, with the way to make one.
  const empty = (
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

  if (!allowed) return <NoPermission />;
  return (
    <>
      <ResourceTable
        resource="volumes"
        columns={columns}
        query={query}
        envName={envName}
        nodeColumn={swarm ? nodeColumn : undefined}
        nodeFilter={swarm ? { value: node, onChange: onNodeChange } : undefined}
        banner={swarm ? <NodesBanner notReporting={query.data?.notReporting} /> : undefined}
        empty={empty}
        getRowKey={(v) => nodeRowKey(v.node, v.name)}
        captionExtra={<div className="ml-auto">{createButton()}</div>}
      />
      {remove.allowed ? (
        <DeleteVolumesDialog
          envId={envId}
          rows={deleting.rows}
          open={deleting.open}
          onClose={() => setDeleting((d) => ({ ...d, open: false }))}
        />
      ) : null}
      {create.allowed ? (
        <CreateVolumeDialog
          envId={envId}
          open={creating}
          onOpenChange={setCreating}
          swarm={swarm}
          networks={nodeSource.data?.items ?? []}
          volumes={(nodeFilter !== undefined ? everything.data : query.data)?.items ?? []}
        />
      ) : null}
    </>
  );
}
