// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { ServiceDetailLayout } from "@/features/service-detail/detail-layout";
import { usePermission } from "@/hooks/use-me";

// A service's page: the frame (breadcrumb, header, alerts, tab bar) around the tab the URL
// names. The trailing underscore on the file name keeps the page outside the list route, so it
// replaces the list instead of rendering under it.
function ServiceDetailRoute() {
  const { envId, serviceId } = Route.useParams();
  const allowed = usePermission("services.read", envId);
  if (!allowed) return <NoPermission />;
  return <ServiceDetailLayout envId={envId} serviceId={serviceId} />;
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId")({
  component: ServiceDetailRoute,
});
