// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { useState } from "react";

import {
  type DeployRequestError,
  type StackDetail,
  type StackState,
  useStartStack,
} from "@/api/stacks";
import { copyText } from "@/components/copyable-code";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { followStackOperation, refusalKey } from "@/features/stacks/follow-operation";
import { RemoveStackDialog } from "@/features/stacks/remove-stack-dialog";
import { StopStackDialog } from "@/features/stacks/stop-stack-dialog";
import { StoppedAlert } from "@/features/stacks/stopped-alert";
import { UpdateImagesDialog } from "@/features/stacks/update-images-dialog";
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

// The states in which an operation is under way on the stack: Remove… waits until it has ended.
const OPERATING: readonly StackState[] = [
  "deploying",
  "rolling_back",
  "starting",
  "stopping",
  "removing",
];

export const canStop = (state: StackState) => STOPPABLE.includes(state);
export const isOperating = (state: StackState) => OPERATING.includes(state);
export const canStart = (state: StackState) => state === "stopped";
// Images can be updated where something runs to update: the states a stop acts on.
export const canUpdateImages = canStop;

type LifecycleStack = Pick<StackDetail, "id" | "envId" | "name" | "target" | "state">;

interface StackActionsProps {
  stack: LifecycleStack;
}

// useStackStart is the Start stack action of a stack: it asks the server to start it and follows what comes
// of it. A start that creates services again is a deploy to follow, so it opens the console; one that only
// scales opens nothing, and the answer is a toast when the job ends. A refusal is a toast, since a start has
// no dialog to keep it in. It is shared by the header, the row menu and the stopped alert.
export function useStackStart(stack: Pick<StackDetail, "id" | "envId" | "name">) {
  const t = useT();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const mutation = useStartStack(stack.envId, stack.id);

  async function start() {
    if (mutation.isPending) return;
    try {
      const started = await mutation.mutateAsync();
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

  return { start, pending: mutation.isPending };
}

// StoppedAlertWithStart is the stopped alert of a stack's page with its Start stack wired for a user who may
// deploy. Anyone else gets the alert without the button.
export function StoppedAlertWithStart({ stack }: { stack: StackDetail }) {
  const needs = useNeeds("stacks.deploy", stack.envId);
  const { start } = useStackStart(stack);
  return <StoppedAlert stack={stack} onStart={needs.allowed ? () => void start() : undefined} />;
}

// StackActions are the Update images…, Stop stack… and Start stack buttons of a stack's header (UI-SPEC "Stack
// page"): all outline, Stop only while there is something to stop and Start only while the stack is stopped. A
// user who may not deploy sees the one that applies off, with the role it takes; a user whose permissions are
// not known yet sees nothing rather than a button that may turn off.
export function StackActions({ stack }: StackActionsProps) {
  return (
    <>
      <UpdateImagesAction stack={stack} />
      <StopStartAction stack={stack} />
      <StackMoreMenu stack={stack} />
    </>
  );
}

// StackMoreMenu is the header's More menu (UI-SPEC "Stack page"): Copy stack name, a separator and Remove… in
// the destructive text colour. Remove… is for an Env Admin: below that it is off with the role it takes, and
// for a user whose permissions are not known yet it is not there. It is off, too, while an operation is under
// way on the stack. The dialog sits beside the menu and not in it, so it outlives the menu closing.
export function StackMoreMenu({ stack }: StackActionsProps) {
  const t = useT();
  const needs = useNeeds("stacks.delete", stack.envId);
  const [removing, setRemoving] = useState(false);
  const operating = isOperating(stack.state);
  async function copy() {
    if (await copyText(stack.name))
      toastSuccess(t("stackRemove.actions.copied", { name: stack.name }));
  }
  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            aria-label={t("stackRemove.actions.more", { name: stack.name })}
          >
            {t("stackRemove.actions.moreLabel")}
            <ChevronDown aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem className="max-md:min-h-11" onSelect={() => void copy()}>
            {t("stackRemove.actions.copyName")}
          </DropdownMenuItem>
          {needs.allowed || needs.reason ? <DropdownMenuSeparator /> : null}
          {needs.allowed ? (
            <DropdownMenuItem
              variant="destructive"
              className="max-md:min-h-11"
              disabled={operating}
              onSelect={() => setRemoving(true)}
            >
              {t("stackRemove.actions.remove")}
            </DropdownMenuItem>
          ) : needs.reason ? (
            <NeedsTooltip reason={needs.reason}>
              <DropdownMenuItem variant="destructive" className="max-md:min-h-11">
                {t("stackRemove.actions.remove")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {needs.allowed ? (
        <RemoveStackDialog stack={stack} open={removing} onOpenChange={setRemoving} />
      ) : null}
    </>
  );
}

// UpdateImagesAction is the header's Update images… (D-16): it opens the dialog that leads into the review of
// the chosen services. It is off, with the reason, for a user who may not deploy and for a stack with nothing
// running to update, and it is not there while an operation is under way on the stack.
function UpdateImagesAction({ stack }: StackActionsProps) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", stack.envId);
  const [open, setOpen] = useState(false);
  if (isOperating(stack.state)) return null;
  if (!needs.allowed && !needs.reason) return null;
  const nothing = !canUpdateImages(stack.state);
  const reason = needs.allowed
    ? nothing
      ? t("stackUpdate.actions.nothing")
      : undefined
    : needs.reason;
  return (
    <>
      <NeedsTooltip reason={reason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          onClick={() => setOpen(true)}
        >
          {t("stackUpdate.actions.update")}
        </Button>
      </NeedsTooltip>
      {needs.allowed && !nothing ? (
        <UpdateImagesDialog stack={stack} open={open} onOpenChange={setOpen} />
      ) : null}
    </>
  );
}

// StopStartAction is the Stop stack… or Start stack button the stack's state calls for, or nothing.
function StopStartAction({ stack }: StackActionsProps) {
  const t = useT();
  const needs = useNeeds("stacks.deploy", stack.envId);
  const { start, pending } = useStackStart(stack);
  const [stopping, setStopping] = useState(false);

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
            aria-disabled={pending}
            onClick={() => void start()}
          >
            {pending ? t("stackStop.actions.starting") : t("stackStop.actions.start")}
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
