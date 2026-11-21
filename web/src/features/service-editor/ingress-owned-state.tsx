// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Lock } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { useT } from "@/i18n/use-t";

interface IngressOwnedStateProps {
  envId: string;
  serviceId: string;
}

// IngressOwnedState is what the edit page becomes for TierPlan's ingress proxy service (UI-SPEC S3,
// D-29): TierPlan creates and updates that service itself, so the page offers no form, review or
// apply, only the way back to the service's Overview. The server refuses every change to it too.
export function IngressOwnedState({ envId, serviceId }: IngressOwnedStateProps) {
  const t = useT();
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Lock aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={2} className="break-words">
          {t("ingress.owned.editorTitle")}
        </EmptyTitle>
        <EmptyDescription>{t("ingress.owned.editorBody")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/services/$serviceId" params={{ envId, serviceId }}>
            {t("ingress.owned.back")}
          </Link>
        </Button>
      </EmptyContent>
    </Empty>
  );
}
