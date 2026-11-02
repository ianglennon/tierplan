// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HealthState } from "@/api/environments";
import type { StackState } from "@/api/stacks";
import { StatusDot } from "@/components/status-dot";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// The status colour of each state (UI-SPEC "Color"). An operation under way is the pulsing warning
// dot, which StatusDot turns off under reduced motion; Degraded is the warning colour without the
// pulse; a failure and a stack that is not running are offline; a stack that is stopped, not
// deployed or not known is neutral.
const TONE: Record<StackState, HealthState> = {
  removing: "reconnecting",
  deploying: "reconnecting",
  rolling_back: "reconnecting",
  starting: "reconnecting",
  stopping: "reconnecting",
  stopped: "unknown",
  deploy_failed: "offline",
  not_running: "offline",
  degraded: "degraded",
  running: "online",
  not_deployed: "unknown",
  unknown: "unknown",
};

interface StackStateCellProps {
  state: StackState;
  // The services that run and the services there are: what Degraded explains in a Tooltip.
  running?: number;
  total?: number;
  // The service the last deploy stopped at: what Deploy failed explains in a Tooltip.
  failedService?: string;
}

// StackStateCell is a stack's state as a dot and a word, never colour alone. Degraded says in a
// Tooltip how many services run, Deploy failed where the last deploy stopped, and Unknown why it is
// not known. The words are text nodes, so nothing a stack or a service is named can add markup.
export function StackStateCell({ state, running, total, failedService }: StackStateCellProps) {
  const t = useT();
  const dot = <StatusDot state={TONE[state]} label={t(`stackOverview.state.${state}`)} />;
  let tip: string | undefined;
  if (state === "degraded" && running !== undefined && total !== undefined) {
    tip = t("stackOverview.state.degradedTooltip", { running, count: total });
  } else if (state === "deploy_failed") {
    tip = failedService
      ? t("stackOverview.state.deployFailedTooltip", { service: failedService })
      : t("stackOverview.state.deployFailedTooltipNoService");
  } else if (state === "unknown") {
    tip = t("stackOverview.state.unknownTooltip");
  }
  if (!tip) return dot;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex items-center whitespace-nowrap">{dot}</span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
    </Tooltip>
  );
}
