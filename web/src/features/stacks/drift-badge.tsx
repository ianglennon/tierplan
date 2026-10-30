// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { GitCompareArrows } from "lucide-react";
import { useId } from "react";

import type { StackItem } from "@/api/stacks";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useCauseText } from "@/features/stack-drift/drift-cause";
import { useT } from "@/i18n/use-t";

interface DriftBadgeProps {
  // The newest drift check the list carries for the stack, absent until it has one.
  drift: StackItem["drift"];
}

// DriftBadge marks a stack whose running state no longer matches its deployed revision (UI-SPEC "Stack lists"). It is
// an outline badge with a warning icon after the stack's name, and draws nothing unless the newest check found drift:
// a stack not yet checked, stopped, never deployed or one that could not be compared shows no badge. The Tooltip says
// which revision the stack drifted from and, when the audit log knows, who changed what in Coaming, and the same words are the badge's description for a reader who never
// opens the Tooltip.
export function DriftBadge({ drift }: DriftBadgeProps) {
  const t = useT();
  const descriptionId = useId();
  const cause = useCauseText({ cause: drift?.cause, count: drift?.causes });
  if (drift?.state !== "drifted") return null;
  const base = t("stackDriftList.tooltip", { revision: drift.revision });
  // The cause is added only when the audit log knows it: "Changed outside Coaming." is for the page, not the Tooltip.
  const tip = cause ? `${base} ${cause}` : base;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0" aria-describedby={descriptionId}>
          <GitCompareArrows className="text-status-warning-text" aria-hidden="true" />
          {t("stackDriftList.badge")}
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
      <span id={descriptionId} className="sr-only">
        {tip}
      </span>
    </Tooltip>
  );
}
