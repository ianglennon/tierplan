// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { BellOff } from "lucide-react";

import type { components } from "@/api/schema";
import { StatusDot } from "@/components/status-dot";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

export type RestartLoop = components["schemas"]["RestartLoopBody"];
export type ContainerRestartLoop = components["schemas"]["ContainerRestartLoopBody"];

interface RestartLoopBadgeProps {
  // The loop, while the service or the container is in one. Its state says whether the alerts
  // are muted, and a container's loop (kind "container") tells restarts and the last exit code
  // where a service's tells failed tasks and the latest error.
  loop?: RestartLoop | ContainerRestartLoop;
  // True when the service's alerts are muted, looping or not: the detail header shows the
  // Muted badge whenever it is (D-16). A list passes only the loop, so a quiet muted service
  // shows nothing there.
  muted?: boolean;
  // What the badge is for, in the muted tooltip.
  kind?: "service" | "container";
}

// RestartLoopBadge flags a service whose tasks keep failing, or a standalone container that keeps
// dying (RES-13, D-15): an outline badge
// with the offline dot and "Restart loop" in the offline text colour. The user confirmed that
// this treatment, and not a red fill, meets D-15's red badge (UI-SPEC Color "Restart-loop badge
// treatment"). A tooltip gives the numbers and the latest error, clamped to three lines. The
// badge sits beside a row's or a header's state and never replaces it. The error is Docker's
// text and is drawn as a text node.
//
// Once the alerts are muted (D-16) it is the muted variant instead: a neutral outline badge with
// BellOff and "Muted", whose tooltip says where to unmute. It draws nothing for a service that
// is neither in a loop nor muted.
export function RestartLoopBadge({ loop, muted = false, kind = "service" }: RestartLoopBadgeProps) {
  const t = useT();
  if (!loop && !muted) return null;
  if (!loop || loop.state === "muted") {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Badge variant="outline" className="shrink-0" tabIndex={0}>
            <BellOff aria-hidden="true" />
            {t("restartLoop.muted.badge")}
          </Badge>
        </TooltipTrigger>
        <TooltipContent className="max-w-xs">
          {t(
            kind === "container"
              ? "restartLoop.muted.tooltipContainer"
              : "restartLoop.muted.tooltipService",
          )}
        </TooltipContent>
      </Tooltip>
    );
  }
  const window = t("restartLoop.minutes", { count: loop.windowMinutes });
  let tooltip: string;
  if ("lastExitCode" in loop) {
    tooltip = t("restartLoop.container.tooltip", {
      count: loop.count,
      window,
      code: loop.lastExitCode,
    });
  } else if (loop.latestError) {
    tooltip = t("restartLoop.tooltip.latest", {
      count: loop.count,
      window,
      error: loop.latestError,
    });
  } else {
    tooltip = t("restartLoop.tooltip.count", { count: loop.count, window });
  }
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
