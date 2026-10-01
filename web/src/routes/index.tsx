// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";

import { AuthLayout } from "@/components/auth-layout";

// Placeholder landing page. The setup and sign-in plans replace this with
// redirect logic (setup needed, signed out, signed in).
function Landing() {
  const { t } = useTranslation();
  return <AuthLayout title={t("common.brand.wordmark")} />;
}

export const Route = createFileRoute("/")({
  component: Landing,
});
