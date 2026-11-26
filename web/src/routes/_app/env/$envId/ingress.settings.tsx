// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";
import { SettingsTab } from "@/features/ingress/settings-tab";

// The Settings tab, a child of the Ingress page: the frame is the layout route above it. It draws while ingress is
// off, since the switch that turns it on is here (D-03).
function IngressSettingsRoute() {
  const { envId } = Route.useParams();
  return <SettingsTab envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/ingress/settings")({
  component: IngressSettingsRoute,
});
