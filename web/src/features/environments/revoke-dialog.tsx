// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  AgentActionError,
  type AgentItem,
  refreshAfterAgentChange,
  revokeAgent,
  unblockAgent,
} from "@/api/agents";
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
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastSuccess, toastSuccessWithAction } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// What an inline message says, as a key under envsettings. A failure that has
// its own sentence is picked by the problem type; the rest is general.
type FailureKey =
  | "revoke.alreadyRevoked"
  | "revoke.failed"
  | "unblock.notBlocked"
  | "unblock.failed";

interface RevokeDialogProps {
  envId: string;
  // The node being revoked. Null while the dialog is closed.
  agent: AgentItem | null;
  onClose: () => void;
  // Offered in the toast after a revoke: rotating the join token is what keeps
  // a compromised host out (UI-SPEC "After revoke toast").
  onRotateToken?: () => void;
  // Where focus goes when the dialog has closed. The row's menu is gone by
  // then, so the table decides.
  onCloseAutoFocus?: (event: Event) => void;
}

// RevokeDialog is "Revoke {node}?" (D-08): an alert-dialog with focus on Cancel
// and a destructive confirm. A failure keeps it open with an inline alert and
// no toast over it, and a node somebody revoked first says so (UI-SPEC
// Concurrency). Radix closes an alert-dialog on any action click, so the
// confirm is a plain button and the dialog closes when the server has answered.
export function RevokeDialog({
  envId,
  agent,
  onClose,
  onRotateToken,
  onCloseAutoFocus,
}: RevokeDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [failure, setFailure] = useState<FailureKey | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  // The dialog keeps the node it was opened for until it has closed, so the
  // title does not lose the name while it fades out.
  const shown = useRef<AgentItem | null>(null);
  if (agent) shown.current = agent;
  const node = shown.current?.node ?? "";

  const revoke = useMutation({
    mutationFn: (target: AgentItem) => revokeAgent(envId, target.id),
    onSuccess: async (_data, target) => {
      await refreshAfterAgentChange(queryClient, envId);
      onClose();
      const done = t("envsettings.revoke.done", { node: target.node });
      if (onRotateToken) {
        toastSuccessWithAction(done, t("envsettings.revoke.rotate"), onRotateToken);
      } else {
        toastSuccess(done);
      }
    },
    onError: (error) => {
      // 409 agent_already_revoked: another admin, or another tab, got there first.
      if (error instanceof AgentActionError && error.alreadyRevoked) {
        setFailure("revoke.alreadyRevoked");
        // The row is stale: bring it up to date behind the message.
        void refreshAfterAgentChange(queryClient, envId);
        return;
      }
      setFailure("revoke.failed");
    },
    onSettled: () => {
      inFlight.current = false;
    },
  });

  function close(open: boolean) {
    if (open || revoke.isPending) return;
    setFailure(null);
    revoke.reset();
    onClose();
  }

  return (
    <AlertDialog open={agent !== null} onOpenChange={close}>
      <AlertDialogContent onCloseAutoFocus={onCloseAutoFocus}>
        <AlertDialogHeader>
          <AlertDialogTitle>{t("envsettings.revoke.title", { node })}</AlertDialogTitle>
          <AlertDialogDescription>{t("envsettings.revoke.body", { node })}</AlertDialogDescription>
        </AlertDialogHeader>
        {failure ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t(`envsettings.${failure}`)}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={revoke.isPending} className={TOUCH_HEIGHT}>
            {t("envsettings.revoke.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            variant="destructive"
            disabled={revoke.isPending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
            onClick={() => {
              if (!agent || inFlight.current) return;
              inFlight.current = true;
              setFailure(null);
              revoke.mutate(agent);
            }}
          >
            {revoke.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("envsettings.revoke.pending")}
              </>
            ) : (
              t("envsettings.revoke.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

interface UnblockDialogProps {
  envId: string;
  agent: AgentItem | null;
  onClose: () => void;
  onCloseAutoFocus?: (event: Event) => void;
}

// UnblockDialog is "Unblock {node}?" (D-09): a plain dialog, since nothing is
// lost by it, with the primary confirm. The revoked agent is not revived: the
// host enrols again as a new agent, which the body says.
export function UnblockDialog({ envId, agent, onClose, onCloseAutoFocus }: UnblockDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [failure, setFailure] = useState<FailureKey | null>(null);
  const inFlight = useRef(false);
  const shown = useRef<AgentItem | null>(null);
  if (agent) shown.current = agent;
  const node = shown.current?.node ?? "";

  const unblock = useMutation({
    mutationFn: (target: AgentItem) => unblockAgent(envId, target.id),
    onSuccess: async (_data, target) => {
      await refreshAfterAgentChange(queryClient, envId);
      onClose();
      toastSuccess(t("envsettings.unblock.done", { node: target.node }));
    },
    onError: (error) => {
      if (error instanceof AgentActionError && error.notBlocked) {
        setFailure("unblock.notBlocked");
        void refreshAfterAgentChange(queryClient, envId);
        return;
      }
      setFailure("unblock.failed");
    },
    onSettled: () => {
      inFlight.current = false;
    },
  });

  function close(open: boolean) {
    if (open || unblock.isPending) return;
    setFailure(null);
    unblock.reset();
    onClose();
  }

  return (
    <Dialog open={agent !== null} onOpenChange={close}>
      <DialogContent showCloseButton={false} onCloseAutoFocus={onCloseAutoFocus}>
        <DialogHeader>
          <DialogTitle>{t("envsettings.unblock.title", { node })}</DialogTitle>
          <DialogDescription>{t("envsettings.unblock.body", { node })}</DialogDescription>
        </DialogHeader>
        {failure ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t(`envsettings.${failure}`)}</AlertDescription>
          </Alert>
        ) : null}
        <DialogFooter>
          <DialogClose asChild>
            <Button
              type="button"
              variant="outline"
              disabled={unblock.isPending}
              className={TOUCH_HEIGHT}
            >
              {t("envsettings.unblock.cancel")}
            </Button>
          </DialogClose>
          <Button
            type="button"
            disabled={unblock.isPending}
            className={TOUCH_HEIGHT}
            onClick={() => {
              if (!agent || inFlight.current) return;
              inFlight.current = true;
              setFailure(null);
              unblock.mutate(agent);
            }}
          >
            {unblock.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("envsettings.unblock.pending")}
              </>
            ) : (
              t("envsettings.unblock.confirm")
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
