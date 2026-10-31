// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  addressSpaceKey,
  deleteOrphanNetwork,
  type OrphanDelete,
  OrphanRequestError,
  type OrphanRow,
} from "@/api/address-space";
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
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What one delete came to. "kept" is a safe answer, never an error: Coaming checked again just
// before and left the network alone. A refusal is a failure with the words that explain it.
export type OrphanResult =
  | { kind: "deleted" }
  | { kind: "kept"; reason: string }
  | { kind: "failed"; reason: string; gone?: boolean };

// keptReason is why Coaming kept a network, in the UI's words. node is who did not answer.
export function keptReason(
  t: Translate,
  name: string,
  reason: OrphanDelete["reason"],
  node: string,
): string {
  switch (reason) {
    case "in_use_again":
      return t("addressSpace.orphans.kept.inUseAgain", { name });
    case "node_missing":
      return t("addressSpace.orphans.kept.nodeMissing", { name, node });
    default:
      return t("addressSpace.orphans.kept.notOrphaned", { name });
  }
}

// refusalReason is the sentence for a delete the server refused or never answered. A code is
// never shown: the SPA words every one of them.
export function refusalReason(t: Translate, name: string, err: OrphanRequestError): string {
  switch (err.type) {
    case "inspect_too_large":
      return t("addressSpace.orphans.refused.inspectTooLarge", { name });
    case "network_builtin":
      return t("addressSpace.orphans.refused.builtin", { name });
    case "network_not_found":
      return t("addressSpace.orphans.refused.gone", { name });
    default:
      return err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
  }
}

// deleteOne sends one delete, once, and words the answer. node is who to name when the server
// could not be sure because a node did not answer.
export async function deleteOne(
  t: Translate,
  envId: string,
  row: OrphanRow,
  node: string,
): Promise<OrphanResult> {
  try {
    const answer = await deleteOrphanNetwork(
      envId,
      row.networkId,
      row.scope === "local" ? row.nodeId : undefined,
    );
    if (answer.outcome === "deleted") return { kind: "deleted" };
    return { kind: "kept", reason: keptReason(t, row.name, answer.reason, node) };
  } catch (e) {
    const err = e instanceof OrphanRequestError ? e : new OrphanRequestError(0);
    // The engine's own in-use refusal is a kept answer, however it reached us.
    if (err.type === "network_in_use") {
      return { kind: "kept", reason: keptReason(t, row.name, "in_use_again", node) };
    }
    return {
      kind: "failed",
      reason: refusalReason(t, row.name, err),
      gone: err.type === "network_not_found",
    };
  }
}

interface DeleteOrphansDialogProps {
  envId: string;
  // The network to delete, or null while the dialog is closed.
  row: OrphanRow | null;
  // Who to name when a node did not answer.
  notReporting: readonly string[];
  onClose: () => void;
}

// DeleteOrphansDialog is "Delete “{name}”?": the server looks again just before it deletes, saves
// the network's details in the audit log first and keeps the network when something started
// using it. Focus starts on Cancel, the dialog cannot be dismissed while the request is out, and
// the answer is a toast: a kept network is information, not an error.
export function DeleteOrphansDialog({
  envId,
  row,
  notReporting,
  onClose,
}: DeleteOrphansDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = row?.name ?? "";

  async function remove() {
    if (!row || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    try {
      const node = notReporting.length > 0 ? notReporting.join(", ") : row.node;
      const result = await deleteOne(t, envId, row, node);
      if (result.kind === "deleted") {
        toastSuccess(t("addressSpace.orphans.delete.done", { name }));
      } else if (result.kind === "kept") {
        toastInfo(result.reason);
      } else if (result.gone) {
        toastInfo(result.reason);
      } else {
        toastError(result.reason);
      }
      void queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) });
      onClose();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    onClose();
  }

  return (
    <AlertDialog open={row !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("addressSpace.orphans.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {t("addressSpace.orphans.delete.body", { time: formatAbsolute(row?.idleSince) })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("addressSpace.orphans.delete.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => {
              if (!pending) void remove();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("addressSpace.orphans.delete.pending")}
              </>
            ) : (
              t("addressSpace.orphans.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
