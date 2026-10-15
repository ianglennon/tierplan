// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { configDetailQuery } from "@/api/configs";
import { Skeleton } from "@/components/ui/skeleton";
import { ObjectUsersTable } from "@/components/used-by-list";
import { useT } from "@/i18n/use-t";

// The Used by tab of a config: every service whose spec mentions it, with the file, owner and
// mode it mounts it with, even a service with no running task. The page frame owns the load
// error and the gone state; this draws the table or its skeleton.
function ConfigUsedByRoute() {
  const t = useT();
  const { envId, configId } = Route.useParams();
  const { data } = useQuery(configDetailQuery(envId, configId));
  if (!data) {
    return (
      <div aria-busy="true">
        <span role="status" className="sr-only">
          {t("configs.detail.usedByTab.loading")}
        </span>
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  return <ObjectUsersTable envId={envId} users={data.usedByRows ?? []} kind="config" />;
}

export const Route = createFileRoute("/_app/env/$envId/configs_/$configId/used-by")({
  component: ConfigUsedByRoute,
});
