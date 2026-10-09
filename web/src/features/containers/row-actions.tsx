// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Ellipsis } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  ActionRequestError,
  type ContainerActionResult,
  useContainerAction,
} from "@/api/containers";
import type { ContainerRow } from "@/api/resources";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The states a container can be started from, and stopped from (UI-SPEC
// eligibility). Any other state (removing, dead) offers neither.
const STARTABLE: ReadonlySet<string> = new Set(["exited", "created"]);
const STOPPABLE: ReadonlySet<string> = new Set(["running", "restarting", "paused"]);
const RESTARTABLE: ReadonlySet<string> = new Set(["running", "exited", "paused", "restarting"]);
const PAUSABLE: ReadonlySet<string> = new Set(["running"]);
const UNPAUSABLE: ReadonlySet<string> = new Set(["paused"]);

// The actions that need no confirmation, with the words of their toast.
type ImmediateAction = "start" | "restart" | "pause" | "unpause";

const DONE_TOAST: Record<ImmediateAction, string> = {
  start: "started",
  restart: "restarted",
  pause: "paused",
  unpause: "unpaused",
};

type Translate = (key: string, options?: Record<string, unknown>) => string;

// reasonOf is the sentence for a container that failed, from its code. A code
// is never shown: the SPA words every one of them (UI-SPEC "Shared refusals").
function reasonOf(t: Translate, row: ContainerRow, result: ContainerActionResult): string {
  switch (result.code) {
    case "own_container":
      return t("actions.refusal.own_container", { name: row.name });
    case "node_unreachable":
      return t("actions.refusal.node_unreachable", { node: row.node });
    case "not_found":
      return t("actions.refusal.not_found", { name: row.name });
    case "agent_policy_denied":
      return t("actions.refusal.agent_policy_denied");
    default:
      // The engine's own message says what it refused; without one, the server
      // could not say more than that something went wrong.
      return result.detail || t("actions.refusal.generic");
  }
}

// requestReasonOf is the sentence for a request that got no answer per
// container: the permission was withdrawn, or the server failed.
function requestReasonOf(t: Translate, err: unknown): string {
  return err instanceof ActionRequestError && err.status === 403
    ? t("actions.refusal.permission")
    : t("actions.refusal.generic");
}

interface StopDialogProps {
  envId: string;
  row: ContainerRow;
  open: boolean;
  onClose: () => void;
  // Called when the dialog has closed on its own account (a stop that worked),
  // so the menu button can take focus back.
  onClosed?: () => void;
  // The container's own stop timeout in seconds, when it is known.
  seconds?: number;
}

// StopDialog is "Stop {name}?" for one container (D-07, UI-SPEC "Container
// action dialogs"). Focus starts on Cancel. A refusal or failure stays inside the
// dialog as an alert and never raises a toast over it; if the dialog was closed
// while the request was out, the answer arrives as a toast instead (UI-SPEC
// pending state).
export function StopDialog({ envId, row, open, onClose, onClosed, seconds }: StopDialogProps) {
  const t = useT();
  const mutation = useContainerAction(envId);
  const [error, setError] = useState<string | null>(null);
  // Whether the dialog is still on screen when the answer arrives.
  const shown = useRef(open);
  shown.current = open;
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = mutation.isPending;

  async function stop() {
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      const results = await mutation.mutateAsync({
        action: "stop",
        targets: [{ nodeId: row.nodeId, id: row.id }],
      });
      const result = results[0];
      if (result?.result === "done") {
        toastSuccess(t("actions.toast.stopped", { name: row.name }));
        onClose();
        onClosed?.();
        return;
      }
      const reason = result ? reasonOf(t, row, result) : t("actions.refusal.generic");
      if (shown.current) setError(reason);
      else
        toastError(
          t("actions.toast.failed", { verb: t("actions.verb.stop"), name: row.name, reason }),
        );
    } catch (err) {
      const reason = requestReasonOf(t, err);
      if (shown.current) setError(reason);
      else
        toastError(
          t("actions.toast.failed", { verb: t("actions.verb.stop"), name: row.name, reason }),
        );
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (next) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent
        onCloseAutoFocus={(event) => {
          // Focus goes back to the row's menu button, not to the page.
          event.preventDefault();
          onClosed?.();
        }}
      >
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("actions.stop.title", { name: row.name })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {seconds === undefined
              ? t("actions.stop.bodyOwn")
              : t("actions.stop.body", { seconds })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel className={TOUCH_HEIGHT}>{t("actions.stop.cancel")}</AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => {
              if (!pending) void stop();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("actions.stop.pending")}
              </>
            ) : (
              t("actions.stop.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

interface ContainerRowActionsProps {
  envId: string;
  row: ContainerRow;
  // Called with true while the menu or a dialog of this row is open and false
  // when it is not, so the list can hold still while a person is choosing.
  onOpenChange?: (open: boolean) => void;
  // The row is gone from the list but still shown: the button is off.
  disabled?: boolean;
}

// ContainerRowActions is the row menu of the containers list: the actions that
// fit the container's state (D-06, D-07). Start, Restart, Pause and Unpause run
// at once; Stop asks first. A caller without containers.control sees the item disabled,
// with the reason (D-19); the server decides either way.
export function ContainerRowActions({
  envId,
  row,
  onOpenChange,
  disabled,
}: ContainerRowActionsProps) {
  const t = useT();
  const needs = useNeeds("containers.control", envId);
  const mutation = useContainerAction(envId);
  const [stopOpen, setStopOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const report = useRef(onOpenChange);
  report.current = onOpenChange;
  const open = menuOpen || stopOpen;
  useEffect(() => {
    report.current?.(open);
  }, [open]);
  // A row that goes away with its menu open must not leave the list frozen.
  useEffect(
    () => () => {
      report.current?.(false);
    },
    [],
  );
  // Set when an item was chosen, so the menu does not hand focus back to its
  // button before the dialog has opened.
  const chose = useRef(false);

  const canStart = STARTABLE.has(row.state);
  const canStop = STOPPABLE.has(row.state);
  const canRestart = RESTARTABLE.has(row.state);
  const canPause = PAUSABLE.has(row.state);
  const canUnpause = UNPAUSABLE.has(row.state);
  if (!canStart && !canStop && !canRestart && !canPause && !canUnpause) return null;

  // Unavailable: the reason when the caller's role is short or the host is
  // read-only, and just unavailable while the role is not yet known.
  const unavailable = !needs.allowed || row.readOnly;
  const reason = needs.reason ?? (row.readOnly ? t("actions.readOnlyHost") : undefined);
  const busy = mutation.isPending;

  // Start, restart, pause and unpause run at once: each is reversible, and
  // none loses data, so none asks first (D-06, D-07).
  async function run(action: ImmediateAction) {
    const verb = t(`actions.verb.${action}`);
    try {
      const results = await mutation.mutateAsync({
        action,
        targets: [{ nodeId: row.nodeId, id: row.id }],
      });
      const result = results[0];
      if (result?.result === "done") {
        toastSuccess(t(`actions.toast.${DONE_TOAST[action]}`, { name: row.name }));
        return;
      }
      const reason = result ? reasonOf(t, row, result) : t("actions.refusal.generic");
      toastError(t("actions.toast.failed", { verb, name: row.name, reason }));
    } catch (err) {
      toastError(
        t("actions.toast.failed", { verb, name: row.name, reason: requestReasonOf(t, err) }),
      );
    }
  }

  const item = (label: string, onSelect: () => void) => (
    <DropdownMenuItem
      className="max-md:min-h-11"
      disabled={unavailable && !reason}
      onSelect={() => {
        if (busy) return;
        chose.current = true;
        onSelect();
      }}
    >
      {label}
    </DropdownMenuItem>
  );

  return (
    <>
      <DropdownMenu open={menuOpen} onOpenChange={setMenuOpen}>
        <DropdownMenuTrigger asChild>
          <Button
            ref={button}
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            disabled={disabled}
            aria-label={t("actions.menu.label", { name: row.name })}
          >
            {busy ? <Spinner aria-hidden="true" /> : <Ellipsis aria-hidden="true" />}
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          onCloseAutoFocus={(event) => {
            if (!chose.current) return;
            chose.current = false;
            event.preventDefault();
          }}
        >
          {canStart ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.start"), () => void run("start"))}
            </NeedsTooltip>
          ) : null}
          {canStop ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.stop"), () => setStopOpen(true))}
            </NeedsTooltip>
          ) : null}
          {canRestart ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.restart"), () => void run("restart"))}
            </NeedsTooltip>
          ) : null}
          {canPause ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.pause"), () => void run("pause"))}
            </NeedsTooltip>
          ) : null}
          {canUnpause ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.unpause"), () => void run("unpause"))}
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      <StopDialog
        envId={envId}
        row={row}
        open={stopOpen}
        onClose={() => setStopOpen(false)}
        onClosed={() => button.current?.focus()}
      />
    </>
  );
}
