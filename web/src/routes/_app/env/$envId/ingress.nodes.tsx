// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { NodesTab } from "@/features/ingress/nodes-tab";

// The Nodes tab, a child of the Ingress page: the frame is the layout route above it.
function IngressNodesRoute() {
  const { envId } = Route.useParams();
  return <NodesTab envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/ingress/nodes")({
  component: IngressNodesRoute,
});
