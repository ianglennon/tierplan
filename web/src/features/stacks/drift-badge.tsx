// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { GitCompareArrows } from "lucide-react";
import { useId } from "react";

import type { StackItem } from "@/api/stacks";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

interface DriftBadgeProps {
  // The newest drift check the list carries for the stack, absent until it has one.
  drift: StackItem["drift"];
}

// DriftBadge marks a stack whose running state no longer matches its deployed revision (UI-SPEC "Stack lists"). It is
// an outline badge with a warning icon after the stack's name, and draws nothing unless the newest check found drift:
// a stack not yet checked, stopped, never deployed or one that could not be compared shows no badge. The Tooltip says
// which revision the stack drifted from, and the same words are the badge's description for a reader who never
// opens the Tooltip.
export function DriftBadge({ drift }: DriftBadgeProps) {
  const t = useT();
  const descriptionId = useId();
  if (drift?.state !== "drifted") return null;
  const tip = t("stackDriftList.tooltip", { revision: drift.revision });
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
