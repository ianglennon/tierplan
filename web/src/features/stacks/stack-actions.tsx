// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import {
  type DeployRequestError,
  type StackDetail,
  type StackState,
  useStartStack,
} from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { followStackOperation } from "@/features/stacks/follow-operation";
import { StopStackDialog } from "@/features/stacks/stop-stack-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// The states in which a stop has something to act on: a stack that is running, partly running, not running
// because something stopped it, or one the engine could not be read for. Every other state is an operation
// under way, a stack never deployed, or one already stopped.
const STOPPABLE: readonly StackState[] = [
  "running",
  "degraded",
  "not_running",
  "deploy_failed",
  "unknown",
];

export const canStop = (state: StackState) => STOPPABLE.includes(state);
export const canStart = (state: StackState) => state === "stopped";

type LifecycleStack = Pick<StackDetail, "id" | "envId" | "name" | "target" | "state">;

// refusalOf words a stop or a start the server refused or could not reach.
export function refusalKey(error: DeployRequestError): string {
  if (error.status === 0) return "stackStop.refusal.unreachable";
  switch (error.type) {
    case "nothing_running":
    case "not_stopped":
    case "operation_locked":
    case "privileged_compose":
      return `stackStop.refusal.${error.type}`;
    default:
      return "stackStop.refusal.other";
  }
}

interface StackActionsProps {
  stack: LifecycleStack;
}

// StackActions are the Stop stack… and Start stack buttons of a stack's header (UI-SPEC "Stack page"): both
// outline, Stop only while there is something to stop and Start only while the stack is stopped. A user who
// may not deploy sees the one that applies off, with the role it takes; a user whose permissions are not
// known yet sees nothing rather than a button that may turn off.
export function StackActions({ stack }: StackActionsProps) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", stack.envId);
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const start = useStartStack(stack.envId, stack.id);
  const [stopping, setStopping] = useState(false);

  async function startIt() {
    if (start.isPending) return;
    try {
      const started = await start.mutateAsync();
      if (started.recreates) {
        // A start that creates services again is a deploy to follow in the console.
        void navigate({
          to: "/env/$envId/stacks/$stackId/deploys/$jobId",
          params: { envId: stack.envId, stackId: stack.id, jobId: started.jobId },
        });
        return;
      }
      followStackOperation(queryClient, {
        envId: stack.envId,
        stackId: stack.id,
        jobId: started.jobId,
        onDone: () => toastSuccess(t("stackStop.toast.started", { name: stack.name })),
        onFailed: (reason) =>
          toastError(
            reason
              ? t("stackStop.toast.startFailed", { name: stack.name, reason })
              : t("stackStop.toast.startFailedNoReason", { name: stack.name }),
          ),
      });
    } catch (e) {
      const reason = t(refusalKey(e as DeployRequestError));
      toastError(t("stackStop.toast.startFailed", { name: stack.name, reason }));
    }
  }

  if (canStop(stack.state)) {
    if (!needs.allowed && !needs.reason) return null;
    return (
      <>
        <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={needs.allowed ? () => setStopping(true) : undefined}
          >
            {t("stackStop.actions.stop")}
          </Button>
        </NeedsTooltip>
        {needs.allowed ? (
          <StopStackDialog stack={stack} open={stopping} onOpenChange={setStopping} />
        ) : null}
      </>
    );
  }
  if (canStart(stack.state)) {
    if (!needs.allowed) {
      if (!needs.reason) return null;
      return (
        <NeedsTooltip reason={needs.reason}>
          <Button type="button" variant="outline" className="max-md:h-11">
            {t("stackStop.actions.start")}
          </Button>
        </NeedsTooltip>
      );
    }
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            aria-disabled={start.isPending}
            onClick={() => void startIt()}
          >
            {start.isPending ? t("stackStop.actions.starting") : t("stackStop.actions.start")}
          </Button>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm break-words">
          {t("stackStop.actions.startTooltip")}
        </TooltipContent>
      </Tooltip>
    );
  }
  return null;
}
