// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useEffect, useRef, useState } from "react";

import { NetworkRequestError, useDisconnectContainer } from "@/api/networks";
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
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// What a disconnect is aimed at: a container on its node and the network it leaves.
export interface DisconnectDialogTarget {
  networkId: string;
  networkName: string;
  containerId: string;
  containerName: string;
  nodeId: string;
  // server or agent for one of Coaming's own containers, when the page knows it. The
  // server decides in the end: a container the page did not know to be its own is asked
  // for by name when the server answers confirmation_required.
  selfKind?: string;
}

interface DisconnectDialogProps {
  envId: string;
  // The attachment to remove, or null while the dialog is closed.
  target: DisconnectDialogTarget | null;
  onClose: () => void;
}

// DisconnectDialog is "Disconnect {container} from {network}?" (D-43): an alert dialog
// with a primary confirm, since a disconnect can be undone by connecting again. Focus
// starts on Cancel, the dialog cannot be dismissed while the request is out, and a
// refusal stays inside it with the way forward. A container that is no longer on the
// network, or one of them that is gone, is said so in a toast and the dialog closes,
// since the refreshed pages no longer show the attachment.
export function DisconnectDialog({ envId, target, onClose }: DisconnectDialogProps) {
  const t = useT();
  const mutation = useDisconnectContainer(envId);
  const [error, setError] = useState<string | null>(null);
  // Coaming's own server and agent containers are disconnected only with their name typed.
  const [typed, setTyped] = useState("");
  const [asked, setAsked] = useState(false);
  useEffect(() => {
    if (target) return;
    setTyped("");
    setAsked(false);
  }, [target]);
  const needName = Boolean(target?.selfKind) || asked;
  const nameMissing = needName && !matchesName(typed, target?.containerName ?? "");
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const names = {
    container: target?.containerName ?? "",
    network: target?.networkName ?? "",
  };

  function wording(err: NetworkRequestError): string {
    switch (err.type) {
      case "host_read_only":
        return t("networks.errors.hostReadOnly");
      case "node_unavailable":
      case "node_unreachable":
        return t("networks.errors.nodeUnreachable");
      default:
        return err.status === 403 ? t("networks.errors.permission") : t("networks.errors.generic");
    }
  }

  async function disconnect() {
    if (!target || inFlight.current || nameMissing) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({
        networkId: target.networkId,
        containerId: target.containerId,
        nodeId: target.nodeId,
        ...(needName ? { confirm: typed.trim() } : {}),
      });
      toastSuccess(t("networks.disconnect.done", names));
      onClose();
    } catch (e) {
      const err = e instanceof NetworkRequestError ? e : new NetworkRequestError(0);
      if (err.type === "not_connected") {
        toastInfo(t("networks.disconnect.notConnected", names));
        onClose();
      } else if (err.type === "network_not_found" || err.type === "container_not_found") {
        toastInfo(t("networks.disconnect.gone", names));
        onClose();
      } else if (err.type === "confirmation_required") {
        setAsked(true);
        setError(t("networks.disconnect.ownAsked"));
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
    <AlertDialog open={target !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("networks.disconnect.title", names)}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("networks.disconnect.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {needName && target ? (
          <>
            <Alert role="note">
              <AlertDescription>{t("networks.disconnect.own")}</AlertDescription>
            </Alert>
            <TypeToConfirm
              name={target.containerName}
              value={typed}
              onChange={setTyped}
              disabled={mutation.isPending}
            />
          </>
        ) : null}
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("networks.disconnect.cancel")}
          </AlertDialogCancel>
          {/* Reversible, so the primary confirm and not the destructive one. */}
          <Button
            type="button"
            aria-disabled={mutation.isPending || nameMissing}
            className={`${TOUCH_HEIGHT} aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void disconnect()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("networks.disconnect.pending")}
              </>
            ) : (
              t("networks.disconnect.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
