// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { TeamsTable } from "@/features/access/teams-table";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > Access > Teams. Teams are managed by instance admins only
// (teams.manage is global, D-11), and the server refuses the list again.
function TeamsTab() {
  const t = useT();
  const allowed = usePermission("teams.manage");
  useDocumentTitle(t("teams.tabTitle"));
  if (!allowed) return <NoPermission />;
  return <TeamsTable />;
}

export const Route = createFileRoute("/_app/settings/access/teams")({
  component: TeamsTab,
});
