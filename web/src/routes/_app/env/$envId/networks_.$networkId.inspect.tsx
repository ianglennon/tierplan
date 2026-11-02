// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { networkDetailQuery, networkInspectQuery } from "@/api/networks";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a network: its raw inspect document, as the engine gave it. The
// page frame owns the gone state; this draws the document, its own skeleton and its own
// load error. Nothing in a network's document is withheld, so no value is masked.
function NetworkInspectRoute() {
  const t = useT();
  const { envId, networkId } = Route.useParams();
  const { node } = Route.useSearch();
  const query = useQuery(networkInspectQuery(envId, networkId, node));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(networkDetailQuery(envId, networkId, node));
  const name = detail.data?.name ?? networkId;

  if (query.data) {
    return <InspectJson name={name} value={query.data.inspect} masked={false} />;
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("networks.detail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("networks.detail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("networks.detail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/networks_/$networkId/inspect")({
  component: NetworkInspectRoute,
});
