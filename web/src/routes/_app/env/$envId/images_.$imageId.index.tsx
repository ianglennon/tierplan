// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ImageOverview } from "@/features/images/overview";

// The Overview tab of an image: the page's first tab and its start address.
function ImageOverviewRoute() {
  const { envId, imageId } = Route.useParams();
  const { node } = Route.useSearch();
  return <ImageOverview envId={envId} imageId={imageId} node={node} />;
}

export const Route = createFileRoute("/_app/env/$envId/images_/$imageId/")({
  component: ImageOverviewRoute,
});
