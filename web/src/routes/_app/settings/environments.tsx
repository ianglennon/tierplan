// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { EnvironmentsSettings } from "@/features/environments/environments-settings";
import { useAnyEnvironmentPermission, useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > Environments (D-20, D-60). The tab is for someone who administers at
// least one environment: environments.configure on any (D-07). The list then shows
// only those; the server checks every action again.
function EnvironmentsTab() {
  const t = useT();
  useDocumentTitle(t("settings.environments.tabTitle"));
  const allowed = useAnyEnvironmentPermission("environments.configure");
  if (!allowed) return <NoPermission />;
  return <EnvironmentsSettings />;
}

export const Route = createFileRoute("/_app/settings/environments")({
  component: EnvironmentsTab,
});
