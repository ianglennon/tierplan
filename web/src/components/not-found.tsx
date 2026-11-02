// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { FileQuestion } from "lucide-react";
import { useTranslation } from "react-i18next";

import { AuthLayout } from "@/components/auth-layout";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";

// NotFound is rendered for any client route that does not exist. It sits in
// AuthLayout so it works before sign-in too (UI-SPEC "Not found"); a signed-in
// user also gets Log out there.
export function NotFound() {
  const { t } = useTranslation();
  return (
    <AuthLayout title={t("common.brand.wordmark")} showSignOut>
      <Empty className="border-0 p-0">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <FileQuestion aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle>{t("common.notFound.heading")}</EmptyTitle>
          <EmptyDescription>{t("common.notFound.body")}</EmptyDescription>
        </EmptyHeader>
        <Button asChild>
          <Link to="/">{t("common.notFound.action")}</Link>
        </Button>
      </Empty>
    </AuthLayout>
  );
}
