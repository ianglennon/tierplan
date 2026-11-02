// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import { NetworkRequestError, useDeleteNetwork } from "@/api/networks";
import type { NetworkRow } from "@/api/resources";
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
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface DeleteNetworkDialogProps {
  envId: string;
  // The network to delete, or null while the dialog is closed.
  network: Pick<NetworkRow, "id" | "name" | "nodeId"> | null;
  onClose: () => void;
}

// DeleteNetworkDialog is "Delete {name}?" (D-41): a plain confirm, because only an
// unused network can reach it and the server checks that again. Focus starts on
// Cancel, the dialog cannot be dismissed while the request is out, and a refusal
// stays inside it with the way forward. A network that is already gone is said so
// in a toast and the dialog closes, since the refreshed list no longer shows it.
export function DeleteNetworkDialog({ envId, network, onClose }: DeleteNetworkDialogProps) {
  const t = useT();
  const mutation = useDeleteNetwork(envId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = network?.name ?? "";

  function wording(err: NetworkRequestError): string {
    switch (err.type) {
      case "network_in_use":
        return t("networks.delete.inUse", { name });
      case "network_builtin":
        return t("networks.delete.builtin", { name });
      case "host_read_only":
        return t("networks.errors.hostReadOnly");
      case "node_unavailable":
      case "node_unreachable":
        return t("networks.errors.nodeUnreachable");
      default:
        return err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
    }
  }

  async function remove() {
    if (!network || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({ id: network.id, nodeId: network.nodeId });
      toastSuccess(t("networks.delete.done", { name }));
      onClose();
    } catch (e) {
      const err = e instanceof NetworkRequestError ? e : new NetworkRequestError(0);
      if (err.type === "network_not_found") {
        toastInfo(t("networks.delete.gone", { name }));
        onClose();
      } else {
        setError(wording(err));
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
    <AlertDialog open={network !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("networks.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("networks.delete.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("networks.delete.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={mutation.isPending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("networks.delete.pending")}
              </>
            ) : (
              t("networks.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
