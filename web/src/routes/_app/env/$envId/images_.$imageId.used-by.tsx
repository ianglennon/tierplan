// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { ImageUsedBy } from "@/features/images/used-by";

// The Used by tab of an image: the containers and swarm services that use it.
function ImageUsedByRoute() {
  const { envId, imageId } = Route.useParams();
  const { node } = Route.useSearch();
  return <ImageUsedBy envId={envId} imageId={imageId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId/used-by")({
  component: ImageUsedByRoute,
});
