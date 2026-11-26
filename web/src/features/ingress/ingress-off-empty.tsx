// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Waypoints } from "lucide-react";
import type { ReactNode } from "react";

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
  // What an Admin does about it: the "Turn on ingress" button the Settings plan supplies. Without one, nothing is
  // drawn for an Admin, and a reader is told to ask an Admin.
  action?: ReactNode;
}

// IngressOffEmpty is the Ingress page's state for an environment whose ingress is off (UI-SPEC S8, D-03). The
// Routes, Certificates and Nodes tabs each draw it in place of a list that cannot have anything in it. A reader
// who cannot turn ingress on is told whom to ask; the action is a slot, filled by the plan that builds the flow.
export function IngressOffEmpty({ envId, action }: IngressOffEmptyProps) {
  const t = useT();
  const manage = useNeeds("ingress.manage", envId);
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
      {action === undefined ? null : <EmptyContent>{action}</EmptyContent>}
    </Empty>
  );
}
