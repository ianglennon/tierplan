// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { UsersTable } from "@/features/access/users-table";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > Access > Users. The parent route has already refused anyone
// without users.manage, and the server refuses the list again.
function UsersTab() {
  const t = useT();
  useDocumentTitle(t("users.tabTitle"));
  return <UsersTable />;
}

export const Route = createFileRoute("/_app/settings/access/users")({
  component: UsersTab,
});
