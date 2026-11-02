// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ImageLayers } from "@/features/images/layers";

// The Layers tab of an image: its history, newest layer first.
function ImageLayersRoute() {
  const { envId, imageId } = Route.useParams();
  const { node } = Route.useSearch();
  return <ImageLayers envId={envId} imageId={imageId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId/layers")({
  component: ImageLayersRoute,
});
