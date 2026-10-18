// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { ConfigOverview } from "@/features/configs/overview";

// The Overview tab of a config: the page's first tab and its start address.
function ConfigOverviewRoute() {
  const { envId, configId } = Route.useParams();
  return <ConfigOverview envId={envId} configId={configId} />;
}

export const Route = createFileRoute("/_app/env/$envId/configs_/$configId/")({
  component: ConfigOverviewRoute,
});
