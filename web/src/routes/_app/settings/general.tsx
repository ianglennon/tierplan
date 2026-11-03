// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { GeneralForm } from "@/features/settings/general-form";
import { JanitorDefaultsCard } from "@/features/settings/janitor-defaults-card";
import { ShellSettingsCard } from "@/features/settings/shell-settings-card";
import { StackHistoryCard } from "@/features/settings/stack-history-card";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Settings > General (D-03, D-04, D-60), the Container shells card (D-29, D-31), the Stack history
// card (D-01) and the Janitor defaults card (anchor #janitor-defaults, Phase 11 D-04, D-16).
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
      <StackHistoryCard />
      <JanitorDefaultsCard />
    </div>
  );
}

export const Route = createFileRoute("/_app/settings/general")({
  component: GeneralTab,
});
