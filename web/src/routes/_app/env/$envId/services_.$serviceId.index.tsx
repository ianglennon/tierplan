// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ServiceOverview } from "@/features/service-detail/overview";

// The Overview tab of a service: the page's first tab and its start address.
function ServiceOverviewRoute() {
  const { envId, serviceId } = Route.useParams();
  return <ServiceOverview envId={envId} serviceId={serviceId} />;
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/")({
  component: ServiceOverviewRoute,
});
