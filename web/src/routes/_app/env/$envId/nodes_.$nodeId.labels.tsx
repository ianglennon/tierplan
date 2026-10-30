// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { nodeQuery } from "@/api/nodes";
import { Skeleton } from "@/components/ui/skeleton";
import { NodeLabelsEditor } from "@/features/nodes/labels-editor";

// The Labels tab of a node: the labels services use in their placement constraints, with the
// editor and the Effect on services for an Env Admin (a lower role sees the labels read-only).
// The page frame owns the gone state and the load error, so this draws the editor once the node
// is there. The editor is keyed by the node, so moving to another node starts it clean.
function NodeLabelsRoute() {
  const { envId, nodeId } = Route.useParams();
  const { data } = useQuery(nodeQuery(envId, nodeId));
  if (!data) return <Skeleton className="h-40 w-full" />;
  return <NodeLabelsEditor key={data.id} envId={envId} node={data} />;
}

export const Route = createFileRoute("/_app/env/$envId/nodes_/$nodeId/labels")({
  component: NodeLabelsRoute,
});
