// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

export type RestartLoop = components["schemas"]["RestartLoopBody"];

interface RestartLoopBadgeProps {
  loop: RestartLoop;
}

// RestartLoopBadge flags a service whose tasks keep failing (RES-13, D-15): an outline badge
// with the offline dot and "Restart loop" in the offline text colour. The user confirmed that
// this treatment, and not a red fill, meets D-15's red badge (UI-SPEC Color "Restart-loop badge
// treatment"). A tooltip gives the numbers and the latest error, clamped to three lines. The
// badge sits beside a row's or a header's state and never replaces it. The error is Docker's
// text and is drawn as a text node.
export function RestartLoopBadge({ loop }: RestartLoopBadgeProps) {
  const t = useT();
  const window = t("restartLoop.minutes", { count: loop.windowMinutes });
  const tooltip = loop.latestError
    ? t("restartLoop.tooltip.latest", { count: loop.count, window, error: loop.latestError })
    : t("restartLoop.tooltip.count", { count: loop.count, window });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Badge variant="outline" className="shrink-0" tabIndex={0}>
          <StatusDot state="offline" label={t("restartLoop.badge")} />
        </Badge>
      </TooltipTrigger>
      <TooltipContent className="max-w-xs">
        <span className="line-clamp-3 break-words">{tooltip}</span>
      </TooltipContent>
    </Tooltip>
  );
}
