// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { RoutesTab } from "@/features/ingress/routes-tab";

// The Routes tab, the Ingress page's index: the page frame is the layout route above it.
function IngressRoutesRoute() {
  const { envId } = Route.useParams();
  return <RoutesTab envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/ingress/")({
  component: IngressRoutesRoute,
});
