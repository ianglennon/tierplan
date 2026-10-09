// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Ellipsis } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { useContainerAction } from "@/api/containers";
import type { ContainerRow } from "@/api/resources";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import { ActionDialog, reasonOf, requestReasonOf } from "@/features/containers/action-dialog";
import { pendingFingerprint, pendingKey, usePendingApi } from "@/features/containers/pending";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

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
  const pend = usePendingApi();
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
    const key = pendingKey(row);
    pend.mark([{ key, name: row.name, fingerprint: pendingFingerprint(row) }], action);
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
      pend.release([key]);
      const reason = result ? reasonOf(t, row, result) : t("actions.refusal.generic");
      toastError(t("actions.toast.failed", { verb, name: row.name, reason }));
    } catch (err) {
      pend.release([key]);
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
      <ActionDialog
        envId={envId}
        verb="stop"
        rows={[row]}
        open={stopOpen}
        onClose={() => setStopOpen(false)}
        onClosed={() => button.current?.focus()}
      />
    </>
  );
}
