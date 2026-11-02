// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ListRestart } from "lucide-react";

import { stackQuery } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface RestoreActionProps {
  envId: string;
  stackId: string;
  // True when the newest check found drift to put back.
  drifted: boolean;
  // The page's footer button is the accent one, the Overview alert's is outline (UI-SPEC "Drift").
  variant: "default" | "outline";
  // The alert's label ends in an ellipsis: it opens the review and changes nothing by itself.
  ellipsis?: boolean;
}

// RestoreAction is "Redeploy to restore" (D-23): it opens the review in restore mode, which shows what the redeploy
// would change and is where a person confirms it. It restores nothing itself. It needs Operator, waits while another
// stack operation holds the environment and has nothing to do without drift; the reason is shown in each case and the
// server refuses the same things again.
export function RestoreAction({
  envId,
  stackId,
  drifted,
  variant,
  ellipsis = false,
}: RestoreActionProps) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const envName = useEnvName(envId);
  const label = t(ellipsis ? "stackRestore.actionAlert" : "stackRestore.action");
  const reason = !needs.allowed
    ? (needs.reason ?? t("needs.role.operator"))
    : stack.data?.lock
      ? t("stackConverge.lock.reason", { env: envName })
      : drifted
        ? undefined
        : t("stackRestore.noDrift");
  if (reason) {
    return (
      <NeedsTooltip reason={reason}>
        <Button type="button" variant={variant} className="max-md:h-11">
          <ListRestart aria-hidden="true" />
          {label}
        </Button>
      </NeedsTooltip>
    );
  }
  return (
    <Button asChild variant={variant} className="max-md:h-11">
      <Link
        to="/env/$envId/stacks/$stackId/review"
        params={{ envId, stackId }}
        search={{ mode: "restore" }}
      >
        <ListRestart aria-hidden="true" />
        {label}
      </Link>
    </Button>
  );
}
