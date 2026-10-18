// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { SecretRequestError, useDeleteSecret } from "@/api/secrets";
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

type Translate = (key: string, options?: Record<string, unknown>) => string;

// secretDeleteReason is why Delete… is unavailable on a secret, or undefined when it is
// available: who may comes first, then whether any service spec still mentions it (a service
// scaled to 0 counts). On the secret's own page the words do not send the reader to the page they
// are on.
export function secretDeleteReason(
  t: Translate,
  needsReason: string | undefined,
  usedBy: number,
  onOwnPage = false,
): string | undefined {
  if (needsReason) return needsReason;
  if (usedBy <= 0) return undefined;
  return onOwnPage
    ? t("secrets.delete.reason", { count: usedBy })
    : t("secrets.list.inUseBy", { count: usedBy });
}

interface DeleteSecretDialogProps {
  envId: string;
  // The secret to delete, or null while the dialog is closed.
  secret: { id: string; name: string } | null;
  onClose: () => void;
  // Called once the secret is gone, after the dialog asked to close.
  onDeleted?: () => void;
}

// DeleteSecretDialog is "Delete {name}?" (UI-SPEC "Destructive and dangerous actions"): the
// secret's name is typed to confirm, and the server checks it again, and checks live that no
// service spec mentions the secret. Focus starts on Cancel, the dialog cannot be dismissed while
// the request is out, and a refusal stays inside it with the way forward. A secret that is already
// gone is said so in a toast and the dialog closes, since the refreshed list no longer shows it.
export function DeleteSecretDialog({ envId, secret, onClose, onDeleted }: DeleteSecretDialogProps) {
  const t = useT();
  const mutation = useDeleteSecret(envId);
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const name = secret?.name ?? "";
  const confirmed = matchesName(typed, name);

  function wording(err: SecretRequestError): string {
    switch (err.type) {
      case "object_in_use":
        return t("secrets.delete.inUse", { name });
      case "host_read_only":
        return t("secrets.errors.hostReadOnly");
      case "no_manager_connected":
        return t("secrets.errors.noManager");
      case "not_a_swarm":
        return t("secrets.errors.notASwarm");
      default:
        return err.status === 403 ? t("secrets.errors.permission") : t("secrets.errors.generic");
    }
  }

  function close() {
    setTyped("");
    setError(null);
    onClose();
  }

  async function remove() {
    if (!secret || !confirmed || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({ id: secret.id, name: secret.name });
      toastSuccess(t("secrets.delete.done", { name }));
      close();
      onDeleted?.();
    } catch (e) {
      const err = e instanceof SecretRequestError ? e : new SecretRequestError(0);
      if (err.type === "secret_not_found") {
        toastInfo(t("secrets.delete.gone", { name }));
        close();
      } else {
        setError(wording(err));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || mutation.isPending) return;
    close();
  }

  return (
    <AlertDialog open={secret !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("secrets.delete.title", { name })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("secrets.delete.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        <TypeToConfirm
          name={name}
          value={typed}
          onChange={setTyped}
          onConfirm={() => void remove()}
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
            {t("secrets.delete.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={!confirmed || mutation.isPending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void remove()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("secrets.delete.pending")}
              </>
            ) : (
              t("secrets.delete.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
