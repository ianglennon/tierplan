// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { serviceInspectQuery, serviceQuery } from "@/api/services";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a service: its raw inspect document, masked on the server for a caller
// without secrets.reveal, in Spec and in PreviousSpec. The page frame owns the gone state; this
// draws the document, its own skeleton and its own load error.
function ServiceInspectRoute() {
  const t = useT();
  const { envId, serviceId } = Route.useParams();
  const query = useQuery(serviceInspectQuery(envId, serviceId));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(serviceQuery(envId, serviceId));
  const name = detail.data?.name ?? serviceId;

  if (query.data) {
    return (
      <InspectJson
        name={name}
        value={query.data.document}
        masked={query.data.masked}
        maskedNotice={t("servicedetail.inspect.masked")}
      />
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("servicedetail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("servicedetail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("servicedetail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/inspect")({
  component: ServiceInspectRoute,
});
