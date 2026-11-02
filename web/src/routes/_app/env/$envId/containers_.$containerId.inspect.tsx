// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { containerDetailQuery, containerInspectQuery } from "@/api/container-detail";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a container: its raw inspect document, masked on the server
// for a caller without secrets.reveal. The page frame owns the gone state; this
// draws the document, its own skeleton and its own load error.
function ContainerInspectRoute() {
  const t = useT();
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  const query = useQuery(containerInspectQuery(envId, containerId, node));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(containerDetailQuery(envId, containerId, node));
  const name = detail.data?.name ?? containerId;

  if (query.data) {
    return <InspectJson name={name} value={query.data.inspect} masked={query.data.masked} />;
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("containerdetail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("containerdetail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("containerdetail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId/inspect")({
  component: ContainerInspectRoute,
});
