// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute, redirect } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";

import { loadInstance } from "@/api/instance";
import { AuthLayout } from "@/components/auth-layout";
import { SetupWizard } from "@/features/setup/setup-wizard";

function SetupPage() {
  const { t } = useTranslation();
  // The wordmark, not the instance name: no instance exists yet (UI-SPEC).
  return (
    <AuthLayout title={t("common.brand.wordmark")} wide>
      <SetupWizard />
    </AuthLayout>
  );
}

export const Route = createFileRoute("/setup")({
  // Once an admin exists the wizard is closed; the server also redirects
  // /setup to /login for a full page load.
  beforeLoad: async () => {
    const instance = await loadInstance();
    if (!instance.needsSetup) throw redirect({ to: "/" });
  },
  component: SetupPage,
});
