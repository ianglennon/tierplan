// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { NetworkDetailLayout } from "@/features/networks/detail-layout";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { usePermission } from "@/hooks/use-me";

// A network's page: the frame (breadcrumb, header, tab bar) around the tab the URL
// names. The trailing underscore on the file name keeps the page outside the list
// route, so it replaces the list instead of rendering under it. The search carries
// the node a local network is on, which the server needs to find it on a swarm.
function NetworkDetailRoute() {
  const { envId, networkId } = Route.useParams();
  const { node } = Route.useSearch();
  const allowed = usePermission("networks.read", envId);
  if (!allowed) return <NoPermission />;
  return <NetworkDetailLayout envId={envId} networkId={networkId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/networks_/$networkId")({
  validateSearch: nodeSearchSchema,
  component: NetworkDetailRoute,
});
