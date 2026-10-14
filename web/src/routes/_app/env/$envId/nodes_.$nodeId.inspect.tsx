// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { nodeInspectQuery, nodeQuery } from "@/api/nodes";
import { InspectJson } from "@/components/inspect-json";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

// The Inspect tab of a node: the manager's raw document of it. It holds the swarm's TLS trust root
// and the node certificate's subject (public) and no join token. The page frame owns the gone
// state; this draws the document, its own skeleton and its own load error.
function NodeInspectRoute() {
  const t = useT();
  const { envId, nodeId } = Route.useParams();
  const query = useQuery(nodeInspectQuery(envId, nodeId));
  // The layout reads the node too, so this is the cached answer; it names the file.
  const detail = useQuery(nodeQuery(envId, nodeId));
  const name = detail.data?.hostname ?? nodeId;

  if (query.data) {
    return <InspectJson name={name} value={query.data.document} masked={false} />;
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("nodes.inspect.loadFailed", { name })}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("nodes.inspect.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("nodes.inspect.loading")}
      </span>
      <Skeleton className="h-80 w-full" />
    </div>
  );
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/inspect")({
  component: NodeInspectRoute,
});
