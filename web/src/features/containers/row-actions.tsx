// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
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
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import {
  ActionDialog,
  type DialogVerb,
  isLostAnswer,
  reasonOf,
  requestReasonOf,
} from "@/features/containers/action-dialog";
import type { BulkReport } from "@/features/containers/bulk-result-dialog";
import { eligibleFor, ownKind } from "@/features/containers/eligibility";
import { pendingFingerprint, pendingKey, usePendingApi } from "@/features/containers/pending";
import { useEnvName } from "@/features/resources/use-env-name";
import { MuteMenuItem } from "@/features/restart-loop/mute-menu-item";
import { IgnoreUpdatesDialog } from "@/features/updates/ignore-updates-dialog";
import { UpdateConfirmDialog } from "@/features/updates/update-confirm-dialog";
import { UpdateMenuItems } from "@/features/updates/update-menu-items";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";

// The actions that need no confirmation, with the words of their toast.
export type ImmediateAction = "start" | "restart" | "pause" | "unpause";

const DONE_TOAST: Record<ImmediateAction, string> = {
  start: "started",
  restart: "restarted",
  pause: "paused",
  unpause: "unpaused",
};

// useImmediateAction runs the actions that need no confirmation on one container:
// Start, Restart, Pause and Unpause. Each is reversible, and none loses data, so
// none asks first (D-06, D-07). The row is marked as waiting until the change is
// seen, a refusal toasts its reason, and busy is true while a request is out. The
// list's row menu and the container page's header share it.
export function useImmediateAction(envId: string, row: ContainerRow) {
  const t = useT();
  const mutation = useContainerAction(envId);
  const pend = usePendingApi();

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
      if (isLostAnswer(err)) {
        toastInfo(t("actions.toast.lost", { verb, name: row.name }));
        return;
      }
      toastError(
        t("actions.toast.failed", { verb, name: row.name, reason: requestReasonOf(t, err) }),
      );
    }
  }

  return { run, busy: mutation.isPending };
}

interface ContainerRowActionsProps {
  envId: string;
  row: ContainerRow;
  // Called with true while the menu or a dialog of this row is open and false
  // when it is not, so the list can hold still while a person is choosing.
  onOpenChange?: (open: boolean) => void;
  // The row is gone from the list but still shown: the button is off.
  disabled?: boolean;
  // Called with the report when the person asks for the details of a bulk; a
  // single container never has one, so this is for the dialog's own sake.
  onDetails?: (report: BulkReport) => void;
}

// ContainerRowActions is the row menu of the containers list: the actions that
// fit the container's state (D-06, D-07). Start, Restart, Pause and Unpause run
// at once; Stop, Kill and Remove ask first, and so does any action on one of
// TierPlan's own containers (D-11). A caller without the permission sees the item
// disabled, with the reason (D-19); the server decides either way.
export function ContainerRowActions({
  envId,
  row,
  onOpenChange,
  disabled,
  onDetails,
}: ContainerRowActionsProps) {
  const t = useT();
  const needs = useNeeds("containers.control", envId);
  const removeNeeds = useNeeds("containers.remove", envId);
  const { run, busy } = useImmediateAction(envId, row);
  // The dialog of the action chosen. The verb stays after it closes so the
  // dialog does not change its words while it fades.
  const [dialogVerb, setDialogVerb] = useState<DialogVerb>("stop");
  const [dialogOpen, setDialogOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  // Ignore updates… opens a dialog of its own, which also holds the list still.
  const [ignoring, setIgnoring] = useState(false);
  // Update… opens the compact confirm of an image update (D-05), which holds the list still as well.
  const [updating, setUpdating] = useState(false);
  const envName = useEnvName(envId);
  const button = useRef<HTMLButtonElement>(null);
  const report = useRef(onOpenChange);
  report.current = onOpenChange;
  const open = menuOpen || dialogOpen || ignoring || updating;
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

  const canStart = eligibleFor("start", row);
  const canStop = eligibleFor("stop", row);
  const canRestart = eligibleFor("restart", row);
  const canPause = eligibleFor("pause", row);
  const canUnpause = eligibleFor("unpause", row);
  const canKill = eligibleFor("kill", row);
  const canRemove = eligibleFor("remove", row);
  if (!canStart && !canStop && !canRestart && !canPause && !canUnpause && !canKill && !canRemove)
    return null;
  // One of TierPlan's own containers asks for its name before it is stopped,
  // killed, restarted, paused or removed; start and unpause need no more care.
  const own = ownKind(row) !== "";

  // Unavailable: the reason when the caller's role is short or the host is
  // read-only, and just unavailable while the role is not yet known.
  const unavailable = !needs.allowed || row.readOnly;
  const reason = needs.reason ?? (row.readOnly ? t("actions.readOnlyHost") : undefined);
  const removeUnavailable = !removeNeeds.allowed || row.readOnly;
  const removeReason = removeNeeds.reason ?? (row.readOnly ? t("actions.readOnlyHost") : undefined);

  function ask(verb: DialogVerb) {
    setDialogVerb(verb);
    setDialogOpen(true);
  }

  const item = (
    label: string,
    onSelect: () => void,
    opts: { remove?: boolean; destructive?: boolean } = {},
  ) => (
    <DropdownMenuItem
      className="max-md:min-h-11"
      variant={opts.destructive ? "destructive" : "default"}
      disabled={opts.remove ? removeUnavailable && !removeReason : unavailable && !reason}
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
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link
              to="/env/$envId/containers/$containerId"
              params={{ envId, containerId: row.id }}
              search={{ node: row.nodeId }}
            >
              {t("actions.menu.open")}
            </Link>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {canStart ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.start"), () => void run("start"))}
            </NeedsTooltip>
          ) : null}
          {canStop ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.stop"), () => ask("stop"))}
            </NeedsTooltip>
          ) : null}
          {canRestart ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.restart"), () => (own ? ask("restart") : void run("restart")))}
            </NeedsTooltip>
          ) : null}
          {canPause ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.pause"), () => (own ? ask("pause") : void run("pause")))}
            </NeedsTooltip>
          ) : null}
          {canUnpause ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.unpause"), () => void run("unpause"))}
            </NeedsTooltip>
          ) : null}
          {row.restartLoop ? (
            <MuteMenuItem
              kind="container"
              envId={envId}
              nodeId={row.nodeId}
              containerId={row.id}
              name={row.name}
              muted={row.restartLoop.state === "muted"}
              needs={needs}
            />
          ) : null}
          <UpdateMenuItems
            envId={envId}
            workload={{ kind: "container", name: row.name }}
            update={row.update}
            onChosen={() => {
              chose.current = true;
            }}
            onIgnore={() => setIgnoring(true)}
            onUpdate={own ? undefined : () => setUpdating(true)}
            project={row.project}
            service={row.service}
          />
          {(canKill || canRemove) &&
          (canStart || canStop || canRestart || canPause || canUnpause) ? (
            <DropdownMenuSeparator />
          ) : null}
          {canKill ? (
            <NeedsTooltip reason={reason}>
              {item(t("actions.menu.kill"), () => ask("kill"), { destructive: true })}
            </NeedsTooltip>
          ) : null}
          {canRemove ? (
            <NeedsTooltip reason={removeReason}>
              {item(t("actions.menu.remove"), () => ask("remove"), {
                remove: true,
                destructive: true,
              })}
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      <ActionDialog
        envId={envId}
        verb={dialogVerb}
        rows={[row]}
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onClosed={() => button.current?.focus()}
        onDetails={onDetails}
      />
      {updating ? (
        <UpdateConfirmDialog
          envId={envId}
          envName={envName}
          workload={{ kind: "container", name: row.name }}
          managedStack={row.tierplanStackId ? row.project || row.stack : undefined}
          onClose={() => setUpdating(false)}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
      {ignoring && row.update ? (
        <IgnoreUpdatesDialog
          envId={envId}
          workload={{ kind: "container", name: row.name }}
          update={row.update}
          open
          onOpenChange={(isOpen) => {
            if (!isOpen) setIgnoring(false);
          }}
          onClosed={() => button.current?.focus()}
        />
      ) : null}
    </>
  );
}
