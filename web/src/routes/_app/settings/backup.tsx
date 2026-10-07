// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { BackupPage } from "@/features/backup/backup-page";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > Backup (D-28, D-60). Instance admins only: backup.manage is global.
// The server checks it again on every route.
function BackupTab() {
  const t = useT();
  useDocumentTitle(t("backup.tabTitle"));
  const allowed = usePermission("backup.manage");
  if (!allowed) return <NoPermission />;
  return <BackupPage />;
}

export const Route = createFileRoute("/_app/settings/backup")({
  component: BackupTab,
});
