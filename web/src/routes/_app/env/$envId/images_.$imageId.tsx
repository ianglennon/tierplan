// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ImageDetailLayout } from "@/features/images/detail-layout";
import { nodeSearchSchema } from "@/features/resources/swarm-lists";
import { usePermission } from "@/hooks/use-me";

// An image's page: the frame (breadcrumb, header, tab bar) around the tab the URL names.
// The trailing underscore on the file name keeps the page outside the list route, so it
// replaces the list instead of rendering under it. The search may carry the node to read
// the image on; without it every node that has the image is read.
function ImageDetailRoute() {
  const { envId, imageId } = Route.useParams();
  const { node } = Route.useSearch();
  const allowed = usePermission("images.read", envId);
  if (!allowed) return <NoPermission />;
  return <ImageDetailLayout envId={envId} imageId={imageId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId")({
  validateSearch: nodeSearchSchema,
  component: ImageDetailRoute,
});
