// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Waypoints } from "lucide-react";
import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface IngressOffEmptyProps {
  envId: string;
  // Replaces the default "Turn on ingress" link to the Settings tab. Without one, an ingress.manage holder gets that
  // link and a reader is told to ask an Admin.
  action?: ReactNode;
}

// IngressOffEmpty is the Ingress page's state for an environment whose ingress is off (UI-SPEC S8, D-03). The
// Routes, Certificates and Nodes tabs each draw it in place of a list that cannot have anything in it. A reader
// who cannot turn ingress on is told whom to ask. The flow itself lives on the Settings tab, so the action only
// navigates there and never writes (T-27-77).
export function IngressOffEmpty({ envId, action }: IngressOffEmptyProps) {
  const t = useT();
  const manage = useNeeds("ingress.manage", envId);
  const content =
    action !== undefined ? (
      action
    ) : manage.allowed ? (
      <Button asChild className="max-md:h-11">
        <Link to="/env/$envId/ingress/settings" params={{ envId }}>
          {t("ingressPage.off.turnOn")}
        </Link>
      </Button>
    ) : null;
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Waypoints aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={3}>
          {t("ingressPage.off.title")}
        </EmptyTitle>
        <EmptyDescription>
          {t("ingressPage.off.body")}
          {action === undefined && !manage.allowed ? ` ${t("ingressPage.off.reader")}` : ""}
        </EmptyDescription>
      </EmptyHeader>
      {content === null ? null : <EmptyContent>{content}</EmptyContent>}
    </Empty>
  );
}
