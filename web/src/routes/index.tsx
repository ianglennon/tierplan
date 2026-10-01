// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, redirect } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";

import { loadInstance } from "@/api/instance";
import { AuthLayout } from "@/components/auth-layout";

// Placeholder landing page. The sign-in plan adds the signed-out and signed-in
// redirects after this one.
function Landing() {
  const { t } = useTranslation();
  return <AuthLayout title={t("common.brand.wordmark")} />;
}

export const Route = createFileRoute("/")({
  // A fresh install has no admin yet, so every visit starts at the wizard (D-02).
  beforeLoad: async () => {
    const instance = await loadInstance();
    if (instance.needsSetup) throw redirect({ to: "/setup" });
  },
  component: Landing,
});
