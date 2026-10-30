// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { VolumeDetailLayout } from "@/features/volumes/detail-layout";
import { usePermission } from "@/hooks/use-me";

// A volume's page: the frame (breadcrumb, header, tab bar) around the tab the URL names.
// The trailing underscore on the file name keeps the page outside the list route, so it
// replaces the list instead of rendering under it. The search carries the node the volume
// is on: a name repeats per node, so the server needs it to find the right one on a swarm.
function VolumeDetailRoute() {
  const { envId, volumeName } = Route.useParams();
  const { node } = Route.useSearch();
  const allowed = usePermission("volumes.read", envId);
  if (!allowed) return <NoPermission />;
  return <VolumeDetailLayout envId={envId} volumeName={volumeName} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/volumes_/$volumeName")({
  validateSearch: nodeSearchSchema,
  component: VolumeDetailRoute,
});
