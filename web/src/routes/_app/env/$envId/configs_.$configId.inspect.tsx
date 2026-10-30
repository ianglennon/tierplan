// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { configDetailQuery, configInspectQuery } from "@/api/configs";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a config: the manager's inspect document, as it gave it. Like the detail it
// carries the content, as base64. The page frame owns the gone state; this draws the document, its
// own skeleton and its own load error.
function ConfigInspectRoute() {
  const t = useT();
  const { envId, configId } = Route.useParams();
  const query = useQuery(configInspectQuery(envId, configId));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(configDetailQuery(envId, configId));
  const name = detail.data?.name ?? configId;

  if (query.data) {
    return <InspectJson name={name} value={query.data.inspect} masked={false} />;
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("configs.detail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("configs.detail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("configs.detail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/configs_/$configId/inspect")({
  component: ConfigInspectRoute,
});
