// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { GeneralForm } from "@/features/settings/general-form";
import { ShellSettingsCard } from "@/features/settings/shell-settings-card";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > General (D-03, D-04, D-60) and the Container shells card (D-29, D-31).
// Reading needs settings.read; the form and the card are read-only without
// settings.write, and the server checks both again.
function GeneralTab() {
  const t = useT();
  useDocumentTitle(t("settings.general.tabTitle"));
  const allowed = usePermission("settings.read");
  if (!allowed) return <NoPermission />;
  return (
    <div className="flex flex-col gap-8">
      <GeneralForm />
      <ShellSettingsCard />
    </div>
  );
}

export const Route = createFileRoute("/_app/settings/general")({
  component: GeneralTab,
});
