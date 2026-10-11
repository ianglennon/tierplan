// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { volumeDetailQuery, volumeInspectQuery } from "@/api/volumes";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a volume: its raw inspect document, as the engine gave it, with the
// option values the caller may not see already replaced by the server. The page frame owns
// the gone state; this draws the document, its own skeleton and its own load error.
function VolumeInspectRoute() {
  const t = useT();
  const { envId, volumeName } = Route.useParams();
  const { node } = Route.useSearch();
  const query = useQuery(volumeInspectQuery(envId, volumeName, node));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(volumeDetailQuery(envId, volumeName, node));
  const name = detail.data?.name ?? volumeName;

  if (query.data) {
    return (
      <InspectJson
        name={name}
        value={query.data.inspect}
        masked={query.data.masked}
        maskedNotice={t("volumes.detail.inspect.masked")}
      />
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("volumes.detail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("volumes.detail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("volumes.detail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/volumes_/$volumeName/inspect")({
  component: VolumeInspectRoute,
});
