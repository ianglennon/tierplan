// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { HttpsPage } from "@/features/https/https-page";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > HTTPS (D-08, D-10). settings.read is global and no new permission is
// added. The server checks it again on every route.
function HttpsTab() {
  const t = useT();
  useDocumentTitle(t("https.tabTitle"));
  const allowed = usePermission("settings.read");
  if (!allowed) return <NoPermission />;
  return <HttpsPage />;
}

export const Route = createFileRoute("/_app/settings/https")({
  component: HttpsTab,
});
