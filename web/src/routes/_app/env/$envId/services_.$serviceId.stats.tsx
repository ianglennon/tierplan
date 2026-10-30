// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { serviceQuery } from "@/api/services";
import { ServiceStatsPanel } from "@/features/stats/stats-panel";

// The Stats tab of a service: the sum over its running tasks, live, with each task's own
// numbers under it (plan 06-33, RES-11, D-29). The page frame owns the gone state and the
// permission check; this draws the panel for the service the URL names.
function ServiceStatsRoute() {
  const { envId, serviceId } = Route.useParams();
  // The layout reads the detail too, so this is the cached answer; it names the surface.
  const detail = useQuery(serviceQuery(envId, serviceId));
  return (
    <ServiceStatsPanel envId={envId} serviceId={serviceId} name={detail.data?.name ?? serviceId} />
  );
}

export const Route = createFileRoute("/_app/env/$envId/services_/$serviceId/stats")({
  component: ServiceStatsRoute,
});
