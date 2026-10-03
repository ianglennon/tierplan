// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { EnvSettingsPage } from "@/features/environments/env-settings-page";

// Settings > Environments > {env} (D-38). The trailing underscore on the file
// name keeps this page inside the Settings layout but outside the list route,
// so it replaces the list instead of rendering under it.
function EnvironmentSettingsRoute() {
  const { envId } = Route.useParams();
  return <EnvSettingsPage envId={envId} />;
}

export const Route = createFileRoute("/_app/settings/environments_/$envId")({
  component: EnvironmentSettingsRoute,
});
