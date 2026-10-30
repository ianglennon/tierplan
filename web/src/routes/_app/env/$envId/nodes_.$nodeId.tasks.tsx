// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { nodeTasksQuery } from "@/api/nodes";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { NodeTasksSkeleton, NodeTaskTable } from "@/features/tasks/task-table";
import { useT } from "@/i18n/use-t";

// The Tasks tab of a node: the tasks the swarm placed on it, polled every 10 s while the page is
// visible. The page frame owns the gone state; this draws the table, its own skeleton and its own
// load error.
function NodeTasksRoute() {
  const t = useT();
  const { envId, nodeId } = Route.useParams();
  const query = useQuery(nodeTasksQuery(envId, nodeId));

  if (query.data) {
    return (
      <NodeTaskTable
        envId={envId}
        tasks={query.data.items ?? []}
        total={query.data.total}
        capped={query.data.capped}
      />
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("nodes.tasks.error.title")}</span>
          <span>{t("nodes.tasks.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("nodes.tasks.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return <NodeTasksSkeleton />;
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/tasks")({
  component: NodeTasksRoute,
});
