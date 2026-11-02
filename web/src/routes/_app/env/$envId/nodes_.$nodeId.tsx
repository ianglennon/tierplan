// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { NodeDetailLayout } from "@/features/nodes/detail-layout";
import { usePermission } from "@/hooks/use-me";

// A node's page: the frame (breadcrumb, header, tab bar) around the tab the URL names. The
// trailing underscore on the file name keeps the page outside the list route, so it replaces the
// list instead of rendering under it.
function NodeDetailRoute() {
  const { envId, nodeId } = Route.useParams();
  const allowed = usePermission("nodes.read", envId);
  if (!allowed) return <NoPermission />;
  return <NodeDetailLayout envId={envId} nodeId={nodeId} />;
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId")({
  component: NodeDetailRoute,
});
