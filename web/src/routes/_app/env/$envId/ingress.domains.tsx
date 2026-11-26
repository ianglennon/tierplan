// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { DomainsTab } from "@/features/ingress/domains-tab";

// The Domains tab, a child of the Ingress page: the frame is the layout route above it. It works while ingress is
// off, so it sits outside the gate the Routes, Certificates and Nodes tabs draw (D-03).
function IngressDomainsRoute() {
  const { envId } = Route.useParams();
  return <DomainsTab envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/ingress/domains")({
  component: IngressDomainsRoute,
});
