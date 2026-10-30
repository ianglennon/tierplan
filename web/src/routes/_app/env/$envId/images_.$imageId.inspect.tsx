// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { imageInspectQuery, imageQuery } from "@/api/images";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { displayTitle } from "@/features/images/detail-layout";
import { useT } from "@/i18n/use-t";

// The Inspect tab of an image: its raw inspect document as the engine gave it, with the
// values of the image configuration's environment already replaced by the server for a caller
// who may not reveal secrets. The page frame owns the gone state; this draws the document, its
// own skeleton and its own load error.
function ImageInspectRoute() {
  const t = useT();
  const { envId, imageId } = Route.useParams();
  const { node } = Route.useSearch();
  const query = useQuery(imageInspectQuery(envId, imageId, node));
  // The layout reads the detail too, so this is the cached answer; it names the file.
  const detail = useQuery(imageQuery(envId, imageId, node));
  const name = detail.data
    ? displayTitle(detail.data)
    : imageId.replace(/^sha256:/, "").slice(0, 12);

  if (query.data) {
    return (
      <InspectJson
        name={name}
        value={query.data.inspect}
        masked={query.data.masked}
        maskedNotice={t("images.detail.inspect.masked")}
      />
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("images.detail.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("images.detail.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("images.detail.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId/inspect")({
  component: ImageInspectRoute,
});
