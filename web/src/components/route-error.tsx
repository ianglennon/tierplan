// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ErrorComponentProps } from "@tanstack/react-router";
import { useRouter } from "@tanstack/react-router";
import { CloudOff } from "lucide-react";
import { useTranslation } from "react-i18next";

import { AuthLayout } from "@/components/auth-layout";
import { Button } from "@/components/ui/button";
import { Empty, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";

// RouteError is what any route shows when its guard or loader throws, most
// often a network or 5xx failure on GET /api/instance. It sits in AuthLayout so
// it works before sign-in too and carries the one version footer (PLAT-05).
// The error object is deliberately ignored: a raw message is never shown.
export function RouteError(_props?: Partial<ErrorComponentProps>) {
  const { t } = useTranslation();
  const router = useRouter();
  return (
    <AuthLayout title={t("common.brand.wordmark")}>
      <Empty className="border-0 p-0">
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <CloudOff aria-hidden="true" />
          </EmptyMedia>
          <EmptyTitle className="text-base">{t("common.errors.unreachable")}</EmptyTitle>
        </EmptyHeader>
        {/* invalidate re-runs the guards and resets the error boundary. */}
        <Button type="button" onClick={() => void router.invalidate()}>
          {t("common.errors.retry")}
        </Button>
      </Empty>
    </AuthLayout>
  );
}
