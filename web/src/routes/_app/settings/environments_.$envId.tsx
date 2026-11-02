// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { EnvSettingsLayout } from "@/features/environments/env-settings-layout";

// Settings > Environments > {env} (D-38): the frame with the breadcrumb, the title
// and the tab bar "Settings" / "Access" (D-25), around the tab the URL names. The
// trailing underscore on the file name keeps this page inside the Settings layout
// but outside the list route, so it replaces the list instead of rendering under it.
function EnvironmentSettingsLayoutRoute() {
  const { envId } = Route.useParams();
  return <EnvSettingsLayout envId={envId} />;
}

export const Route = createFileRoute("/_app/settings/environments_/$envId")({
  component: EnvironmentSettingsLayoutRoute,
});
