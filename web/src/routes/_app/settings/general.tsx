// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { GeneralForm } from "@/features/settings/general-form";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > General (D-03, D-04, D-60). Reading needs settings.read; the form
// is read-only without settings.write, and the server checks both again.
function GeneralTab() {
  const t = useT();
  useDocumentTitle(t("settings.general.tabTitle"));
  const allowed = usePermission("settings.read");
  if (!allowed) return <NoPermission />;
  return <GeneralForm />;
}

export const Route = createFileRoute("/_app/settings/general")({
  component: GeneralTab,
});
