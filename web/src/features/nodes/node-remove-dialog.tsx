// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useEffect, useRef, useState } from "react";

import { NodeActionError, useRemoveNode } from "@/api/nodes";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
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
import { Spinner } from "@/components/ui/spinner";
import { nodeActionWording } from "@/features/nodes/node-action-dialog";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = ReturnType<typeof useT>;

export interface NodeRemoveDialogProps {
  envId: string;
  // The node to remove, or null while the dialog is closed.
  node: { id: string; hostname: string } | null;
  onClose: () => void;
  // Called once the node is gone (or was already), after the dialog asked to close.
  onRemoved?: (removed: { nodeId: string }) => void;
}

// removeWording words a removal that did not happen, from the problem type the server named. The
// two refusals about the node's state have their own words; Docker's own are passed on where the
// server passes them; the rest are those of any change to a node.
function removeWording(t: Translate, err: unknown, name: string, environment: string): string {
  const e = err instanceof NodeActionError ? err : new NodeActionError(0);
  switch (e.type) {
    case "node_not_down":
    case "node_is_manager":
      return t(`nodes.remove.refusal.${e.type}`, { name });
    case "node_remove_failed":
      if (e.detail) return t("nodes.action.refusal.rejected", { message: e.detail });
      return t("actions.refusal.generic");
  }
  return nodeActionWording(t, e, { name, environment });
}

// NodeRemoveDialog is "Remove {node}?" for a node that is down (D-22, UI-SPEC "Nodes"): an alert
// dialog that says Swarm forgets the node and TierPlan marks its agent as gone, and asks for the
// node's name before its destructive confirm turns on. Focus starts on Cancel and the dialog
// cannot be dismissed while the request is out. A refusal stays inside it in words; a node that is
// already gone is said so in a toast and the dialog closes, since the list no longer shows it.
// The removal is sent once, with the name, and never retried.
export function NodeRemoveDialog({ envId, node, onClose, onRemoved }: NodeRemoveDialogProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const mutation = useRemoveNode(envId);
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = node?.hostname ?? "";
  const id = node?.id;
  const ready = node !== null && matchesName(typed, name);

  // A dialog opened again, for another node, starts clean.
  useEffect(() => {
    void id;
    setTyped("");
    setError(null);
  }, [id]);

  async function submit() {
    if (!node || !ready || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({ nodeId: node.id, confirm: typed.trim() });
      toastSuccess(t("nodes.remove.done", { name }));
      onClose();
      onRemoved?.({ nodeId: node.id });
    } catch (e) {
      if (e instanceof NodeActionError && e.type === "node_not_found") {
        toastInfo(t("nodes.remove.gone", { name }));
        onClose();
        onRemoved?.({ nodeId: node.id });
      } else {
        setError(removeWording(t, e, name, environment));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || mutation.isPending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={node !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps anywhere rather than running off the dialog. */}
          <AlertDialogTitle className="break-all">
            {t("nodes.remove.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("nodes.remove.body", { name })}</AlertDialogDescription>
        </AlertDialogHeader>
        <TypeToConfirm
          name={name}
          value={typed}
          onChange={setTyped}
          onConfirm={() => void submit()}
          disabled={mutation.isPending}
        />
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("nodes.remove.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={mutation.isPending || !ready}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void submit()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("nodes.remove.pending")}
              </>
            ) : (
              t("nodes.remove.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
