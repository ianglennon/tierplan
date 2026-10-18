// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { useState } from "react";

import { serviceQuery, serviceTasksQuery } from "@/api/services";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { canScale, itemOf } from "@/features/service-detail/header-actions";
import { ScaleDialog } from "@/features/services/scale-dialog";
import { TasksSkeleton, TaskTable } from "@/features/tasks/task-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

// The Tasks tab of a service: its slots with the history Docker keeps, polled every 5 s while
// the page is visible. The page frame owns the gone state; this draws the table, its own
// skeleton and its own load error. Show only failed is in the URL (failed=1), so the view can
// be linked to.
const tasksSearchSchema = z.object({
  failed: z.literal(1).optional().catch(undefined),
});

function ServiceTasksRoute() {
  const t = useT();
  const { envId, serviceId } = Route.useParams();
  const { failed } = Route.useSearch();
  const navigate = Route.useNavigate();
  const query = useQuery(serviceTasksQuery(envId, serviceId));
  // The layout reads the detail too, so this is the cached answer; it names the tasks and says
  // whether the service has a replica count to set.
  const detail = useQuery(serviceQuery(envId, serviceId)).data;
  const needs = useNeeds("services.control", envId);
  const [scaling, setScaling] = useState(false);

  if (query.data) {
    const mayScale = needs.allowed && detail !== undefined && canScale(detail);
    return (
      <>
        <TaskTable
          envId={envId}
          serviceId={serviceId}
          serviceName={detail?.name}
          slots={query.data.slots ?? []}
          retention={query.data.retention}
          global={query.data.global}
          showFailedOnly={failed === 1}
          onShowFailedOnlyChange={(on) =>
            void navigate({ search: on ? { failed: 1 } : {}, replace: true })
          }
          onScale={mayScale ? () => setScaling(true) : undefined}
        />
        {mayScale ? (
          <ScaleDialog
            envId={envId}
            targets={[itemOf(detail)]}
            open={scaling}
            onOpenChange={setScaling}
          />
        ) : null}
      </>
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("tasks.error.title")}</span>
          <span>{t("tasks.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("tasks.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return <TasksSkeleton />;
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/tasks")({
  validateSearch: tasksSearchSchema,
  component: ServiceTasksRoute,
});
