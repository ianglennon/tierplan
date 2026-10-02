// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { EnvironmentsSettings } from "@/features/environments/environments-settings";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > Environments (D-20, D-60). Rename, remove and add change the
// registry, so the tab needs environments.write; the server checks it again.
function EnvironmentsTab() {
  const t = useT();
  useDocumentTitle(t("settings.environments.tabTitle"));
  const allowed = usePermission("environments.write");
  if (!allowed) return <NoPermission />;
  return <EnvironmentsSettings />;
}

export const Route = createFileRoute("/_app/settings/environments")({
  component: EnvironmentsTab,
});
