// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { EnvSettingsPage } from "@/features/environments/env-settings-page";

// The Settings tab of an environment's page: the cards that were the whole page
// before it had tabs.
function EnvironmentSettingsRoute() {
  const { envId } = Route.useParams();
  return <EnvSettingsPage envId={envId} />;
}

export const Route = createFileRoute("/_app/settings/environments_/$envId/")({
  component: EnvironmentSettingsRoute,
});
