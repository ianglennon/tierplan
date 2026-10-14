// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import type { HealthState } from "@/api/environments";
import { StatusDot } from "@/components/status-dot";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { PlacementTask } from "@/features/placement/project";
import { taskState } from "@/features/tasks/task-state";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The 2px bar on the card's left edge is the task state's colour. The state's word is always
// beside it (UI-SPEC "Color"): colour alone says nothing.
const BAR: Record<string, string> = {
  online: "border-l-status-online",
  degraded: "border-l-status-warning",
  reconnecting: "border-l-status-warning",
  offline: "border-l-status-offline",
  unknown: "border-l-status-unknown",
};

interface TaskCardProps {
  envId: string;
  task: PlacementTask;
}

// TaskCard is one task in a node column: a 40px link to its service. Line 1 is the service and
// its slot, line 2 the state's dot and word and, for a task Docker could not place, why. The
// names and the error are Docker's text, drawn as text nodes and cut with the whole text in a
// tooltip.
export function TaskCard({ envId, task }: TaskCardProps) {
  const t = useT();
  const { tone, labelKey } = taskState(task.state);
  const state = t(labelKey);
  // A global service's tasks have no slot.
  const label =
    task.slot > 0
      ? t("placement.card.open", { service: task.service, slot: task.slot, state })
      : t("placement.card.openGlobal", { service: task.service, state });
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Link
          to="/env/$envId/services/$serviceId"
          params={{ envId, serviceId: task.serviceId }}
          aria-label={label}
          data-slot="task-card"
          className={cn(
            "flex h-10 min-w-0 flex-col justify-center rounded-md border border-l-2 bg-background px-2 text-left outline-none",
            "hover:bg-accent focus-visible:ring-3 focus-visible:ring-ring/50 max-md:h-11",
            BAR[tone] ?? BAR.unknown,
          )}
        >
          <span className="flex min-w-0 items-baseline gap-2">
            <span className="min-w-0 truncate text-sm leading-4 font-semibold">{task.service}</span>
            {task.slot > 0 ? (
              <span className="shrink-0 font-mono text-xs leading-4 text-muted-foreground">
                {t("placement.card.slot", { slot: task.slot })}
              </span>
            ) : null}
          </span>
          <span className="flex min-w-0 items-center gap-2 text-xs leading-4">
            <StatusDot state={tone as HealthState} label={state} className="shrink-0" />
            {task.error ? (
              <span className="min-w-0 truncate text-status-offline-text">{task.error}</span>
            ) : null}
          </span>
        </Link>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 break-words">
        <span className="block font-semibold">
          {task.slot > 0 ? `${task.service}.${task.slot}` : task.service}
        </span>
        {task.error ? <span className="block">{task.error}</span> : null}
      </TooltipContent>
    </Tooltip>
  );
}
