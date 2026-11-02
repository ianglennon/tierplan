// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { DetailLayout } from "@/features/container-detail/detail-layout";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { usePermission } from "@/hooks/use-me";

// A container's page: the frame (breadcrumb, header, tab bar) around the tab the
// URL names. The trailing underscore on the file name keeps the page outside the
// list route, so it replaces the list instead of rendering under it. The search
// carries the node the container is on, which the server needs to find it on a
// swarm.
function ContainerDetailRoute() {
  const { envId, containerId } = Route.useParams();
  const { node } = Route.useSearch();
  const allowed = usePermission("containers.read", envId);
  if (!allowed) return <NoPermission />;
  return <DetailLayout envId={envId} containerId={containerId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/containers_/$containerId")({
  validateSearch: nodeSearchSchema,
  component: ContainerDetailRoute,
});
